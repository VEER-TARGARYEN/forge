// Package serve exposes the router as an OpenAI-compatible HTTP endpoint.
//
// This is what makes Phase 1 useful before any agent code exists: point Cline,
// Roo, Continue, Aider, or anything else that speaks the OpenAI API at
// http://127.0.0.1:4000/v1 and it inherits the whole free-tier fallback chain.
package serve

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/VEER-TARGARYEN/forge/internal/provider"
	"github.com/VEER-TARGARYEN/forge/internal/router"
)

type Server struct {
	r      *router.Router
	addr   string
	apiKey string
	defCls string
	logger *log.Logger
}

func New(r *router.Router, logger *log.Logger) *Server {
	cfg := r.Config()
	return &Server{
		r:      r,
		addr:   cfg.Server.Addr,
		apiKey: cfg.Server.APIKey,
		defCls: cfg.Server.DefaultClass,
		logger: logger,
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /v1/models", s.auth(s.handleModels))
	mux.HandleFunc("POST /v1/chat/completions", s.auth(s.handleChat))
	mux.HandleFunc("GET /v1/status", s.auth(s.handleStatus))
	return cors(mux)
}

func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:    s.addr,
		Handler: s.Handler(),
		// Generous: a local 7B on CPU can legitimately take many minutes.
		ReadHeaderTimeout: 30 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		s.logger.Printf("listening on http://%s/v1  (default class: %s)", s.addr, s.defCls)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	case <-ctx.Done():
		s.logger.Printf("shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	}
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.apiKey != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if strings.TrimSpace(got) != s.apiKey {
				writeErr(w, http.StatusUnauthorized, "invalid api key")
				return
			}
		}
		next(w, r)
	}
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- /v1/models ----------

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	cfg := s.r.Config()
	now := time.Now().Unix()
	out := struct {
		Object string  `json:"object"`
		Data   []model `json:"data"`
	}{Object: "list"}
	for _, c := range cfg.ClassNames() {
		out.Data = append(out.Data, model{ID: "forge-" + c, Object: "model", Created: now, OwnedBy: "forge"})
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------- /v1/status ----------

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	cfg := s.r.Config()
	snap := s.r.Health().Snapshot()
	now := time.Now()

	type targetStatus struct {
		Provider  string `json:"provider"`
		Model     string `json:"model"`
		Available bool   `json:"available"`
		Reason    string `json:"reason,omitempty"`
		CooldownS int    `json:"cooldown_s,omitempty"`
		Calls     int    `json:"calls,omitempty"`
	}
	out := map[string][]targetStatus{}
	for _, class := range cfg.ClassNames() {
		for _, t := range cfg.Classes[class] {
			ts := targetStatus{Provider: t.Provider, Model: t.Model, Available: true}
			if p, ok := s.r.Registry().Get(t.Provider); !ok {
				ts.Available, ts.Reason = false, "disabled"
			} else if !p.Configured() {
				ts.Available, ts.Reason = false, "no api key"
			}
			if e, ok := snap[t.Key()]; ok {
				ts.Calls = e.Calls
				if !e.CooldownUntil.IsZero() && now.Before(e.CooldownUntil) {
					ts.Available = false
					ts.Reason = e.LastErrKind
					ts.CooldownS = int(e.CooldownUntil.Sub(now).Seconds())
				}
			}
			out[class] = append(out[class], ts)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------- /v1/chat/completions ----------

type chatReq struct {
	Model       string             `json:"model"`
	Messages    []provider.Message `json:"messages"`
	Tools       []provider.Tool    `json:"tools,omitempty"`
	ToolChoice  any                `json:"tool_choice,omitempty"`
	Temperature *float64           `json:"temperature,omitempty"`
	TopP        *float64           `json:"top_p,omitempty"`
	MaxTokens   *int               `json:"max_tokens,omitempty"`
	MaxComp     *int               `json:"max_completion_tokens,omitempty"`
	Stop        []string           `json:"stop,omitempty"`
	Seed        *int               `json:"seed,omitempty"`
	Stream      bool               `json:"stream,omitempty"`
	RespFormat  *json.RawMessage   `json:"response_format,omitempty"`
}

// classOf maps the incoming model field onto a routing class. Anything
// unrecognised falls through to the default class rather than erroring, so a
// client configured with a stale model name still works.
func (s *Server) classOf(model string) string {
	m := strings.TrimSpace(model)
	switch {
	case m == "", strings.EqualFold(m, "forge"), strings.EqualFold(m, "auto"):
		return s.defCls
	}
	// Explicit pin: "provider:model" goes straight through to the router.
	if p, rest, ok := strings.Cut(m, ":"); ok && rest != "" {
		if _, exists := s.r.Config().ProviderByName(p); exists {
			return m
		}
	}
	name := strings.TrimPrefix(m, "forge-")
	if _, ok := s.r.Config().Classes[name]; ok {
		return name
	}
	return s.defCls
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var in chatReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if len(in.Messages) == 0 {
		writeErr(w, http.StatusBadRequest, "messages is required")
		return
	}

	class := s.classOf(in.Model)
	req := provider.Request{Messages: in.Messages, Tools: in.Tools, Stop: in.Stop, Seed: in.Seed}
	if in.Temperature != nil {
		req.Temperature = *in.Temperature
	}
	if in.TopP != nil {
		req.TopP = *in.TopP
	}
	switch {
	case in.MaxTokens != nil:
		req.MaxTokens = *in.MaxTokens
	case in.MaxComp != nil:
		req.MaxTokens = *in.MaxComp
	}
	if tc, ok := in.ToolChoice.(string); ok {
		req.ToolChoice = tc
	}
	if in.RespFormat != nil {
		var rf struct {
			Type       string `json:"type"`
			JSONSchema struct {
				Name   string         `json:"name"`
				Schema map[string]any `json:"schema"`
			} `json:"json_schema"`
		}
		if err := json.Unmarshal(*in.RespFormat, &rf); err == nil && rf.Type == "json_schema" {
			req.JSONSchema = rf.JSONSchema.Schema
			req.SchemaName = rf.JSONSchema.Name
		}
	}

	id := "chatcmpl-" + randID()
	created := time.Now().Unix()

	if !in.Stream {
		resp, err := s.r.Chat(r.Context(), class, req)
		if err != nil {
			s.logger.Printf("chat %s failed: %v", class, err)
			writeErr(w, statusFor(err), err.Error())
			return
		}
		s.logger.Printf("chat %s -> %s/%s  %d+%d tok  %s",
			class, resp.Provider, resp.Model, resp.Usage.PromptTokens, resp.Usage.CompletionTokens,
			resp.Latency.Round(time.Millisecond))
		writeJSON(w, http.StatusOK, buildFinal(id, created, resp))
		return
	}

	// Streaming path.
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	sentRole := false
	send := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	resp, err := s.r.Stream(r.Context(), class, req, func(c provider.Chunk) error {
		if !sentRole {
			sentRole = true
			if err := send(deltaFrame(id, created, class, map[string]any{"role": "assistant", "content": ""}, "")); err != nil {
				return err
			}
		}
		switch {
		case c.Content != "":
			return send(deltaFrame(id, created, class, map[string]any{"content": c.Content}, ""))
		case c.ToolCallDelta != nil:
			tc := map[string]any{
				"index":    c.ToolCallIndex,
				"type":     "function",
				"function": map[string]any{},
			}
			if c.ToolCallID != "" {
				tc["id"] = c.ToolCallID
			}
			fn := tc["function"].(map[string]any)
			if c.ToolCallDelta.Name != "" {
				fn["name"] = c.ToolCallDelta.Name
			}
			if c.ToolCallDelta.Arguments != "" {
				fn["arguments"] = c.ToolCallDelta.Arguments
			}
			return send(deltaFrame(id, created, class, map[string]any{"tool_calls": []any{tc}}, ""))
		}
		return nil
	})
	if err != nil {
		s.logger.Printf("stream %s failed: %v", class, err)
		if !sentRole {
			writeErr(w, statusFor(err), err.Error())
			return
		}
		// Already streaming: the only honest signal left is an error frame
		// followed by [DONE].
		_ = send(map[string]any{"error": map[string]any{"message": err.Error(), "type": "forge_route_error"}})
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	finish := resp.FinishReason
	if finish == "" {
		finish = "stop"
	}
	_ = send(deltaFrame(id, created, class, map[string]any{}, finish))
	_ = send(map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": created,
		"model": "forge-" + class, "choices": []any{},
		"usage": map[string]any{
			"prompt_tokens":     resp.Usage.PromptTokens,
			"completion_tokens": resp.Usage.CompletionTokens,
			"total_tokens":      resp.Usage.TotalTokens,
		},
	})
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()

	s.logger.Printf("stream %s -> %s/%s  %d+%d tok  ttft %s  %.1f tok/s",
		class, resp.Provider, resp.Model, resp.Usage.PromptTokens, resp.Usage.CompletionTokens,
		resp.TTFT.Round(time.Millisecond), resp.DecodeTPS())
}

func deltaFrame(id string, created int64, class string, delta map[string]any, finish string) map[string]any {
	choice := map[string]any{"index": 0, "delta": delta}
	if finish != "" {
		choice["finish_reason"] = finish
	} else {
		choice["finish_reason"] = nil
	}
	return map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": created,
		"model": "forge-" + class, "choices": []any{choice},
	}
}

func buildFinal(id string, created int64, resp *provider.Response) map[string]any {
	msg := map[string]any{"role": "assistant", "content": resp.Content}
	if len(resp.ToolCalls) > 0 {
		msg["tool_calls"] = resp.ToolCalls
	}
	finish := resp.FinishReason
	if finish == "" {
		finish = "stop"
	}
	return map[string]any{
		"id": id, "object": "chat.completion", "created": created,
		"model": resp.Model,
		"choices": []any{map[string]any{
			"index": 0, "message": msg, "finish_reason": finish,
		}},
		"usage": map[string]any{
			"prompt_tokens":     resp.Usage.PromptTokens,
			"completion_tokens": resp.Usage.CompletionTokens,
			"total_tokens":      resp.Usage.TotalTokens,
		},
		"x_forge": map[string]any{
			"provider":   resp.Provider,
			"latency_ms": resp.Latency.Milliseconds(),
			"ttft_ms":    resp.TTFT.Milliseconds(),
			"estimated":  resp.Usage.Estimated,
		},
	}
}

// statusFor maps a routing failure to an HTTP status the client will handle
// sensibly: 429 when everything is rate limited, 502 otherwise.
func statusFor(err error) int {
	switch provider.KindOf(err) {
	case provider.ErrRateLimit, provider.ErrQuota:
		return http.StatusTooManyRequests
	case provider.ErrAuth:
		return http.StatusUnauthorized
	case provider.ErrContextLength, provider.ErrBadRequest:
		return http.StatusBadRequest
	default:
		return http.StatusBadGateway
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": msg, "type": "forge_error", "code": status},
	})
}

func randID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
