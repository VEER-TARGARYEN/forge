package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/VEER-TARGARYEN/forge/internal/agent"
	"github.com/VEER-TARGARYEN/forge/internal/checkpoint"
	"github.com/VEER-TARGARYEN/forge/internal/embed"
	"github.com/VEER-TARGARYEN/forge/internal/gui"
	"github.com/VEER-TARGARYEN/forge/internal/index"
	"github.com/VEER-TARGARYEN/forge/internal/repomap"
	"github.com/VEER-TARGARYEN/forge/internal/router"
	"github.com/VEER-TARGARYEN/forge/internal/tools"
	"github.com/VEER-TARGARYEN/forge/internal/verify"
)

// guiBackend adapts the command layer's wiring to what the browser needs.
//
// It is the only place that knows both halves: how `forge do` assembles an
// agent, and what the HTTP layer asks for. Per-workspace state — the search
// index, the detected checks — is cached because building an index on every
// keystroke would make search unusable, and detection walks the tree.
type guiBackend struct {
	e        *env
	defDir   string
	embedder *embed.Embedder
	opts     guiOptions

	mu       sync.Mutex
	indexes  map[string]*lazyIndex
	verifies map[string]*verifier
}

type guiOptions struct {
	maxBytes     int
	repoMapTok   int
	embedClass   string
	verifyCmd    string
	vTimeout     time.Duration
	maxSpawns    int
	maxParallel  int
	noSubAgents  bool
	noNotes      bool
	maxToolBytes int
}

func newGUIBackend(e *env, dir string, em *embed.Embedder, o guiOptions) *guiBackend {
	return &guiBackend{
		e: e, defDir: dir, embedder: em, opts: o,
		indexes:  map[string]*lazyIndex{},
		verifies: map[string]*verifier{},
	}
}

// resolveDir turns a browser-supplied directory into an absolute path,
// defaulting to the one the server was started in.
func (g *guiBackend) resolveDir(dir string) string {
	if strings.TrimSpace(dir) == "" {
		dir = g.defDir
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return g.defDir
	}
	return abs
}

func (g *guiBackend) indexFor(dir string) *lazyIndex {
	g.mu.Lock()
	defer g.mu.Unlock()
	if li, ok := g.indexes[dir]; ok {
		return li
	}
	li := newLazyIndex(dir, index.Dir(g.e.cfg.Dir(), dir), g.e.rt, g.opts.embedClass, index.Options{})
	if g.embedder != nil {
		li.useLocal(g.embedder)
	}
	// The index prints build progress; in a server that belongs in the log,
	// not interleaved with whatever else is on stderr.
	li.out = io.Discard
	g.indexes[dir] = li
	return li
}

func (g *guiBackend) verifierFor(dir string) *verifier {
	g.mu.Lock()
	defer g.mu.Unlock()
	if v, ok := g.verifies[dir]; ok {
		return v
	}
	v := newVerifier(dir, g.opts.verifyCmd, g.opts.vTimeout)
	v.out = io.Discard
	g.verifies[dir] = v
	return v
}

// ---------- Run ----------

func (g *guiBackend) Run(ctx context.Context, req gui.RunRequest, obs agent.Observer, ap tools.Approver) (*agent.Outcome, error) {
	dir := g.resolveDir(req.Dir)
	ws, err := tools.NewWorkspace(dir)
	if err != nil {
		return nil, err
	}
	proto, err := agent.ParseProtocol(req.Protocol)
	if err != nil {
		return nil, err
	}
	class := req.Class
	if class == "" {
		class = g.e.cfg.Server.DefaultClass
	}

	li := g.indexFor(ws.Root())
	v := g.verifierFor(ws.Root())
	journal := checkpoint.New(ws.Root())

	notesFile := ""
	if !g.opts.noNotes {
		notesFile = tools.NotesPath(g.e.cfg.Dir(), ws.Root())
	}

	reg := defaultTools(!g.opts.noSubAgents)
	env := &tools.Env{
		WS:         ws,
		Approver:   ap,
		Out:        io.Discard, // the browser renders from events, not text
		MaxBytes:   g.opts.maxToolBytes,
		Todos:      tools.NewTodoList(),
		Overflow:   tools.NewOverflow(filepath.Join(g.e.cfg.Dir(), "overflow")),
		NotesFile:  notesFile,
		SearchCode: li.search,
		Snapshot:   journal.Record,
	}
	if v.enabled() {
		env.Verify = v.forTool
	}

	repoMap := ""
	if g.opts.repoMapTok > 0 {
		if m, err := repomap.Build(ws.Root(), repomap.Options{CacheDir: g.e.cfg.Dir()}); err == nil {
			repoMap = m.Render(g.opts.repoMapTok)
		}
	}

	var spawner *agent.Spawner
	if !g.opts.noSubAgents {
		spawner = agent.NewSpawner(g.e.rt, env, reg, agent.SpawnerConfig{
			MaxSpawns:     g.opts.maxSpawns,
			MaxConcurrent: g.opts.maxParallel,
			RepoMap:       repoMap,
			ParentClass:   class,
		}, io.Discard)
		env.Spawn = spawner.Spawn
	}

	ag := agent.New(g.e.rt, reg, env, agent.Config{
		Class:      class,
		MaxSteps:   req.MaxSteps,
		Protocol:   proto,
		RepoMap:    repoMap,
		Notes:      tools.LoadNotes(notesFile, 4000),
		Verify:     verifyHook(v, req.NoVerify),
		MaxRepairs: 3,
		SubStats:   subStats(spawner),
		Observer:   obs,
	}, io.Discard)

	out, runErr := ag.Run(ctx, req.Task)

	// Originals are always kept. A GUI makes it far easier to fire off a run
	// by accident than a shell does, so the undo path must never be optional.
	if journal.Len() > 0 {
		snapDir := filepath.Join(g.e.cfg.Dir(), "undo", time.Now().Format("20060102-150405"))
		_ = journal.Save(snapDir)
	}
	return out, runErr
}

// ---------- read-only surfaces ----------

func (g *guiBackend) Search(ctx context.Context, dir, query string, hybrid bool, limit int) ([]gui.SearchHit, error) {
	mode := "keyword"
	if hybrid {
		mode = "hybrid"
	}
	if limit <= 0 || limit > 50 {
		limit = 12
	}
	hits, _, err := g.indexFor(g.resolveDir(dir)).search(ctx, query, limit, mode)
	if err != nil {
		return nil, err
	}
	out := make([]gui.SearchHit, 0, len(hits))
	for _, h := range hits {
		out = append(out, gui.SearchHit{
			Path: h.File, Start: h.Start, End: h.End,
			Symbol: h.Symbol, Score: h.Score, Text: h.Text,
		})
	}
	return out, nil
}

func (g *guiBackend) RepoMap(dir string, tokens int) (gui.RepoMapView, error) {
	root := g.resolveDir(dir)
	if tokens <= 0 || tokens > 20000 {
		tokens = 2048
	}
	start := time.Now()
	m, err := repomap.Build(root, repomap.Options{CacheDir: g.e.cfg.Dir()})
	if err != nil {
		return gui.RepoMapView{}, err
	}
	return gui.RepoMapView{
		Files: m.Scanned,
		Text:  m.Render(tokens),
		MS:    time.Since(start).Milliseconds(),
	}, nil
}

func (g *guiBackend) ReadFile(dir, rel string) (gui.FileView, error) {
	ws, err := tools.NewWorkspace(g.resolveDir(dir))
	if err != nil {
		return gui.FileView{}, err
	}
	// Resolve through the workspace so the sandbox applies here too: an HTTP
	// endpoint that reads arbitrary paths would be a hole straight past it.
	abs, err := ws.Resolve(rel)
	if err != nil {
		return gui.FileView{}, err
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return gui.FileView{}, err
	}
	const maxFile = 512 << 10
	truncated := false
	if len(b) > maxFile {
		b, truncated = b[:maxFile], true
	}
	text := string(b)
	if truncated {
		text += "\n\n… truncated at 512 KB"
	}
	return gui.FileView{
		Path: rel, Text: text, Bytes: len(b),
		Lines: strings.Count(text, "\n") + 1,
	}, nil
}

func (g *guiBackend) Verify(ctx context.Context, dir string) (gui.VerifyView, error) {
	root := g.resolveDir(dir)
	v := g.verifierFor(root)
	if !v.enabled() {
		return gui.VerifyView{}, fmt.Errorf("no verification commands detected for %s", root)
	}
	rep := verify.Run(ctx, root, v.checks, v.opts)
	view := gui.VerifyView{
		Passed:   rep.Passed,
		Summary:  rep.Short(),
		Problems: len(rep.Failures()),
	}
	for _, c := range rep.Results {
		out := c.Output
		if len(out) > 8000 {
			out = out[:8000] + "\n… truncated"
		}
		if c.Passed {
			out = "" // only failures need their output on screen
		}
		view.Checks = append(view.Checks, gui.CheckView{
			Command: c.Check.Command, Passed: c.Passed,
			Output: out, MS: c.Duration.Milliseconds(),
		})
	}
	return view, nil
}

func (g *guiBackend) Providers(ctx context.Context, probe bool) []gui.ProviderView {
	snap := g.e.health.Snapshot()
	now := time.Now()

	// Worst remaining cooldown across every target a provider serves. One
	// rate-limited model is not the same as the provider being down, but it is
	// the thing you want to see when a run stalls.
	cooldown := map[string]time.Duration{}
	lastErr := map[string]string{}
	for key, ent := range snap {
		prov, _, ok := strings.Cut(key, "|")
		if !ok || ent.CooldownUntil.IsZero() || !now.Before(ent.CooldownUntil) {
			continue
		}
		if d := ent.CooldownUntil.Sub(now); d > cooldown[prov] {
			cooldown[prov] = d
			lastErr[prov] = ent.LastErrKind
		}
	}

	names := g.e.reg.Names()
	out := make([]gui.ProviderView, 0, len(names))
	byName := map[string]*gui.ProviderView{}

	for _, name := range names {
		p, _ := g.e.reg.Get(name)
		v := gui.ProviderView{
			Name:       name,
			Enabled:    true,
			Configured: p.Configured(),
			Cooldown:   int64(cooldown[name].Seconds()),
			LastErr:    lastErr[name],
		}
		for _, pc := range g.e.cfg.Providers {
			if pc.Name == name {
				v.BaseURL, v.Note = pc.BaseURL, pc.Note
			}
		}
		out = append(out, v)
		byName[name] = &out[len(out)-1]
	}

	// Providers turned off in the config never reach the registry, so they are
	// added here — "disabled" is a different answer from "missing", and only
	// showing the ones that loaded makes a disabled provider look like a typo.
	for _, pc := range g.e.cfg.Providers {
		if _, ok := byName[pc.Name]; ok {
			continue
		}
		out = append(out, gui.ProviderView{
			Name: pc.Name, Enabled: false, BaseURL: pc.BaseURL, Note: pc.Note,
		})
	}

	if !probe {
		return out
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	for i := range out {
		v := &out[i]
		if !v.Configured || !v.Enabled {
			continue
		}
		p, ok := g.e.reg.Get(v.Name)
		if !ok {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, 12*time.Second)
			defer cancel()
			start := time.Now()
			models, err := p.ListModels(pctx)
			d := time.Since(start)
			mu.Lock()
			defer mu.Unlock()
			v.MS = d.Milliseconds()
			if err != nil {
				v.Detail = firstLine(err.Error())
				return
			}
			v.OK, v.Models = true, len(models)
		}()
	}
	wg.Wait()
	return out
}

func (g *guiBackend) ResetHealth() {
	g.e.health.Clear()
	_ = g.e.health.Flush()
}

func (g *guiBackend) Bootstrap() gui.BootstrapView {
	snap := g.e.health.Snapshot()
	now := time.Now()

	classes := map[string][]gui.TargetView{}
	for _, name := range g.e.cfg.ClassNames() {
		for _, t := range g.e.cfg.Classes[name] {
			tv := gui.TargetView{
				Provider: t.Provider, Model: t.Model,
				MaxContext: t.MaxContext, Note: t.Note,
			}
			if ent, ok := snap[t.Key()]; ok && !ent.CooldownUntil.IsZero() && now.Before(ent.CooldownUntil) {
				tv.CooldownSec = int64(ent.CooldownUntil.Sub(now).Seconds())
				tv.LastErrKind = ent.LastErrKind
			}
			classes[name] = append(classes[name], tv)
		}
	}

	var checks []string
	if v := g.verifierFor(g.defDir); v.enabled() {
		for _, c := range v.checks {
			checks = append(checks, c.Command)
		}
	}

	embedder := ""
	if g.embedder != nil {
		embedder = g.embedder.Describe()
	}

	return gui.BootstrapView{
		Version:    version,
		ConfigPath: g.e.cfg.Path(),
		StateDir:   g.e.cfg.Dir(),
		Workspace:  g.defDir,
		Classes:    classes,
		ClassNames: g.e.cfg.ClassNames(),
		Default:    g.e.cfg.Server.DefaultClass,
		Providers:  g.Providers(context.Background(), false),
		Verify:     checks,
		Embedder:   embedder,
	}
}

func (g *guiBackend) Usage() gui.UsageView {
	path := g.e.ledger.Path()
	view := gui.UsageView{ByModel: []gui.UsageRow{}, ByDay: []gui.UsageDayRow{}, Recent: []gui.UsageCallRow{}}

	byTarget, err := router.Summarize(path, time.Time{}, func(r router.Record) string {
		return r.Provider + "\x00" + r.Model
	})
	if err == nil {
		for _, s := range byTarget {
			prov, model, _ := strings.Cut(s.Key, "\x00")
			view.ByModel = append(view.ByModel, gui.UsageRow{
				Provider: prov, Model: model, Calls: s.Calls,
				Prompt: s.PromptTok, Completion: s.OutTok,
			})
			view.TotalPrompt += s.PromptTok
			view.TotalCompletion += s.OutTok
		}
		sort.Slice(view.ByModel, func(i, j int) bool {
			a, b := view.ByModel[i], view.ByModel[j]
			return a.Prompt+a.Completion > b.Prompt+b.Completion
		})
	}

	byDay, err := router.Summarize(path, time.Now().AddDate(0, 0, -13), func(r router.Record) string {
		return r.TS.Format("2006-01-02")
	})
	if err == nil {
		for _, s := range byDay {
			view.ByDay = append(view.ByDay, gui.UsageDayRow{Day: s.Key, Tokens: s.PromptTok + s.OutTok})
		}
		sort.Slice(view.ByDay, func(i, j int) bool { return view.ByDay[i].Day < view.ByDay[j].Day })
	}

	view.Recent = recentCalls(path, 25)
	return view
}

// recentCalls tails the ledger. The file is append-only JSON lines, so the
// last N decodable lines are the last N calls.
func recentCalls(path string, n int) []gui.UsageCallRow {
	f, err := os.Open(path)
	if err != nil {
		return []gui.UsageCallRow{}
	}
	defer f.Close()

	ring := make([]gui.UsageCallRow, 0, n)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var r router.Record
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		row := gui.UsageCallRow{
			T: r.TS.UnixMilli(), Provider: r.Provider, Model: r.Model,
			Prompt: r.PromptTok, Completion: r.OutTok, MS: r.LatencyMS, Err: r.ErrKind,
		}
		if len(ring) == n {
			ring = ring[1:]
		}
		ring = append(ring, row)
	}
	// Newest first, which is how the panel reads.
	for i, j := 0, len(ring)-1; i < j; i, j = i+1, j-1 {
		ring[i], ring[j] = ring[j], ring[i]
	}
	return ring
}

// ---------- command ----------

func cmdGUI(args []string) error {
	fs := flag.NewFlagSet("gui", flag.ExitOnError)
	cfgPath := addConfigFlag(fs)
	dir := fs.String("dir", ".", "workspace root the agent may act in")
	addr := fs.String("addr", "127.0.0.1:4100", "address to serve the interface on")
	open_ := fs.Bool("open", true, "open the interface in your browser")
	embedModel := fs.String("embed-model", "", "path to a local embedding model directory")
	embedClass := fs.String("embed-class", "embed", "routing class used for semantic search")
	repoMapTokens := fs.Int("repo-map", 1024, "token budget for the repository map (0 disables it)")
	maxBytes := fs.Int("max-tool-bytes", 30000, "cap on how much any single tool result may add to context")
	verifyCmd := fs.String("verify-cmd", "", "run this instead of the auto-detected checks")
	verifyTimeout := fs.Duration("verify-timeout", 5*time.Minute, "per-check timeout")
	maxSpawns := fs.Int("max-subagents", 8, "total sub-agent delegations allowed per run")
	maxParallel := fs.Int("parallel-subagents", 3, "how many delegations may run at once")
	noSubAgents := fs.Bool("no-subagents", false, "disable delegation entirely")
	noNotes := fs.Bool("no-notes", false, "do not load or write cross-session notes")
	if err := fs.Parse(args); err != nil {
		return err
	}

	e, err := open(*cfgPath, false)
	if err != nil {
		return err
	}
	defer e.Close()

	root, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	if _, err := tools.NewWorkspace(root); err != nil {
		return err
	}

	em, err := resolveEmbedder(*embedModel, e.cfg.Dir(), nil)
	if err != nil {
		return err
	}

	be := newGUIBackend(e, root, em, guiOptions{
		repoMapTok:   *repoMapTokens,
		embedClass:   *embedClass,
		verifyCmd:    *verifyCmd,
		vTimeout:     *verifyTimeout,
		maxSpawns:    *maxSpawns,
		maxParallel:  *maxParallel,
		noSubAgents:  *noSubAgents,
		noNotes:      *noNotes,
		maxToolBytes: *maxBytes,
	})

	srv := gui.NewServer(be, *addr)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	return srv.Run(ctx, func(url string) {
		fmt.Fprintf(os.Stderr, "forge gui\n")
		fmt.Fprintf(os.Stderr, "  workspace: %s\n", root)
		fmt.Fprintf(os.Stderr, "  config:    %s\n", e.cfg.Path())
		if em != nil {
			fmt.Fprintf(os.Stderr, "  embedder:  %s\n", em.Describe())
		}
		fmt.Fprintf(os.Stderr, "\n  → %s\n\n", url)
		fmt.Fprintf(os.Stderr, "  ctrl-c to stop\n")
		if *open_ {
			openBrowser(url)
		}
	})
}

// openBrowser is best effort. Failing to launch one is not an error worth
// stopping the server for — the URL is printed either way.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
