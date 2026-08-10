// Package agent implements the ReAct loop: the model proposes an action, the
// action runs, the result goes back into context, repeat until it stops asking
// for actions.
//
// There is no framework here on purpose. The loop is about forty lines; what
// actually determines whether an agent works is everything around it — the
// tool contracts, the failure feedback, the stop conditions, and the budget.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/VEER-TARGARYEN/forge/internal/approval"
	"github.com/VEER-TARGARYEN/forge/internal/compact"
	"github.com/VEER-TARGARYEN/forge/internal/provider"
	"github.com/VEER-TARGARYEN/forge/internal/router"
	"github.com/VEER-TARGARYEN/forge/internal/tools"
)

type Config struct {
	Class       string
	MaxSteps    int
	MaxTokens   int // total budget for the run; 0 means unlimited
	Temperature float64
	Protocol    EditProtocol
	// Quiet suppresses streaming of the model's prose.
	Quiet bool

	// RepoMap is a prebuilt repository map appended to the system prompt.
	// The caller builds it so the agent stays independent of the scanner,
	// and so it is computed exactly once per run — a map that changed every
	// turn would invalidate the prompt prefix on every turn.
	RepoMap string
	// Notes is durable context carried in from previous sessions.
	Notes string
	// SubRole, when set, replaces the top-level workflow instructions with a
	// sub-agent's role brief. It is what makes a delegated context behave like
	// a specialist rather than a second full agent.
	SubRole string

	// ContextBudget is the window to compact against. 0 means ask the router
	// what the class can hold.
	ContextBudget int
	// CompactAt is the fraction of the budget at which compaction triggers.
	CompactAt float64
	// CompactClass routes the summarisation call. Default "cheap".
	CompactClass string
	// KeepTail is how many recent messages survive compaction verbatim.
	KeepTail int

	// Verify runs the project's checks. Nil disables the self-repair loop.
	Verify VerifyFunc
	// MaxRepairs bounds how many times a failing verification is handed back
	// for another attempt. Beyond a few rounds a model that has not fixed it
	// is usually thrashing, and each round costs a full test run.
	MaxRepairs int

	// SubStats reports tokens and delegation count spent by sub-agents, so
	// the run's accounting includes work done in contexts the parent never
	// saw. Without it a delegating run would under-report its real cost.
	SubStats func() (provider.Usage, int)

	// Observer receives progress notifications for a live display. Nil is
	// fine — the loop is fully functional with nothing watching it.
	Observer Observer
}

// Observer is notified as a run progresses, so a renderer can show live state
// without the agent knowing anything about terminals.
type Observer interface {
	SetStep(n int)
	SetActivity(format string, args ...any)
	AddUsage(provider, model string, prompt, completion int)
	SetCounts(subAgents, changed int)
}

// notify is a nil-safe Observer accessor, so call sites stay uncluttered.
func (a *Agent) notify() Observer {
	if a.cfg.Observer == nil {
		return nopObserver{}
	}
	return a.cfg.Observer
}

type nopObserver struct{}

func (nopObserver) SetStep(int)                       {}
func (nopObserver) SetActivity(string, ...any)        {}
func (nopObserver) AddUsage(string, string, int, int) {}
func (nopObserver) SetCounts(int, int)                {}

// VerifyFunc runs the project's checks and reports the summary, whether it
// passed, and how many located problems there were.
type VerifyFunc func(ctx context.Context) (summary string, passed bool, problems int, err error)

func (c *Config) applyDefaults() {
	if c.Class == "" {
		c.Class = "coder"
	}
	if c.MaxSteps <= 0 {
		c.MaxSteps = 30
	}
	if c.Protocol == "" {
		c.Protocol = ProtoAuto
	}
	if c.CompactAt <= 0 || c.CompactAt >= 1 {
		c.CompactAt = 0.7
	}
	if c.CompactClass == "" {
		c.CompactClass = "cheap"
	}
	if c.KeepTail <= 0 {
		c.KeepTail = 6
	}
	if c.MaxRepairs < 0 {
		c.MaxRepairs = 0
	}
	if c.MaxRepairs == 0 {
		c.MaxRepairs = 3
	}
}

type Agent struct {
	rt   *router.Router
	reg  *tools.Registry
	env  *tools.Env
	cfg  Config
	out  io.Writer
	msgs []provider.Message

	usage       provider.Usage
	changed     map[string]bool
	toolSeen    map[string]int
	compactions int
	tokensSaved int
}

// Outcome summarizes a completed run.
type Outcome struct {
	Steps        int
	Usage        provider.Usage
	FinalText    string
	FilesChanged []string
	StopReason   string
	Elapsed      time.Duration
	Compactions  int
	TokensSaved  int

	// Verified is true only when verification ran and passed. It stays false
	// when no checks were configured, so "not verified" is never mistaken for
	// "verified clean".
	Verified      bool
	VerifyRan     bool
	VerifySummary string
	Repairs       int

	// SubAgents counts delegations; SubUsage is the tokens they spent, which
	// is already included in Usage.
	SubAgents int
	SubUsage  provider.Usage
}

func New(rt *router.Router, reg *tools.Registry, env *tools.Env, cfg Config, out io.Writer) *Agent {
	cfg.applyDefaults()
	a := &Agent{
		rt: rt, reg: reg, env: env, cfg: cfg, out: out,
		changed:  map[string]bool{},
		toolSeen: map[string]int{},
	}
	// Tools report their own mutations rather than the agent inferring them
	// from arguments, so a partial or redirected write is still recorded.
	env.Changed = func(path string) { a.changed[path] = true }

	// One system message holding everything invariant. Keeping the prefix
	// byte-identical across turns is what lets llama.cpp reuse its KV cache;
	// on a CPU-bound local model that turns a multi-minute prefill into a
	// near-instant one, so nothing volatile may go in here.
	var sys strings.Builder
	if cfg.SubRole != "" {
		sys.WriteString(SubSystem(env.WS, reg, cfg.SubRole))
	} else {
		sys.WriteString(System(env.WS, reg, cfg.Protocol))
	}
	if cfg.Notes != "" {
		sys.WriteString("\n\nNOTES FROM EARLIER SESSIONS\n")
		sys.WriteString(cfg.Notes)
		sys.WriteString("\n")
	}
	if cfg.RepoMap != "" {
		sys.WriteString("\n\n")
		sys.WriteString(cfg.RepoMap)
	}
	a.msgs = []provider.Message{{Role: provider.RoleSystem, Content: sys.String()}}
	return a
}

// contextBudget resolves the window to compact against.
func (a *Agent) contextBudget() int {
	b := a.cfg.ContextBudget
	if b <= 0 {
		b = a.rt.ClassContext(a.cfg.Class)
	}
	if b <= 0 {
		b = 32000 // a local 7B's practical window; the safe assumption
	}
	// Past this, compaction is about latency and cost rather than fitting,
	// and summarising a 500k-token conversation is its own expensive problem.
	if b > 200000 {
		b = 200000
	}
	return b
}

// compactClass picks where summarisation runs.
//
// The configured class is preferred because summarising is grunt work not
// worth a good model, but falling back matters more than saving money: a
// compaction that cannot run means no context relief at all, and the run hits
// the window instead.
func (a *Agent) compactClass() string {
	if a.rt.ClassUsable(a.cfg.CompactClass) {
		return a.cfg.CompactClass
	}
	return a.cfg.Class
}

// maybeCompact collapses the conversation when it approaches the budget.
func (a *Agent) maybeCompact(ctx context.Context, force bool) {
	budget := a.contextBudget()
	est := provider.Request{Messages: a.msgs, Tools: a.specs()}.TokenEstimate()
	threshold := int(float64(budget) * a.cfg.CompactAt)
	if !force && est < threshold {
		return
	}

	a.printf("  ⋯ compacting: ~%d tok vs %d budget\n", est, budget)
	res, err := compact.Run(ctx, a.rt, a.msgs, compact.Options{
		Class:    a.compactClass(),
		KeepTail: a.cfg.KeepTail,
	})
	if err != nil {
		// A failed compaction is not fatal: the call may still fit, and if it
		// does not the provider will say so more precisely than we can.
		a.printf("  ! compaction failed: %v\n", err)
		return
	}
	if res.Collapsed == 0 {
		return
	}
	a.msgs = res.Messages
	a.compactions++
	a.tokensSaved += res.BeforeTokens - res.AfterTokens
	a.printf("  ⋯ collapsed %d messages: ~%d → ~%d tok\n",
		res.Collapsed, res.BeforeTokens, res.AfterTokens)
}

// Messages exposes the conversation, for inspection and for Phase 3 to
// compact.
func (a *Agent) Messages() []provider.Message { return a.msgs }

func (a *Agent) specs() []provider.Tool {
	specs := a.reg.Specs()
	out := make([]provider.Tool, 0, len(specs))
	for _, s := range specs {
		// In pure-blocks mode edit_file is hidden, so a weak model is not
		// tempted into the JSON path it handles badly.
		if a.cfg.Protocol == ProtoBlocks && s.Name == "edit_file" {
			continue
		}
		out = append(out, provider.Tool{
			Type: "function",
			Function: provider.ToolFunction{
				Name: s.Name, Description: s.Description, Parameters: s.Schema,
			},
		})
	}
	return out
}

func (a *Agent) Run(ctx context.Context, task string) (*Outcome, error) {
	start := time.Now()
	a.msgs = append(a.msgs, provider.Message{Role: provider.RoleUser, Content: task})

	outcome := &Outcome{}
	for step := 1; step <= a.cfg.MaxSteps; step++ {
		outcome.Steps = step

		if a.cfg.MaxTokens > 0 && a.usage.TotalTokens >= a.cfg.MaxTokens {
			outcome.StopReason = fmt.Sprintf("token budget exhausted (%d)", a.usage.TotalTokens)
			break
		}
		if err := ctx.Err(); err != nil {
			outcome.StopReason = "cancelled"
			break
		}

		a.printf("\n─── step %d ───\n", step)
		a.notify().SetStep(step)

		// Proactive: stay under the window rather than discovering we are over.
		a.notify().SetActivity("checking context budget")
		a.maybeCompact(ctx, false)

		a.notify().SetActivity("waiting on the model")
		resp, err := a.callModel(ctx)
		if err != nil {
			// Reactive: every target rejected the request as too long. That is
			// exactly the signal compaction exists for, so collapse and retry
			// once before giving up.
			if provider.KindOf(err) == provider.ErrContextLength {
				a.printf("  ! context overflow; compacting and retrying\n")
				before := a.compactions
				a.maybeCompact(ctx, true)
				if a.compactions > before {
					resp, err = a.callModel(ctx)
				}
			}
			if err != nil {
				return a.finish(outcome, start, "model call failed"), err
			}
		}

		a.usage.PromptTokens += resp.Usage.PromptTokens
		a.usage.CompletionTokens += resp.Usage.CompletionTokens
		a.usage.TotalTokens += resp.Usage.TotalTokens
		a.printf("\n  [%s/%s  %d+%d tok]\n", resp.Provider, resp.Model,
			resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
		a.notify().AddUsage(resp.Provider, resp.Model,
			resp.Usage.PromptTokens, resp.Usage.CompletionTokens)

		a.msgs = append(a.msgs, provider.Message{
			Role:      provider.RoleAssistant,
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})

		if len(resp.ToolCalls) > 0 {
			stop, err := a.runToolCalls(ctx, resp.ToolCalls)
			if err != nil {
				return a.finish(outcome, start, "aborted"), err
			}
			if stop != "" {
				outcome.StopReason = stop
				break
			}
			continue
		}

		// No tool calls: the edit may still be in the message content.
		blocks, problems := tools.ParseBlocks(resp.Content)
		if len(blocks) > 0 {
			results := tools.ApplyBlocks(blocks, a.env)
			for _, r := range results {
				if r.OK {
					a.printf("  ✓ %s  %s\n", r.Block.Path, r.Message)
				} else {
					a.printf("  ✗ %s  %s\n", r.Block.Path, r.Message)
				}
			}
			if abortedIn(results) {
				outcome.StopReason = "aborted by user"
				break
			}
			a.msgs = append(a.msgs, provider.Message{
				Role: provider.RoleUser, Content: tools.FormatBlockResults(results),
			})
			continue
		}
		if len(problems) > 0 {
			// Malformed blocks are recoverable: tell the model exactly what
			// was wrong rather than treating the turn as a final answer.
			a.printf("  ! %d malformed edit block(s)\n", len(problems))
			a.msgs = append(a.msgs, provider.Message{
				Role: provider.RoleUser,
				Content: "Your edit blocks were malformed:\n" + strings.Join(problems, "\n") +
					"\n\nResend them in the exact SEARCH/REPLACE format.",
			})
			continue
		}

		// The model believes it is finished. Before accepting that, run the
		// project's own checks — a model's confidence is not evidence.
		if done, reason := a.settle(ctx, outcome, resp); done {
			outcome.StopReason = reason
			break
		}
	}

	if outcome.StopReason == "" {
		outcome.StopReason = fmt.Sprintf("hit step limit (%d)", a.cfg.MaxSteps)
	}
	return a.finish(outcome, start, outcome.StopReason), nil
}

// settle decides whether a turn with no actions really ends the run.
//
// It returns (true, reason) to stop, or (false, "") to hand a failing
// verification back to the model for another attempt. Verification is skipped
// when nothing was changed: there is nothing to have broken, and a full test
// run to confirm that is pure latency.
func (a *Agent) settle(ctx context.Context, outcome *Outcome, resp *provider.Response) (bool, string) {
	outcome.FinalText = strings.TrimSpace(resp.Content)

	if a.cfg.Verify == nil || len(a.changed) == 0 {
		return true, "done"
	}

	a.printf("  ⋯ verifying %d changed file(s)\n", len(a.changed))
	a.notify().SetActivity("running the project's checks")
	summary, passed, problems, err := a.cfg.Verify(ctx)
	if err != nil {
		// A verification that cannot run is not a failed verification. Say so
		// and stop, rather than sending the model to chase a phantom bug.
		a.printf("  ! verification could not run: %v\n", err)
		return true, "done (verification unavailable)"
	}
	outcome.VerifyRan = true
	outcome.VerifySummary = summary
	outcome.Verified = passed

	if passed {
		a.printf("  ✓ verification passed\n")
		return true, "done"
	}

	if outcome.Repairs >= a.cfg.MaxRepairs {
		a.printf("  ✗ verification still failing after %d repair attempt(s)\n", outcome.Repairs)
		return true, fmt.Sprintf("verification failed after %d repair attempt(s)", outcome.Repairs)
	}
	outcome.Repairs++
	a.printf("  ✗ verification failed (%d problem(s)); repair attempt %d of %d\n",
		problems, outcome.Repairs, a.cfg.MaxRepairs)

	a.msgs = append(a.msgs, provider.Message{
		Role: provider.RoleUser,
		Content: summary + "\n\nFix these. Read the exact lines named above before editing — " +
			"do not guess at the cause. If a failure is pre-existing and unrelated to your " +
			"change, say so explicitly instead of trying to fix it.",
	})
	return false, ""
}

// callModel issues one streaming turn and prints the model's prose.
func (a *Agent) callModel(ctx context.Context) (*provider.Response, error) {
	printer := newStreamPrinter(a.out, a.cfg.Quiet)
	resp, err := a.rt.Stream(ctx, a.cfg.Class, provider.Request{
		Messages:    a.msgs,
		Tools:       a.specs(),
		Temperature: a.cfg.Temperature,
	}, func(c provider.Chunk) error {
		printer.write(c.Content)
		return nil
	})
	printer.flush()
	return resp, err
}

func (a *Agent) finish(o *Outcome, start time.Time, reason string) *Outcome {
	o.Usage = a.usage
	if a.cfg.SubStats != nil {
		sub, n := a.cfg.SubStats()
		o.SubAgents, o.SubUsage = n, sub
		// Sub-agent tokens are real spend and belong in the total, even though
		// none of them ever entered this conversation.
		o.Usage.PromptTokens += sub.PromptTokens
		o.Usage.CompletionTokens += sub.CompletionTokens
		o.Usage.TotalTokens += sub.TotalTokens
	}
	o.Compactions = a.compactions
	o.TokensSaved = a.tokensSaved
	o.Elapsed = time.Since(start)
	if o.StopReason == "" {
		o.StopReason = reason
	}
	for p := range a.changed {
		o.FilesChanged = append(o.FilesChanged, p)
	}
	sort.Strings(o.FilesChanged)
	return o
}

// runToolCalls executes each requested call and appends its result. It returns
// a non-empty stop reason when the run should end.
//
// Parallel-safe calls — currently only delegation — run concurrently, while
// everything that touches the filesystem or the approver stays sequential.
// Results are always appended in the order the model requested them, because
// providers expect tool results to line up with their tool_calls.
func (a *Agent) runToolCalls(ctx context.Context, calls []provider.ToolCall) (string, error) {
	results := make([]*tools.Result, len(calls))
	stop := ""

	var wg sync.WaitGroup
	var mu sync.Mutex
	parallel := 0

	for i, tc := range calls {
		name := tc.Function.Name
		args := json.RawMessage(tc.Function.Arguments)

		t, ok := a.reg.Get(name)
		if !ok {
			a.printf("  → %s(%s)\n", name, summarizeArgs(args))
			results[i] = tools.Errorf(
				"no tool named %q. Available tools: %s", name, strings.Join(a.reg.Names(), ", "))
			continue
		}

		// A model stuck in a loop will re-issue an identical failing call
		// forever. Detect it and say so, rather than burning the step budget.
		key := name + "\x00" + string(args)
		a.toolSeen[key]++
		if a.toolSeen[key] == 4 {
			a.printf("  → %s(%s)\n", name, summarizeArgs(args))
			results[i] = tools.Errorf(
				"you have made this exact call %d times. It will not start working. "+
					"Change the arguments, use a different tool, or explain what is blocking you.",
				a.toolSeen[key])
			continue
		}

		if tools.IsParallelSafe(t) && len(calls) > 1 {
			parallel++
			a.printf("  ⇉ %s(%s)\n", name, summarizeArgs(args))
			wg.Add(1)
			go func(i int, tc provider.ToolCall, t tools.Tool, args json.RawMessage) {
				defer wg.Done()
				res, err := t.Run(ctx, args, a.env)
				mu.Lock()
				defer mu.Unlock()
				results[i] = normalizeResult(tc.Function.Name, res, err)
			}(i, tc, t, args)
			continue
		}

		a.printf("  → %s(%s)\n", name, summarizeArgs(args))
		a.notify().SetActivity("%s", name)
		res, err := t.Run(ctx, args, a.env)
		if err != nil {
			var aborted *approval.Aborted
			if errors.As(err, &aborted) {
				stop = "aborted by user"
				break
			}
			if errors.Is(err, context.Canceled) {
				stop = "cancelled"
				break
			}
		}
		results[i] = normalizeResult(name, res, err)
		a.printf("  %s %s\n", mark(results[i]), firstLines(results[i].ForHuman(), 6))
	}

	wg.Wait()
	if parallel > 1 {
		a.printf("  ⇉ %d delegations ran concurrently\n", parallel)
	}
	if a.cfg.SubStats != nil {
		_, n := a.cfg.SubStats()
		a.notify().SetCounts(n, len(a.changed))
	} else {
		a.notify().SetCounts(0, len(a.changed))
	}

	for i, tc := range calls {
		if results[i] == nil {
			continue // a call skipped because an earlier one aborted the run
		}
		if tools.IsParallelSafe(mustTool(a.reg, tc.Function.Name)) && len(calls) > 1 {
			a.printf("  %s %s\n", mark(results[i]), firstLines(results[i].ForHuman(), 6))
		}
		a.appendToolResult(tc, results[i])
	}
	return stop, nil
}

func normalizeResult(name string, res *tools.Result, err error) *tools.Result {
	if err != nil {
		return tools.Errorf("%s failed: %v", name, err)
	}
	if res == nil {
		return tools.Errorf("%s returned no result", name)
	}
	return res
}

func mark(r *tools.Result) string {
	if r.IsError {
		return "✗"
	}
	return "✓"
}

// mustTool returns a tool or a nil-safe placeholder, so display logic never
// panics on a name that failed lookup earlier in the same batch.
func mustTool(reg *tools.Registry, name string) tools.Tool {
	if t, ok := reg.Get(name); ok {
		return t
	}
	return nil
}

func (a *Agent) appendToolResult(tc provider.ToolCall, res *tools.Result) {
	a.msgs = append(a.msgs, provider.Message{
		Role:       provider.RoleTool,
		ToolCallID: tc.ID,
		Name:       tc.Function.Name,
		Content:    res.ForModel(),
	})
}

func abortedIn(rs []tools.BlockResult) bool {
	for _, r := range rs {
		if !r.OK && strings.Contains(r.Message, "aborted") {
			return true
		}
	}
	return false
}

func (a *Agent) printf(format string, args ...any) {
	if a.out == nil {
		return
	}
	fmt.Fprintf(a.out, format, args...)
}

// summarizeArgs renders tool arguments as a short call signature, so the
// transcript stays readable when an argument is a whole file.
func summarizeArgs(raw json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return truncate(strings.TrimSpace(string(raw)), 60)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// Lead with the identifying argument rather than whatever sorts first.
	for _, lead := range []string{"path", "pattern", "command"} {
		for i, k := range keys {
			if k == lead && i != 0 {
				keys[0], keys[i] = keys[i], keys[0]
			}
		}
	}
	var parts []string
	for i, k := range keys {
		if i == 2 {
			parts = append(parts, "…")
			break
		}
		parts = append(parts, fmt.Sprintf("%s=%s", k, truncate(fmt.Sprint(m[k]), 40)))
	}
	return strings.Join(parts, ", ")
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", "⏎")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n     ")
	}
	return strings.Join(lines[:n], "\n     ") + fmt.Sprintf("\n     … (%d more lines)", len(lines)-n)
}

// ---------- streaming display ----------

// streamPrinter echoes the model's prose but hides the interior of
// SEARCH/REPLACE blocks. A 200-line edit streaming past twice — once as raw
// text, once as the approval diff — is noise; the diff is the useful copy.
type streamPrinter struct {
	out     io.Writer
	quiet   bool
	buf     strings.Builder
	inBlock bool
	shown   bool
}

func newStreamPrinter(out io.Writer, quiet bool) *streamPrinter {
	return &streamPrinter{out: out, quiet: quiet}
}

func (p *streamPrinter) write(s string) {
	if s == "" || p.out == nil || p.quiet {
		return
	}
	p.buf.WriteString(s)
	text := p.buf.String()
	for {
		i := strings.IndexByte(text, '\n')
		if i < 0 {
			break
		}
		p.emit(text[:i])
		text = text[i+1:]
	}
	p.buf.Reset()
	p.buf.WriteString(text)
}

func (p *streamPrinter) emit(line string) {
	t := strings.TrimSpace(line)
	switch {
	case strings.HasPrefix(t, "<<<<<<<"):
		p.inBlock = true
		if !p.shown {
			fmt.Fprintf(p.out, "  ⟨edit block⟩\n")
			p.shown = true
		}
		return
	case p.inBlock && strings.HasPrefix(t, ">>>>>>>"):
		p.inBlock = false
		p.shown = false
		return
	case p.inBlock:
		return
	}
	fmt.Fprintln(p.out, line)
}

func (p *streamPrinter) flush() {
	if p.out == nil || p.quiet {
		return
	}
	if rest := p.buf.String(); rest != "" && !p.inBlock {
		fmt.Fprintln(p.out, rest)
	}
	p.buf.Reset()
}
