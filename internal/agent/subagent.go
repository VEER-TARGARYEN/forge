package agent

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/VEER-TARGARYEN/forge/internal/approval"
	"github.com/VEER-TARGARYEN/forge/internal/provider"
	"github.com/VEER-TARGARYEN/forge/internal/router"
	"github.com/VEER-TARGARYEN/forge/internal/tools"
)

// Sub-agents exist for one reason: an exploration that costs 15,000 tokens
// should cost the caller 300.
//
// A search that reads nine files to answer one question currently leaves all
// nine files in the main conversation forever, where they get re-sent on every
// subsequent turn and eventually trigger compaction. Running it in a separate
// context and returning only the conclusion keeps the finding and discards the
// search.
//
// The corollary is that a sub-agent's final message *is* its entire output.
// Nothing else it does is visible to the caller, which its prompt states in as
// many words — a sub-agent that answers "I found it in the config package"
// has failed, however much good work preceded that sentence.

// Spec defines a sub-agent role.
type Spec struct {
	Name        string
	Description string
	// Class routes this role. Exploration is grunt work and defaults to the
	// cheap class; review benefits from the stronger one.
	Class string
	Tools []string
	// MaxSteps bounds one delegation. Sub-agents are meant to be short.
	MaxSteps int
	// MaxWords caps the returned summary. A chatty sub-agent would otherwise
	// reintroduce the very context cost it exists to avoid.
	MaxWords int
	Role     string
}

// readOnlyTools is the toolset every built-in sub-agent gets.
//
// Sub-agents cannot write, run commands, or verify. That is a deliberate
// limit, not an oversight: parallel delegations editing the same tree would
// race, and an approval prompt attributed to an invisible sub-context is not
// something a human can meaningfully answer.
var readOnlyTools = []string{"read_file", "glob", "grep", "list_dir", "search_code", "expand"}

// Builtins are the roles available to the task tool.
var Builtins = []Spec{
	{
		Name: "explore",
		Description: "Search the codebase and report findings. Use for questions like " +
			"'where is X handled' or 'what calls Y' when answering would take several " +
			"searches and file reads.",
		Class: "cheap", Tools: readOnlyTools, MaxSteps: 12, MaxWords: 400,
		Role: `Find what was asked for and report it precisely.

Search first, read second. Prefer grep and search_code over reading files
speculatively — you have a small step budget and reading a whole package to
answer one question will exhaust it.`,
	},
	{
		Name: "review",
		Description: "Adversarially review code for bugs. Use after making a non-trivial change, " +
			"or when asked to check existing code.",
		Class: "coder", Tools: readOnlyTools, MaxSteps: 14, MaxWords: 400,
		Role: `Look for defects, not style. Read the code named in the task and try to break it.

Prioritise: logic that is wrong for some input, unhandled errors, off-by-one
and boundary cases, concurrent access to shared state, resource leaks, and
assumptions the surrounding code does not guarantee.

Report only problems you can point at a specific line for. If you find nothing,
say so — inventing a concern to look thorough is worse than finding nothing.`,
	},
	{
		Name: "plan",
		Description: "Produce an implementation plan for a change. Use before a task that touches " +
			"several files, so the work is scoped before any edits happen.",
		Class: "planner", Tools: readOnlyTools, MaxSteps: 14, MaxWords: 500,
		Role: `Work out how the change should be made, then describe it as ordered steps.

Read enough of the real code to be concrete. Each step must name the file it
touches and what changes there. Flag anything that looks like it will be
harder than it appears.

Do not write the code. The caller will.`,
	},
}

// SpecsByName indexes a spec list.
func SpecsByName(specs []Spec) map[string]Spec {
	m := make(map[string]Spec, len(specs))
	for _, s := range specs {
		m[s.Name] = s
	}
	return m
}

// Infos renders the roster for the task tool's schema.
func Infos(specs []Spec) []tools.AgentInfo {
	out := make([]tools.AgentInfo, 0, len(specs))
	for _, s := range specs {
		out = append(out, tools.AgentInfo{Name: s.Name, Description: s.Description})
	}
	return out
}

// SpawnerConfig configures delegation for one run.
type SpawnerConfig struct {
	Specs []Spec
	// MaxSpawns bounds total delegations per run, so a loop of "explore some
	// more" cannot run up an unbounded bill.
	MaxSpawns int
	// MaxConcurrent bounds parallel delegations. Against hosted providers,
	// running several at once is close to free wall-clock. Against one local
	// model it is not — the requests just queue — so this drops to 1 for a
	// local-only setup.
	MaxConcurrent int
	// RepoMap is handed to sub-agents so they start oriented rather than
	// spending their first two steps working out the layout.
	RepoMap string
	// ParentClass is the fallback when a role's preferred class is not defined
	// in the user's config.
	ParentClass string
	// Quiet suppresses the per-delegation transcript line.
	Quiet bool
}

func (c *SpawnerConfig) applyDefaults() {
	if len(c.Specs) == 0 {
		c.Specs = Builtins
	}
	if c.MaxSpawns <= 0 {
		c.MaxSpawns = 8
	}
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = 3
	}
}

// Spawner runs sub-agents in isolated contexts.
type Spawner struct {
	rt        *router.Router
	parent    *tools.Env
	parentReg *tools.Registry
	cfg       SpawnerConfig
	specs     map[string]Spec
	out       io.Writer

	mu     sync.Mutex
	spawns int
	usage  provider.Usage
	sem    chan struct{}
}

// NewSpawner builds the delegation engine. parentReg supplies the tool
// implementations a sub-agent may use; each role picks a subset by name, so a
// sub-agent can never hold a capability the parent does not have.
func NewSpawner(rt *router.Router, parentEnv *tools.Env, parentReg *tools.Registry, cfg SpawnerConfig, out io.Writer) *Spawner {
	cfg.applyDefaults()
	return &Spawner{
		rt: rt, parent: parentEnv, parentReg: parentReg, cfg: cfg,
		specs: SpecsByName(cfg.Specs), out: out,
		sem: make(chan struct{}, cfg.MaxConcurrent),
	}
}

// Usage reports the tokens spent by every sub-agent so far, so the parent can
// roll them into its own accounting rather than under-reporting the run.
func (s *Spawner) Usage() provider.Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage
}

func (s *Spawner) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spawns
}

func (s *Spawner) reserve() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.spawns >= s.cfg.MaxSpawns {
		return fmt.Errorf("delegation limit reached (%d); do the remaining work yourself", s.cfg.MaxSpawns)
	}
	s.spawns++
	return nil
}

// Spawn runs one sub-agent to completion and returns only its final message.
func (s *Spawner) Spawn(ctx context.Context, req tools.SpawnRequest) (*tools.SpawnResult, error) {
	spec, ok := s.specs[strings.ToLower(strings.TrimSpace(req.Agent))]
	if !ok {
		return nil, fmt.Errorf("no sub-agent named %q; available: %s", req.Agent, strings.Join(s.names(), ", "))
	}
	if strings.TrimSpace(req.Task) == "" {
		return nil, fmt.Errorf("task is required")
	}
	if err := s.reserve(); err != nil {
		return nil, err
	}

	// Bound concurrency here rather than at the call site, so parallel and
	// sequential callers share one limit.
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	start := time.Now()
	child := s.childAgent(spec)
	out, err := child.Run(ctx, req.Task)

	// Usage is rolled up even on failure: a delegation that burned tokens and
	// then died still cost them.
	if out != nil {
		s.mu.Lock()
		s.usage.PromptTokens += out.Usage.PromptTokens
		s.usage.CompletionTokens += out.Usage.CompletionTokens
		s.usage.TotalTokens += out.Usage.TotalTokens
		s.mu.Unlock()
	}
	if err != nil {
		return nil, fmt.Errorf("%s sub-agent failed: %w", spec.Name, err)
	}

	summary := strings.TrimSpace(out.FinalText)
	if summary == "" {
		summary = fmt.Sprintf("(the %s sub-agent stopped after %d step(s) without reporting anything: %s)",
			spec.Name, out.Steps, out.StopReason)
	}
	summary = clampWords(summary, spec.MaxWords)

	if !s.cfg.Quiet && s.out != nil {
		fmt.Fprintf(s.out, "  ⟨%s⟩ %s → %d steps, %d tok, %s\n",
			spec.Name, firstLine(req.Task, 60), out.Steps, out.Usage.TotalTokens,
			time.Since(start).Round(time.Millisecond))
	}

	return &tools.SpawnResult{
		Agent: spec.Name, Summary: summary, Steps: out.Steps,
		PromptTokens: out.Usage.PromptTokens, CompletionTokens: out.Usage.CompletionTokens,
		StopReason: out.StopReason,
	}, nil
}

// childAgent builds an isolated agent for one delegation.
func (s *Spawner) childAgent(spec Spec) *Agent {
	reg := tools.NewRegistry()
	for _, name := range spec.Tools {
		if t, ok := s.parentRegistryTool(name); ok {
			reg.Register(t)
		}
	}

	// A fresh Env: same workspace and overflow store, but no approver-backed
	// capabilities. Sharing the parent's Env struct would let agent.New
	// overwrite the parent's Changed hook.
	env := &tools.Env{
		WS:         s.parent.WS,
		Approver:   &approval.Static{Allow: false, Reason: "sub-agents are read-only"},
		Out:        io.Discard,
		MaxBytes:   s.parent.MaxBytes,
		Todos:      tools.NewTodoList(),
		Overflow:   s.parent.Overflow,
		SearchCode: s.parent.SearchCode,
		// Deliberately nil: no Verify, no Snapshot, no Spawn. The last of
		// those is the depth limit — a sub-agent cannot delegate further.
	}

	return New(s.rt, reg, env, Config{
		Class:    s.classFor(spec),
		MaxSteps: spec.MaxSteps,
		Protocol: ProtoTool, // read-only: the block protocol is irrelevant
		Quiet:    true,
		RepoMap:  s.cfg.RepoMap,
		SubRole:  subPrompt(spec),
	}, io.Discard)
}

// classFor resolves a role's routing class against what the config actually
// defines, falling back to the parent's class.
//
// The built-in roles name "cheap" and "planner" because that is the right
// split, but a user's config need not define either. Refusing to delegate
// because a convenience class is missing would be a poor trade: running the
// sub-agent on the parent's class still isolates the context, which is the
// part that matters.
func (s *Spawner) classFor(spec Spec) string {
	for _, c := range []string{spec.Class, s.cfg.ParentClass, "coder"} {
		if c == "" {
			continue
		}
		// Usable, not merely defined: a class whose every target lacks a key
		// resolves fine and then fails on the first call.
		if s.rt.ClassUsable(c) {
			return c
		}
	}
	return s.cfg.ParentClass
}

// tools registered on the parent, looked up by name.
func (s *Spawner) parentRegistryTool(name string) (tools.Tool, bool) {
	if s.parentReg == nil {
		return nil, false
	}
	return s.parentReg.Get(name)
}

func (s *Spawner) names() []string {
	out := make([]string, 0, len(s.specs))
	for n := range s.specs {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// subPrompt builds the role section appended to a sub-agent's system prompt.
func subPrompt(spec Spec) string {
	return fmt.Sprintf(`YOU ARE A SUB-AGENT

You are the %q sub-agent, running inside a larger task on behalf of another
agent.

YOUR FINAL MESSAGE IS YOUR ENTIRE OUTPUT. The agent that called you cannot see
your tool calls, your reasoning, or anything you read — only the last message
you write. If a fact is not in that message, it is lost.

%s

WRITING THE RESULT
- Be concrete: exact file paths, line numbers, symbol names, short quotes.
  "The config is loaded somewhere in internal" is useless.
  "config.Load reads FORGE_CONFIG, then ./forge.json (internal/config/config.go:171)"
  is what the caller needs.
- Do not describe your search. Report what you found.
- If you could not determine something, say so plainly. Do not guess.
- Under %d words.`, spec.Name, spec.Role, spec.MaxWords)
}

func clampWords(s string, max int) string {
	if max <= 0 {
		return s
	}
	words := strings.Fields(s)
	if len(words) <= max {
		return s
	}
	return strings.Join(words[:max], " ") + fmt.Sprintf("\n\n[truncated at %d words]", max)
}

func firstLine(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}
