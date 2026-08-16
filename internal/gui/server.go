package gui

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

//go:embed web
var webFS embed.FS

// View types returned by the read-only endpoints. They live here rather than
// in the backend so the wire shape is defined next to the handlers that emit
// it, and so the command layer depends on this package rather than the reverse.

type SearchHit struct {
	Path   string  `json:"path"`
	Start  int     `json:"start"`
	End    int     `json:"end"`
	Symbol string  `json:"symbol,omitempty"`
	Score  float64 `json:"score"`
	Text   string  `json:"text"`
}

type RepoMapView struct {
	Files int    `json:"files"`
	Text  string `json:"text"`
	MS    int64  `json:"ms"`
}

type FileView struct {
	Path  string `json:"path"`
	Text  string `json:"text"`
	Lines int    `json:"lines"`
	Bytes int    `json:"bytes"`
}

type VerifyView struct {
	Passed   bool        `json:"passed"`
	Summary  string      `json:"summary"`
	Checks   []CheckView `json:"checks"`
	Problems int         `json:"problems"`
}

type CheckView struct {
	Command string `json:"command"`
	Passed  bool   `json:"passed"`
	Output  string `json:"output,omitempty"`
	MS      int64  `json:"ms"`
}

type ProviderView struct {
	Name       string `json:"name"`
	Configured bool   `json:"configured"`
	Enabled    bool   `json:"enabled"`
	BaseURL    string `json:"baseUrl"`
	Note       string `json:"note,omitempty"`
	OK         bool   `json:"ok"`
	Detail     string `json:"detail,omitempty"`
	Models     int    `json:"models,omitempty"`
	MS         int64  `json:"ms,omitempty"`
	Cooldown   int64  `json:"cooldownSec,omitempty"`
	LastErr    string `json:"lastError,omitempty"`
}

type TargetView struct {
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	MaxContext  int    `json:"maxContext"`
	Note        string `json:"note,omitempty"`
	CooldownSec int64  `json:"cooldownSec,omitempty"`
	LastErrKind string `json:"lastErrorKind,omitempty"`
}

type UsageView struct {
	TotalPrompt     int            `json:"totalPrompt"`
	TotalCompletion int            `json:"totalCompletion"`
	ByModel         []UsageRow     `json:"byModel"`
	ByDay           []UsageDayRow  `json:"byDay"`
	Recent          []UsageCallRow `json:"recent"`
}

type UsageRow struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Calls      int    `json:"calls"`
	Prompt     int    `json:"prompt"`
	Completion int    `json:"completion"`
}

type UsageDayRow struct {
	Day    string `json:"day"`
	Tokens int    `json:"tokens"`
}

type UsageCallRow struct {
	T          int64  `json:"t"`
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Prompt     int    `json:"prompt"`
	Completion int    `json:"completion"`
	MS         int64  `json:"ms"`
	Err        string `json:"error,omitempty"`
}

type BootstrapView struct {
	Version    string                  `json:"version"`
	ConfigPath string                  `json:"configPath"`
	StateDir   string                  `json:"stateDir"`
	Workspace  string                  `json:"workspace"`
	Classes    map[string][]TargetView `json:"classes"`
	ClassNames []string                `json:"classNames"`
	Default    string                  `json:"defaultClass"`
	Providers  []ProviderView          `json:"providers"`
	Verify     []string                `json:"verifyChecks"`
	Embedder   string                  `json:"embedder,omitempty"`
}

// Server wires the API and the embedded front end onto one mux.
type Server struct {
	be   Backend
	mgr  *Manager
	addr string
}

func NewServer(be Backend, addr string) *Server {
	return &Server{be: be, mgr: NewManager(be), addr: addr}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/bootstrap", s.handleBootstrap)
	mux.HandleFunc("POST /api/workspace", s.handleSetWorkspace)
	mux.HandleFunc("GET /api/sessions", s.handleListSessions)
	mux.HandleFunc("POST /api/sessions", s.handleCreateSession)
	mux.HandleFunc("GET /api/sessions/{id}", s.handleGetSession)
	mux.HandleFunc("GET /api/sessions/{id}/events", s.handleEvents)
	mux.HandleFunc("GET /api/sessions/{id}/stream", s.handleStream)
	mux.HandleFunc("POST /api/sessions/{id}/approve", s.handleApprove)
	mux.HandleFunc("POST /api/sessions/{id}/cancel", s.handleCancel)

	mux.HandleFunc("GET /api/search", s.handleSearch)
	mux.HandleFunc("GET /api/map", s.handleMap)
	mux.HandleFunc("GET /api/file", s.handleFile)
	mux.HandleFunc("POST /api/verify", s.handleVerify)
	mux.HandleFunc("GET /api/providers", s.handleProviders)
	mux.HandleFunc("POST /api/providers/reset", s.handleResetHealth)
	mux.HandleFunc("GET /api/usage", s.handleUsage)

	mux.HandleFunc("GET /icon-192.png", s.icon(192, false))
	mux.HandleFunc("GET /icon-512.png", s.icon(512, false))
	mux.HandleFunc("GET /icon-maskable-512.png", s.icon(512, true))
	mux.HandleFunc("GET /favicon.ico", s.favicon)

	mux.Handle("/", s.static())
	return noStoreAPI(mux)
}

// static serves the embedded front end, falling back to index.html so client
// side routes survive a reload.
func (s *Server) static() http.Handler {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "front end not embedded: "+err.Error(), http.StatusInternalServerError)
		})
	}
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := strings.TrimPrefix(r.URL.Path, "/")
		if clean == "" {
			clean = "index.html"
		}
		if _, err := fs.Stat(sub, clean); err != nil {
			// Unknown path: hand back the shell and let the router decide.
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}
		// The assets are compiled into the binary, so they change exactly when
		// the binary does — and then the cached copy is wrong. Revalidating is
		// free over loopback and beats shipping an upgrade the browser quietly
		// ignores. ETag still makes the common case a 304.
		w.Header().Set("Cache-Control", "no-cache")
		// Go's mime table has no entry for .webmanifest, so it sniffs the file
		// as text/plain — and a manifest served as text/plain is ignored,
		// which silently costs installability.
		if strings.HasSuffix(clean, ".webmanifest") {
			w.Header().Set("Content-Type", "application/manifest+json")
		}
		files.ServeHTTP(w, r)
	})
}

// icon serves a rasterised mark. These are drawn, not stored, so they are the
// one thing here worth letting the browser keep for a while.
func (s *Server) icon(size int, maskable bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := IconPNG(size, maskable)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(b)
	}
}

func (s *Server) favicon(w http.ResponseWriter, r *http.Request) {
	b, err := IconICO(16, 32, 48)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/x-icon")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(b)
}

// noStoreAPI keeps API responses out of the browser cache. A cached
// /api/sessions is worse than a slow one: it shows a run that has already
// moved on and looks like the server is stuck.
func noStoreAPI(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		h.ServeHTTP(w, r)
	})
}

func (s *Server) Run(ctx context.Context, onReady func(url string)) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.addr, err)
	}
	srv := &http.Server{
		Handler: s.Handler(),
		// No write timeout: SSE streams stay open for the length of a run,
		// and a run can legitimately take an hour on a local model.
		ReadHeaderTimeout: 10 * time.Second,
	}
	if onReady != nil {
		onReady("http://" + ln.Addr().String())
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		s.mgr.CancelAll()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		return nil
	}
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) (*Session, bool) {
	sess, ok := s.mgr.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "no such session")
		return nil, false
	}
	return sess, true
}

func intParam(r *http.Request, name string, def int) int {
	if v := r.URL.Query().Get(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// ---------- handlers ----------

func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.be.Bootstrap())
}

func (s *Server) handleSetWorkspace(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Dir string `json:"dir"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request body")
		return
	}
	if strings.TrimSpace(body.Dir) == "" {
		writeErr(w, http.StatusBadRequest, "dir is required")
		return
	}
	view, err := s.be.SetWorkspace(body.Dir)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.mgr.List())
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req RunRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Task) == "" {
		writeErr(w, http.StatusBadRequest, "task is required")
		return
	}
	sess, err := s.mgr.Start(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sess.View())
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, sess.View())
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	// Always a list. A nil slice marshals to null, which turns "no events
	// since that point" — the normal answer when a client is caught up — into
	// a type error for anyone reading .length.
	evs := sess.log.since(intParam(r, "from", 0))
	if evs == nil {
		evs = []Event{}
	}
	writeJSON(w, http.StatusOK, evs)
}

// handleStream is the live view: replay everything after ?from, then stream.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	from := intParam(r, "from", 0)
	if id := r.Header.Get("Last-Event-ID"); id != "" {
		if n, err := strconv.Atoi(id); err == nil {
			from = n
		}
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	// Without this an intervening proxy may buffer the whole run and deliver
	// it at the end, which looks exactly like a hang.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	backlog, ch := sess.log.subscribe(from)
	defer sess.log.unsubscribe(ch)

	send := func(ev Event) {
		fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Kind, ev.encode())
	}
	for _, ev := range backlog {
		send(ev)
	}
	flusher.Flush()

	// A heartbeat keeps intermediaries from reaping an idle connection while
	// a local model spends two minutes on prefill without emitting a token.
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case ev, open := <-ch:
			if !open {
				fmt.Fprintf(w, "event: close\ndata: {}\n\n")
				flusher.Flush()
				return
			}
			send(ev)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var body struct {
		ID       string `json:"id"`
		Decision string `json:"decision"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request body")
		return
	}
	switch body.Decision {
	case "approve", "always", "deny", "abort":
	default:
		writeErr(w, http.StatusBadRequest, "decision must be approve, always, deny, or abort")
		return
	}
	if !sess.Resolve(body.ID, body.Decision) {
		writeErr(w, http.StatusNotFound, "that approval is no longer pending")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	sess.Cancel()
	writeJSON(w, http.StatusOK, sess.View())
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if strings.TrimSpace(q) == "" {
		writeErr(w, http.StatusBadRequest, "q is required")
		return
	}
	hybrid := r.URL.Query().Get("mode") != "keyword"
	hits, err := s.be.Search(r.Context(), r.URL.Query().Get("dir"), q, hybrid, intParam(r, "limit", 12))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if hits == nil {
		hits = []SearchHit{}
	}
	writeJSON(w, http.StatusOK, hits)
}

func (s *Server) handleMap(w http.ResponseWriter, r *http.Request) {
	v, err := s.be.RepoMap(r.URL.Query().Get("dir"), intParam(r, "tokens", 2048))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	rel := r.URL.Query().Get("path")
	if rel == "" {
		writeErr(w, http.StatusBadRequest, "path is required")
		return
	}
	v, err := s.be.ReadFile(r.URL.Query().Get("dir"), rel)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	v, err := s.be.Verify(r.Context(), r.URL.Query().Get("dir"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.be.Providers(r.Context(), r.URL.Query().Get("probe") == "1"))
}

func (s *Server) handleResetHealth(w http.ResponseWriter, r *http.Request) {
	s.be.ResetHealth()
	writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.be.Usage())
}
