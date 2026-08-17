// Command forge is the CLI entry point.
//
// Phase 0/1 scope: benchmark your hardware and your free tiers, then expose
// them as one OpenAI-compatible endpoint that fails over automatically.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/VEER-TARGARYEN/forge/internal/agent"
	"github.com/VEER-TARGARYEN/forge/internal/approval"
	"github.com/VEER-TARGARYEN/forge/internal/bench"
	"github.com/VEER-TARGARYEN/forge/internal/checkpoint"
	"github.com/VEER-TARGARYEN/forge/internal/config"
	"github.com/VEER-TARGARYEN/forge/internal/embed"
	"github.com/VEER-TARGARYEN/forge/internal/index"
	"github.com/VEER-TARGARYEN/forge/internal/provider"
	"github.com/VEER-TARGARYEN/forge/internal/repomap"
	"github.com/VEER-TARGARYEN/forge/internal/router"
	"github.com/VEER-TARGARYEN/forge/internal/selfcheck"
	"github.com/VEER-TARGARYEN/forge/internal/serve"
	"github.com/VEER-TARGARYEN/forge/internal/term"
	"github.com/VEER-TARGARYEN/forge/internal/tools"
)

// version is stamped by mkdist at release time with
// -ldflags "-X main.version=...". The default is what a plain `go build`
// produces, so a locally built binary never claims to be a release.
var version = "1.0.0-dev"

// defaultCommand is what to run when the binary is invoked with no arguments.
//
// Empty for the CLI build, where printing usage is the right answer. The
// desktop build sets it to "app" via
// -ldflags "-X main.defaultCommand=app -H=windowsgui", which is what lets one
// source tree produce both a terminal tool and a double-clickable application
// without a second main package or a duplicated dispatch table.
var defaultCommand = ""

func main() {
	log.SetFlags(0)
	argv := os.Args
	if len(argv) < 2 {
		if defaultCommand == "" {
			usage()
			os.Exit(2)
		}
		argv = append(argv, defaultCommand)
	}
	args := argv[2:]
	var err error
	switch argv[1] {
	case "init":
		err = cmdInit(args)
	case "doctor":
		err = cmdDoctor(args)
	case "models":
		err = cmdModels(args)
	case "bench":
		err = cmdBench(args)
	case "chat":
		err = cmdChat(args)
	case "do":
		err = cmdDo(args)
	case "map":
		err = cmdMap(args)
	case "index":
		err = cmdIndex(args)
	case "search":
		err = cmdSearch(args)
	case "verify":
		err = cmdVerify(args)
	case "embed":
		err = cmdEmbed(args)
	case "serve":
		err = cmdServe(args)
	case "gui":
		err = cmdGUI(args)
	case "app":
		err = cmdApp(args)
	case "usage":
		err = cmdUsage(args)
	case "selfcheck":
		err = cmdSelfcheck(args)
	case "version", "-v", "--version":
		fmt.Println("forge", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", argv[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: "+err.Error())
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `forge — a from-scratch coding agent stack

usage: forge <command> [flags]

  init      write a starter config with the free-tier providers wired up
  doctor    check which providers are configured, reachable, and healthy
  models    list the model ids a provider actually offers
  bench     measure prefill/decode throughput for local and hosted targets
  chat      one-shot prompt through a routing class
  do        run the coding agent on a task in a workspace
  map       print the ranked repository map the agent would see
  index     build or update the code search index
  search    query the code search index
  verify    run the project's build, lint, and test checks
  embed     run the built-in embedding model, or benchmark its kernels
  app       open FORGE as a desktop app in its own window
  gui       open the browser interface for running and watching sessions
  serve     expose the router as an OpenAI-compatible endpoint
  usage     summarize the token ledger
  selfcheck run the routing and streaming invariants against stub servers

run 'forge <command> -h' for flags
`)
}

// ---------- shared plumbing ----------

type env struct {
	cfg    *config.Config
	reg    *provider.Registry
	health *router.Health
	ledger *router.Ledger
	rt     *router.Router
}

func (e *env) Close() { _ = e.ledger.Close(); _ = e.health.Flush() }

func open(path string, verbose bool) (*env, error) {
	cfg, err := config.Load(path)
	if err != nil {
		if os.IsNotExist(err) || strings.Contains(err.Error(), "cannot find") {
			return nil, fmt.Errorf("%w\n\nrun 'forge init' to create one", err)
		}
		return nil, err
	}
	reg := provider.NewRegistry(cfg)
	health := router.NewHealth(cfg.Dir(), cfg.Policy)
	ledger, err := router.NewLedger(cfg.Dir())
	if err != nil {
		return nil, err
	}
	rt := router.New(cfg, reg, health, ledger)
	if verbose {
		rt.OnEvent = func(ev router.Event) {
			switch ev.Kind {
			case "skip":
				fmt.Fprintf(os.Stderr, "  · skip %s/%s (%s)\n", ev.Provider, ev.Model, ev.Detail)
			case "attempt":
				fmt.Fprintf(os.Stderr, "  → try  %s/%s\n", ev.Provider, ev.Model)
			case "wait":
				fmt.Fprintf(os.Stderr, "  ⏸ %s/%s: %s\n", ev.Provider, ev.Model, ev.Detail)
			case "failure":
				cd := ""
				if ev.Cooldown > 0 {
					cd = fmt.Sprintf(", cooling down %s", ev.Cooldown.Round(time.Second))
				}
				fmt.Fprintf(os.Stderr, "  ✗ fail %s/%s: %s%s\n", ev.Provider, ev.Model, firstLine(ev.Detail), cd)
			case "success":
				fmt.Fprintf(os.Stderr, "  ✓ ok   %s/%s\n", ev.Provider, ev.Model)
			}
		}
	}
	return &env{cfg: cfg, reg: reg, health: health, ledger: ledger, rt: rt}, nil
}

func addConfigFlag(fs *flag.FlagSet) *string {
	return fs.String("config", "", "path to forge config (default: $FORGE_CONFIG, ./forge.json, ~/.forge/config.json)")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ---------- init ----------

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	path := fs.String("o", "", "output path (default: ~/.forge/config.json)")
	force := fs.Bool("force", false, "overwrite an existing config")
	local := fs.Bool("local", false, "offline only: Ollama and nothing else, no API keys")
	model := fs.String("model", "", "model for -local (default: qwen2.5-coder:7b)")
	small := fs.String("small-model", "", "faster model for compaction under -local")
	ctx := fs.Int("context", 0, "context window to declare for -local (default: 16384)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	out := *path
	if out == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		out = home + string(os.PathSeparator) + ".forge" + string(os.PathSeparator) + "config.json"
	}
	if _, err := os.Stat(out); err == nil && !*force {
		return fmt.Errorf("%s already exists (use -force to overwrite)", out)
	}
	if *local {
		if err := config.Save(config.LocalOnly(*model, *small, *ctx), out); err != nil {
			return err
		}
		c, err := config.Load(out)
		if err != nil {
			return err
		}
		m := c.Classes["coder"][0]
		fmt.Printf("wrote %s\n\n", out)
		fmt.Printf(`Offline only. No account, no API key, no quota, no rate limit.

  1. Install Ollama            https://ollama.com
  2. ollama pull %s
  3. Match the window forge declares, or the model will be given
     more context than it can hold and silently truncate it:

       setx OLLAMA_CONTEXT_LENGTH %d      (then restart Ollama)

  4. forge doctor              confirm it is reachable
     forge app                 open the interface

`, m.Model, m.MaxContext)
		return nil
	}

	if err := config.Save(config.Default(), out); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n\n", out)
	fmt.Print(`Next: export the keys you have. Any provider whose key is missing is
skipped automatically, so you can start with one and add more later.

  setx CEREBRAS_API_KEY   "..."     # cloud.cerebras.ai
  setx GROQ_API_KEY       "..."     # console.groq.com/keys
  setx GEMINI_API_KEY     "..."     # aistudio.google.com/apikey
  setx OPENROUTER_API_KEY "..."     # openrouter.ai/keys

Open a NEW terminal after setx, then run:  forge doctor
`)
	return nil
}

// ---------- doctor ----------

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	cfgPath := addConfigFlag(fs)
	reset := fs.Bool("reset", false, "clear all cooldowns before checking")
	timeout := fs.Duration("timeout", 15*time.Second, "per-provider reachability timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	e, err := open(*cfgPath, false)
	if err != nil {
		return err
	}
	defer e.Close()

	if *reset {
		e.health.Clear()
		_ = e.health.Flush()
		fmt.Println("cleared all cooldowns")
	}

	fmt.Printf("config:    %s\n", e.cfg.Path())
	fmt.Printf("state dir: %s\n\n", e.cfg.Dir())

	fmt.Println("PROVIDERS")
	type result struct {
		name   string
		ok     bool
		detail string
		models int
		dur    time.Duration
	}
	results := map[string]result{}
	for _, name := range e.reg.Names() {
		p, _ := e.reg.Get(name)
		if !p.Configured() {
			results[name] = result{name: name, detail: "no API key set"}
			fmt.Printf("  [ -- ] %-12s no API key set\n", name)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		start := time.Now()
		models, err := p.ListModels(ctx)
		cancel()
		d := time.Since(start)
		if err != nil {
			results[name] = result{name: name, detail: firstLine(err.Error()), dur: d}
			fmt.Printf("  [FAIL] %-12s %s\n", name, firstLine(err.Error()))
			continue
		}
		results[name] = result{name: name, ok: true, models: len(models), dur: d}
		fmt.Printf("  [ ok ] %-12s %d models, %s\n", name, len(models), d.Round(time.Millisecond))
	}

	fmt.Println("\nCLASS CHAINS")
	snap := e.health.Snapshot()
	now := time.Now()
	for _, class := range e.cfg.ClassNames() {
		fmt.Printf("  %s\n", class)
		usable := 0
		for i, t := range e.cfg.Classes[class] {
			status := "ready"
			mark := " ok "
			if r, ok := results[t.Provider]; !ok {
				status, mark = "provider disabled", " -- "
			} else if !r.ok {
				status, mark = r.detail, "FAIL"
			}
			if ent, ok := snap[t.Key()]; ok && !ent.CooldownUntil.IsZero() && now.Before(ent.CooldownUntil) {
				status = fmt.Sprintf("cooling down %s (%s)", ent.CooldownUntil.Sub(now).Round(time.Second), ent.LastErrKind)
				mark = "WAIT"
			}
			if mark == " ok " {
				usable++
			}
			fmt.Printf("    %d. [%s] %-12s %-42s %s\n", i+1, mark, t.Provider, t.Model, status)
		}
		if usable == 0 {
			fmt.Printf("    !! no usable target in this class\n")
		}
	}

	fmt.Println("\nIf a model id is wrong, run 'forge models <provider>' and fix the id in the config.")
	return nil
}

// ---------- models ----------

func cmdModels(args []string) error {
	fs := flag.NewFlagSet("models", flag.ExitOnError)
	cfgPath := addConfigFlag(fs)
	filter := fs.String("filter", "", "only show ids containing this substring")
	if err := fs.Parse(args); err != nil {
		return err
	}
	e, err := open(*cfgPath, false)
	if err != nil {
		return err
	}
	defer e.Close()

	names := fs.Args()
	if len(names) == 0 {
		names = e.reg.Names()
	}
	for _, name := range names {
		p, ok := e.reg.Get(name)
		if !ok {
			fmt.Printf("%s: not configured or disabled\n\n", name)
			continue
		}
		if !p.Configured() {
			fmt.Printf("%s: no API key\n\n", name)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		models, err := p.ListModels(ctx)
		cancel()
		if err != nil {
			fmt.Printf("%s: %v\n\n", name, firstLine(err.Error()))
			continue
		}
		sort.Strings(models)
		fmt.Printf("%s (%d)\n", name, len(models))
		for _, m := range models {
			if *filter != "" && !strings.Contains(strings.ToLower(m), strings.ToLower(*filter)) {
				continue
			}
			fmt.Printf("  %s\n", m)
		}
		fmt.Println()
	}
	return nil
}

// ---------- bench ----------

func cmdBench(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	cfgPath := addConfigFlag(fs)
	class := fs.String("class", "", "benchmark every target in this class")
	target := fs.String("target", "", "benchmark one target, as provider:model")
	sizes := fs.String("sizes", "256,1024,4096", "comma-separated approximate prompt sizes in tokens")
	gen := fs.Int("gen", 128, "tokens to generate per run")
	repeats := fs.Int("repeats", 2, "runs per (target, size); best decode rate is kept")
	noWarmup := fs.Bool("no-warmup", false, "skip the warmup call")
	save := fs.Bool("save", true, "save the raw report as JSON in the state dir")
	if err := fs.Parse(args); err != nil {
		return err
	}
	e, err := open(*cfgPath, false)
	if err != nil {
		return err
	}
	defer e.Close()

	var targets []config.Target
	switch {
	case *target != "":
		p, m, ok := strings.Cut(*target, ":")
		if !ok || m == "" {
			return fmt.Errorf("-target must look like provider:model")
		}
		targets = []config.Target{{Provider: p, Model: m}}
	case *class != "":
		ts, ok := e.cfg.Classes[*class]
		if !ok {
			return fmt.Errorf("unknown class %q", *class)
		}
		targets = ts
	default:
		// Default: one representative target per provider, deduplicated,
		// so a bare `forge bench` covers the whole fleet without repeats.
		seen := map[string]bool{}
		for _, cls := range e.cfg.ClassNames() {
			for _, t := range e.cfg.Classes[cls] {
				if seen[t.Provider] {
					continue
				}
				seen[t.Provider] = true
				targets = append(targets, t)
			}
		}
	}

	opts := bench.DefaultOptions()
	opts.GenTokens = *gen
	opts.Repeats = *repeats
	opts.Warmup = !*noWarmup
	opts.PromptSizes = nil
	for _, s := range strings.Split(*sizes, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			return fmt.Errorf("bad -sizes entry %q", s)
		}
		opts.PromptSizes = append(opts.PromptSizes, n)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rep, err := bench.Run(ctx, e.reg, targets, opts)
	if err != nil {
		return err
	}
	bench.WriteMarkdown(os.Stdout, rep)
	if *save {
		path, err := bench.Save(e.cfg.Dir(), rep)
		if err != nil {
			return err
		}
		fmt.Printf("\nraw report: %s\n", path)
	}
	return nil
}

// ---------- chat ----------

func cmdChat(args []string) error {
	fs := flag.NewFlagSet("chat", flag.ExitOnError)
	cfgPath := addConfigFlag(fs)
	class := fs.String("class", "", "routing class or provider:model pin (default: server.default_class)")
	system := fs.String("system", "", "system prompt")
	maxTok := fs.Int("max-tokens", 0, "max output tokens")
	temp := fs.Float64("temp", 0, "temperature")
	noStream := fs.Bool("no-stream", false, "use the non-streaming API")
	quiet := fs.Bool("quiet", false, "suppress routing trace on stderr")
	if err := fs.Parse(args); err != nil {
		return err
	}
	e, err := open(*cfgPath, !*quiet)
	if err != nil {
		return err
	}
	defer e.Close()

	prompt := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if prompt == "" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		prompt = strings.TrimSpace(string(b))
	}
	if prompt == "" {
		return fmt.Errorf("no prompt given (pass it as an argument or on stdin)")
	}

	cls := *class
	if cls == "" {
		cls = e.cfg.Server.DefaultClass
	}

	req := provider.Request{MaxTokens: *maxTok, Temperature: *temp}
	if *system != "" {
		req.Messages = append(req.Messages, provider.Message{Role: provider.RoleSystem, Content: *system})
	}
	req.Messages = append(req.Messages, provider.Message{Role: provider.RoleUser, Content: prompt})

	ctx := context.Background()
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	var resp *provider.Response
	if *noStream {
		resp, err = e.rt.Chat(ctx, cls, req)
		if err != nil {
			return err
		}
		out.WriteString(resp.Content)
	} else {
		resp, err = e.rt.Stream(ctx, cls, req, func(c provider.Chunk) error {
			if c.Content != "" {
				out.WriteString(c.Content)
				out.Flush()
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	out.WriteString("\n")
	out.Flush()

	est := ""
	if resp.Usage.Estimated {
		est = " (estimated)"
	}
	fmt.Fprintf(os.Stderr, "\n[%s/%s  %d in + %d out%s  ttft %s  %.1f tok/s  total %s]\n",
		resp.Provider, resp.Model, resp.Usage.PromptTokens, resp.Usage.CompletionTokens, est,
		resp.TTFT.Round(time.Millisecond), resp.DecodeTPS(), resp.Latency.Round(time.Millisecond))
	return nil
}

// ---------- do (the agent) ----------

// DefaultTools is the Phase 2 toolset. Read/search tools are unrestricted;
// the three that change state route through the approver.
func defaultTools(withSubAgents bool) *tools.Registry {
	r := tools.NewRegistry()
	if withSubAgents {
		r.Register(tools.Task{Agents: agent.Infos(agent.Builtins)})
	}
	r.Register(
		tools.ReadFile{},
		tools.Glob{},
		tools.Grep{},
		tools.ListDir{},
		tools.SearchCode{},
		tools.Expand{},
		tools.EditFile{},
		tools.WriteFile{},
		tools.RunCommand{},
		tools.Verify{},
		tools.TodoWrite{},
		tools.Remember{},
	)
	return r
}

func cmdDo(args []string) error {
	fs := flag.NewFlagSet("do", flag.ExitOnError)
	cfgPath := addConfigFlag(fs)
	class := fs.String("class", "", "routing class or provider:model pin (default: server.default_class)")
	dir := fs.String("dir", ".", "workspace root; the agent cannot read or write outside it")
	modeFlag := fs.String("approval", "ask", "readonly | ask | auto-edit | yolo")
	protoFlag := fs.String("edit", "auto", "edit protocol: blocks (best for small models) | tool | auto")
	maxSteps := fs.Int("max-steps", 30, "maximum model turns before giving up")
	maxTokens := fs.Int("max-tokens", 0, "total token budget for the run (0 = unlimited)")
	maxBytes := fs.Int("max-tool-bytes", 30000, "cap on how much any single tool result may add to context")
	temp := fs.Float64("temp", 0, "sampling temperature (0 = provider default)")
	allow := fs.String("allow", "", "comma-separated extra command prefixes to auto-approve")
	quiet := fs.Bool("quiet", false, "suppress the model's prose, show only actions")
	trace := fs.Bool("trace", false, "show provider routing decisions")
	repoMapTokens := fs.Int("repo-map", 1024, "token budget for the repository map (0 disables it)")
	focus := fs.String("focus", "", "comma-separated files to bias the repo map toward")
	ctxBudget := fs.Int("context-budget", 0, "context window to compact against (0 = ask the router)")
	compactAt := fs.Float64("compact-at", 0.7, "fraction of the budget at which to compact")
	keepTail := fs.Int("keep-tail", 6, "recent messages kept verbatim through compaction")
	noNotes := fs.Bool("no-notes", false, "do not load or write cross-session notes")
	embedClass := fs.String("embed-class", "embed", "routing class used for semantic search")
	embedModel := fs.String("embed-model", "", "path to a local embedding model directory")
	verifyCmd := fs.String("verify-cmd", "", "run this instead of the auto-detected build/test checks")
	noVerify := fs.Bool("no-verify", false, "do not run the project's checks when the model finishes")
	maxRepairs := fs.Int("max-repairs", 3, "how many times a failing verification is handed back")
	verifyTimeout := fs.Duration("verify-timeout", 5*time.Minute, "per-check timeout")
	revertOnFail := fs.Bool("revert-on-fail", false, "restore every changed file if verification never passes")
	maxSpawns := fs.Int("max-subagents", 8, "total sub-agent delegations allowed per run")
	maxParallel := fs.Int("parallel-subagents", 3, "how many delegations may run at once (1 for a local-only setup)")
	noSubAgents := fs.Bool("no-subagents", false, "disable delegation entirely")
	uiFlag := fs.String("ui", "auto", "auto | rich | plain")
	if err := fs.Parse(args); err != nil {
		return err
	}

	mode, err := approval.ParseMode(*modeFlag)
	if err != nil {
		return err
	}
	uiMode, err := parseUIMode(*uiFlag)
	if err != nil {
		return err
	}
	proto, err := agent.ParseProtocol(*protoFlag)
	if err != nil {
		return err
	}

	task := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if task == "" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		task = strings.TrimSpace(string(b))
	}
	if task == "" {
		return fmt.Errorf("no task given (pass it as an argument or on stdin)")
	}

	e, err := open(*cfgPath, *trace)
	if err != nil {
		return err
	}
	defer e.Close()

	ws, err := tools.NewWorkspace(*dir)
	if err != nil {
		return err
	}

	var extraAllow []string
	for _, p := range strings.Split(*allow, ",") {
		if p = strings.TrimSpace(p); p != "" {
			extraAllow = append(extraAllow, p)
		}
	}

	cls := *class
	if cls == "" {
		cls = e.cfg.Server.DefaultClass
	}

	// The display owns the terminal, so it is torn down on every exit path —
	// leaving a shell in raw mode is the one failure a TUI must never have.
	policy := approval.NewPolicy(mode, extraAllow)
	disp := newDisplay(uiMode, policy, cls, *maxSteps, *maxTokens)
	defer disp.Close()

	notesFile := ""
	if !*noNotes {
		notesFile = tools.NotesPath(e.cfg.Dir(), ws.Root())
	}

	li := newLazyIndex(ws.Root(), index.Dir(e.cfg.Dir(), ws.Root()), e.rt, *embedClass, index.Options{})
	if em, err := resolveEmbedder(*embedModel, e.cfg.Dir(), nil); err != nil {
		return err
	} else if em != nil {
		li.useLocal(em)
		disp.Printf("embedder:  local, %s", em.Describe())
	}
	journal := checkpoint.New(ws.Root())
	v := newVerifier(ws.Root(), *verifyCmd, *verifyTimeout)

	reg := defaultTools(!*noSubAgents)
	env := &tools.Env{
		WS:         ws,
		Approver:   disp.approver,
		Out:        disp.Writer(),
		MaxBytes:   *maxBytes,
		Todos:      tools.NewTodoList(),
		Overflow:   tools.NewOverflow(filepath.Join(e.cfg.Dir(), "overflow")),
		NotesFile:  notesFile,
		SearchCode: li.search,
		Snapshot:   journal.Record,
	}
	if v.enabled() {
		env.Verify = v.forTool
	}

	disp.Printf("workspace: %s", ws.Root())
	disp.Printf("class:     %s", cls)
	line := fmt.Sprintf("approval:  %s", mode)
	if !term.IsTTY(os.Stdin) && (mode == approval.Ask || mode == approval.ReadOnly) {
		line += "  (stdin is not a terminal; prompts will be denied)"
	}
	disp.Printf("%s", line)
	disp.Printf("edits:     %s", proto)
	disp.Printf("ui:        %s", uiLabel(disp))
	switch {
	case *noVerify:
		disp.Printf("verify:    disabled")
	case v.enabled():
		names := make([]string, 0, len(v.checks))
		for _, c := range v.checks {
			names = append(names, c.Command)
		}
		disp.Printf("verify:    %s", strings.Join(names, " · "))
	default:
		disp.Printf("verify:    none detected (pass -verify-cmd to enable)")
	}

	// The repo map is built once per run and folded into the system prompt.
	// Rebuilding it mid-run would change the prompt prefix every turn and
	// throw away the local model's KV cache each time.
	repoMap := ""
	if *repoMapTokens > 0 {
		var focusFiles []string
		for _, f := range strings.Split(*focus, ",") {
			if f = strings.TrimSpace(f); f != "" {
				focusFiles = append(focusFiles, f)
			}
		}
		started := time.Now()
		m, err := repomap.Build(ws.Root(), repomap.Options{
			Focus: focusFiles, CacheDir: e.cfg.Dir(),
		})
		if err != nil {
			disp.Printf("repo map: %v (continuing without one)", err)
		} else {
			repoMap = m.Render(*repoMapTokens)
			disp.Printf("repo map:  %d files, ~%d tok, %s",
				m.Scanned, len(repoMap)*10/36, time.Since(started).Round(time.Millisecond))
		}
	}

	notes := tools.LoadNotes(notesFile, 4000)
	if notes != "" {
		disp.Printf("notes:     %d chars from earlier sessions", len(notes))
	}

	// Delegation is wired after the env exists, because sub-agents inherit the
	// parent's workspace, overflow store, and search backend.
	var spawner *agent.Spawner
	if !*noSubAgents {
		spawner = agent.NewSpawner(e.rt, env, reg, agent.SpawnerConfig{
			MaxSpawns:     *maxSpawns,
			MaxConcurrent: *maxParallel,
			RepoMap:       repoMap,
			ParentClass:   cls,
		}, disp.Writer())
		env.Spawn = spawner.Spawn
		disp.Printf("subagents: %d max, %d parallel", *maxSpawns, *maxParallel)
	}

	ag := agent.New(e.rt, reg, env, agent.Config{
		Class:         cls,
		MaxSteps:      *maxSteps,
		MaxTokens:     *maxTokens,
		Temperature:   *temp,
		Protocol:      proto,
		Quiet:         *quiet,
		RepoMap:       repoMap,
		Notes:         notes,
		ContextBudget: *ctxBudget,
		CompactAt:     *compactAt,
		KeepTail:      *keepTail,
		Verify:        verifyHook(v, *noVerify),
		MaxRepairs:    *maxRepairs,
		SubStats:      subStats(spawner),
		Observer:      disp,
	}, disp.Writer())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	out, runErr := ag.Run(ctx, task)

	// The summary belongs in ordinary scrollback, not under a live status
	// region, so the UI is torn down before it is printed.
	disp.Close()

	fmt.Fprintf(os.Stderr, "\n─── %s ───\n", out.StopReason)
	fmt.Fprintf(os.Stderr, "steps %d · %d in + %d out tok · %s\n",
		out.Steps, out.Usage.PromptTokens, out.Usage.CompletionTokens, out.Elapsed.Round(time.Second))
	if out.SubAgents > 0 {
		fmt.Fprintf(os.Stderr, "%d delegation(s) spent %d tok in contexts this conversation never saw\n",
			out.SubAgents, out.SubUsage.TotalTokens)
	}
	if out.Compactions > 0 {
		fmt.Fprintf(os.Stderr, "compacted %d time(s), ~%d tokens reclaimed\n", out.Compactions, out.TokensSaved)
	}
	switch {
	case !out.VerifyRan:
		fmt.Fprintf(os.Stderr, "verification: not run\n")
	case out.Verified:
		fmt.Fprintf(os.Stderr, "verification: PASSED")
		if out.Repairs > 0 {
			fmt.Fprintf(os.Stderr, " after %d repair(s)", out.Repairs)
		}
		fmt.Fprintln(os.Stderr)
	default:
		fmt.Fprintf(os.Stderr, "verification: FAILED after %d repair attempt(s)\n", out.Repairs)
	}

	// Only offer to undo when verification actually ran and actually failed.
	// Reverting because no checks exist would throw away good work.
	if *revertOnFail && out.VerifyRan && !out.Verified && journal.Len() > 0 {
		restored, err := journal.Revert()
		if err != nil {
			fmt.Fprintf(os.Stderr, "revert: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "reverted %d file(s) to their pre-run state\n", len(restored))
		}
	} else if journal.Len() > 0 {
		snapDir := filepath.Join(e.cfg.Dir(), "undo", time.Now().Format("20060102-150405"))
		if err := journal.Save(snapDir); err == nil {
			fmt.Fprintf(os.Stderr, "originals saved: %s\n", snapDir)
		}
	}
	if len(out.FilesChanged) > 0 {
		fmt.Fprintf(os.Stderr, "changed %d file(s):\n", len(out.FilesChanged))
		for _, f := range out.FilesChanged {
			fmt.Fprintf(os.Stderr, "  %s\n", f)
		}
	} else {
		fmt.Fprintf(os.Stderr, "no files changed\n")
	}
	if out.FinalText != "" {
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Println(out.FinalText)
	}
	return runErr
}

// ---------- map ----------

func cmdMap(args []string) error {
	fs := flag.NewFlagSet("map", flag.ExitOnError)
	dir := fs.String("dir", ".", "repository root")
	budget := fs.Int("tokens", 1024, "token budget for the map")
	focus := fs.String("focus", "", "comma-separated files to bias ranking toward")
	ranked := fs.Bool("ranked", false, "list files by rank instead of rendering the map")
	noCache := fs.Bool("no-cache", false, "ignore the extraction cache")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cacheDir := ""
	if !*noCache {
		if cfg, err := config.Load(""); err == nil {
			cacheDir = cfg.Dir()
		}
	}
	var focusFiles []string
	for _, f := range strings.Split(*focus, ",") {
		if f = strings.TrimSpace(f); f != "" {
			focusFiles = append(focusFiles, f)
		}
	}

	started := time.Now()
	m, err := repomap.Build(*dir, repomap.Options{Focus: focusFiles, CacheDir: cacheDir})
	if err != nil {
		return err
	}
	elapsed := time.Since(started)

	if *ranked {
		fmt.Printf("%-56s %s\n", "FILE", "RANK")
		for _, p := range m.RankedFiles() {
			fmt.Printf("%-56s %.5f\n", trim(p, 56), m.Rank(p))
		}
	} else {
		out := m.Render(*budget)
		fmt.Print(out)
		fmt.Fprintf(os.Stderr, "\n~%d tokens\n", len(out)*10/36)
	}
	fmt.Fprintf(os.Stderr, "%d files scanned in %s\n", m.Scanned, elapsed.Round(time.Millisecond))
	return nil
}

// ---------- index / search ----------

func cmdIndex(args []string) error {
	fs := flag.NewFlagSet("index", flag.ExitOnError)
	cfgPath := addConfigFlag(fs)
	dir := fs.String("dir", ".", "repository root")
	doEmbed := fs.Bool("embed", false, "also compute embeddings for semantic search")
	class := fs.String("embed-class", "embed", "routing class used for embeddings")
	rebuild := fs.Bool("rebuild", false, "discard the existing index and start over")
	statsOnly := fs.Bool("stats", false, "report on the existing index without changing it")
	maxChunks := fs.Int("max-chunks", 0, "cap the corpus size (0 = unbounded)")
	modelDir := fs.String("embed-model", "", "path to a local embedding model directory (no server needed)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	e, err := open(*cfgPath, false)
	if err != nil {
		return err
	}
	defer e.Close()

	ws, err := tools.NewWorkspace(*dir)
	if err != nil {
		return err
	}
	idxDir := index.Dir(e.cfg.Dir(), ws.Root())
	opts := index.Options{MaxChunks: *maxChunks}

	if *statsOnly {
		ix, err := index.Load(idxDir)
		if err != nil {
			return fmt.Errorf("no index at %s (run 'forge index' first)", idxDir)
		}
		changed, removed, _ := ix.Stale(ws.Root(), opts)
		printIndexStats(ix, idxDir)
		fmt.Printf("stale:       %d changed, %d removed\n", len(changed), len(removed))
		return nil
	}

	started := time.Now()
	var ix *index.Index
	if *rebuild {
		ix, err = index.Build(ws.Root(), opts)
	} else {
		ix, err = index.OpenOrBuild(idxDir, ws.Root(), opts)
	}
	if err != nil {
		return err
	}
	fmt.Printf("chunked %d files into %d chunks in %s\n",
		len(ix.Files), len(ix.Chunks), time.Since(started).Round(time.Millisecond))

	if *doEmbed {
		var embedFn index.EmbedFunc
		model := ""

		em, err := resolveEmbedder(*modelDir, e.cfg.Dir(), os.Stdout)
		if err != nil {
			return err
		}

		switch {
		case em != nil:
			// In-process: no server, no key, no quota.
			embedFn = em.Embed
			model = "local:" + filepath.Base(strings.TrimRight(em.Source(), `/\`))

		case e.rt.CanEmbed(*class):
			if targets, err := e.rt.Resolve(*class); err == nil && len(targets) > 0 {
				model = targets[0].Model
			}
			embedFn = func(ctx context.Context, texts []string) ([][]float32, error) {
				resp, err := e.rt.Embed(ctx, *class, texts)
				if err != nil {
					return nil, err
				}
				return resp.Vectors, nil
			}

		default:
			return fmt.Errorf("nothing can embed: class %q has no capable target and -embed-model was not given\n\n%s",
				*class, embed.ModelHint)
		}

		need, _ := ix.NeedsEmbedding()
		if len(need) == 0 {
			fmt.Println("all chunks already have embeddings")
		} else {
			fmt.Printf("embedding %d chunks ...\n", len(need))
		}
		embStart := time.Now()
		err = ix.Vectorize(context.Background(), model, embedFn, func(done, total int) {
			fmt.Printf("\r  %d/%d", done, total)
		})
		fmt.Println()
		if err != nil {
			return fmt.Errorf("embedding: %w", err)
		}
		if len(need) > 0 {
			fmt.Printf("embedded in %s\n", time.Since(embStart).Round(time.Second))
		}
	}

	if err := ix.Save(idxDir); err != nil {
		return err
	}
	printIndexStats(ix, idxDir)
	return nil
}

func printIndexStats(ix *index.Index, dir string) {
	s := ix.Stats()
	fmt.Printf("\nfiles:       %d\nchunks:      %d\nterms:       %d\n", s.Files, s.Chunks, s.Terms)
	if s.Embedded > 0 {
		fmt.Printf("embedded:    %d chunks, %d dims via %s\n", s.Embedded, s.Dim, s.EmbedModel)
		// The whole argument for brute force lives in these two numbers.
		fmt.Printf("vector RAM:  %.1f MB binary (first stage) + %.1f MB float32 (rerank)\n",
			float64(s.BinaryBytes)/(1<<20), float64(s.FullBytes)/(1<<20))
	} else {
		fmt.Printf("embedded:    none (keyword search only; use -embed to add vectors)\n")
	}
	fmt.Printf("location:    %s\n", dir)
}

func cmdSearch(args []string) error {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	cfgPath := addConfigFlag(fs)
	dir := fs.String("dir", ".", "repository root")
	limit := fs.Int("limit", 8, "maximum results")
	mode := fs.String("mode", "hybrid", "hybrid, keyword, or semantic")
	class := fs.String("embed-class", "embed", "routing class used for embeddings")
	modelDir := fs.String("embed-model", "", "path to a local embedding model directory")
	full := fs.Bool("full", false, "print the full text of each result")
	if err := fs.Parse(args); err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		return fmt.Errorf("no query given")
	}

	e, err := open(*cfgPath, false)
	if err != nil {
		return err
	}
	defer e.Close()

	ws, err := tools.NewWorkspace(*dir)
	if err != nil {
		return err
	}
	li := newLazyIndex(ws.Root(), index.Dir(e.cfg.Dir(), ws.Root()), e.rt, *class, index.Options{})
	if em, err := resolveEmbedder(*modelDir, e.cfg.Dir(), nil); err != nil {
		return err
	} else if em != nil {
		li.useLocal(em)
	}

	started := time.Now()
	hits, modeUsed, err := li.search(context.Background(), query, *limit, *mode)
	if err != nil {
		return err
	}
	elapsed := time.Since(started)

	if len(hits) == 0 {
		fmt.Printf("no results for %q\n", query)
		return nil
	}
	fmt.Printf("%d results for %q (%s, %s)\n\n", len(hits), query, modeUsed, elapsed.Round(time.Microsecond))
	for i, h := range hits {
		fmt.Printf("── %d. %s:%d-%d", i+1, h.File, h.Start, h.End)
		if h.Symbol != "" {
			fmt.Printf("  %s %s", h.Kind, h.Symbol)
		}
		fmt.Printf("  (%.3f)\n", h.Score)
		if *full {
			fmt.Printf("%s\n\n", h.Text)
		} else {
			fmt.Printf("%s\n\n", firstNLines(h.Text, 4))
		}
	}
	return nil
}

func firstNLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n") + fmt.Sprintf("\n   … (%d more lines)", len(lines)-n)
}

// ---------- serve ----------

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := addConfigFlag(fs)
	addr := fs.String("addr", "", "listen address (default: server.addr from config)")
	quiet := fs.Bool("quiet", false, "suppress per-attempt routing trace")
	if err := fs.Parse(args); err != nil {
		return err
	}
	e, err := open(*cfgPath, !*quiet)
	if err != nil {
		return err
	}
	defer e.Close()

	if *addr != "" {
		e.cfg.Server.Addr = *addr
	}
	logger := log.New(os.Stderr, "forge ", log.LstdFlags)
	srv := serve.New(e.rt, logger)

	fmt.Printf(`point any OpenAI-compatible client at:

  base URL : http://%s/v1
  api key  : %s
  models   : %s

`, e.cfg.Server.Addr, keyHint(e.cfg.Server.APIKey), strings.Join(prefixed(e.cfg.ClassNames()), ", "))

	return srv.Run(context.Background())
}

func prefixed(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = "forge-" + s
	}
	return out
}

func keyHint(k string) string {
	if k == "" {
		return "(any value — none required)"
	}
	return "(set in config)"
}

// ---------- selfcheck ----------

func cmdSelfcheck(args []string) error {
	fs := flag.NewFlagSet("selfcheck", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	fmt.Print("running invariants against in-process stub servers\n\n")
	checks, ok := selfcheck.Run(os.Stdout)

	passed := 0
	for _, c := range checks {
		if c.Passed {
			passed++
		}
	}
	fmt.Printf("\n%d/%d checks passed\n", passed, len(checks))
	if !ok {
		return fmt.Errorf("selfcheck failed")
	}
	return nil
}

// ---------- usage ----------

func cmdUsage(args []string) error {
	fs := flag.NewFlagSet("usage", flag.ExitOnError)
	cfgPath := addConfigFlag(fs)
	since := fs.String("since", "24h", "window, e.g. 90m, 24h, 7d, or 'all'")
	by := fs.String("by", "target", "group by: target, provider, class, day")
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	e, err := open(*cfgPath, false)
	if err != nil {
		return err
	}
	defer e.Close()

	cutoff, err := parseSince(*since)
	if err != nil {
		return err
	}
	var keyOf func(router.Record) string
	switch *by {
	case "target":
		keyOf = func(r router.Record) string { return r.Provider + "/" + r.Model }
	case "provider":
		keyOf = func(r router.Record) string { return r.Provider }
	case "class":
		keyOf = func(r router.Record) string { return r.Class }
	case "day":
		keyOf = func(r router.Record) string { return r.TS.Format("2006-01-02") }
	default:
		return fmt.Errorf("-by must be one of: target, provider, class, day")
	}

	stats, err := router.Summarize(e.ledger.Path(), cutoff, keyOf)
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(stats)
	}
	if len(stats) == 0 {
		fmt.Printf("no usage recorded since %s\n(ledger: %s)\n", *since, e.ledger.Path())
		return nil
	}

	fmt.Printf("usage since %s, grouped by %s\n\n", *since, *by)
	fmt.Printf("%-46s %7s %7s %12s %12s %10s\n", strings.ToUpper(*by), "CALLS", "FAILS", "IN TOK", "OUT TOK", "AVG MS")
	var tc, tf, ti, to int
	for _, s := range stats {
		fmt.Printf("%-46s %7d %7d %12d %12d %10d\n", trim(s.Key, 46), s.Calls, s.Failures, s.PromptTok, s.OutTok, s.AvgLatencyMS())
		tc += s.Calls
		tf += s.Failures
		ti += s.PromptTok
		to += s.OutTok
	}
	fmt.Printf("%-46s %7d %7d %12d %12d\n", "TOTAL", tc, tf, ti, to)
	fmt.Printf("\nledger: %s\n", e.ledger.Path())
	return nil
}

func parseSince(s string) (time.Time, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "all" || s == "" {
		return time.Time{}, nil
	}
	// time.ParseDuration has no day unit, and "7d" is the natural thing to type.
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return time.Time{}, fmt.Errorf("bad -since %q", s)
		}
		return time.Now().Add(-time.Duration(n) * 24 * time.Hour), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return time.Time{}, fmt.Errorf("bad -since %q", s)
	}
	return time.Now().Add(-d), nil
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
