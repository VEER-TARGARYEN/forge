// Package tools defines the capabilities the agent can invoke and the safety
// boundary around them. Every tool is confined to a workspace root, and every
// mutating tool passes through an approver before it touches anything.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Result is what a tool hands back. Content goes into the model's context;
// Display is what the human sees. They differ on purpose — the model wants
// "edited main.go: +3 -1", the human wants the diff.
//
// Oversized results are clipped by Env.Clip before they get here, with the
// remainder parked in the overflow store and a retrieval handle appended to
// Content. Nothing downstream needs to know that happened.
type Result struct {
	Content string
	Display string
	IsError bool
}

func (r *Result) ForModel() string { return r.Content }

func (r *Result) ForHuman() string {
	if r.Display != "" {
		return r.Display
	}
	return r.Content
}

func Errorf(format string, a ...any) *Result {
	return &Result{Content: fmt.Sprintf(format, a...), IsError: true}
}

func Textf(format string, a ...any) *Result {
	return &Result{Content: fmt.Sprintf(format, a...)}
}

// Spec is the model-facing declaration of a tool. The agent converts these to
// provider.Tool; keeping the type local means this package does not depend on
// the wire format.
type Spec struct {
	Name        string
	Description string
	Schema      map[string]any
}

type Tool interface {
	Spec() Spec
	// Mutates reports whether invoking this tool can change state. Read-only
	// tools bypass the approver entirely.
	Mutates() bool
	Run(ctx context.Context, args json.RawMessage, env *Env) (*Result, error)
}

// ApprovalRequest is what the human is asked to authorize.
type ApprovalRequest struct {
	Tool    string
	Kind    string // "write" | "edit" | "command"
	Summary string // one line
	Detail  string // diff, or the command text
	Path    string
	// Risky marks operations that are hard to undo. Approvers may require a
	// stronger confirmation for these even in permissive modes.
	Risky bool
}

type Approver interface {
	Approve(req ApprovalRequest) error
}

// Env is the execution context handed to every tool.
type Env struct {
	WS       *Workspace
	Approver Approver
	Out      io.Writer
	// MaxBytes caps how much any single tool result may put into context.
	// Context is the scarcest resource in an agent loop; an unbounded grep
	// can consume an entire window in one call.
	MaxBytes int
	// Todos is shared mutable state for the todo tools.
	Todos *TodoList
	// Changed is called with the workspace-relative path of every file a tool
	// actually modified. Tools report this themselves rather than the caller
	// inferring it from arguments, so a declined or failed write is never
	// counted as a change.
	Changed func(path string)
	// Overflow holds the full text of clipped results for later retrieval.
	Overflow *Overflow
	// NotesFile is where durable cross-session notes are appended. Empty
	// disables the remember tool.
	NotesFile string
	// SearchCode backs the search_code tool. Nil makes the tool report that
	// search is unavailable rather than failing the call.
	SearchCode SearchFunc
	// Verify backs the verify tool. Nil disables it.
	Verify VerifyFunc
	// Snapshot is called with a file's contents immediately BEFORE it is
	// modified, so the change can be undone. It is separate from Changed,
	// which fires after: an approval that gets declined must record neither.
	Snapshot func(path string, original []byte, existed bool)
	// Spawn delegates to a sub-agent. Nil disables the task tool, which is
	// also how sub-agents are prevented from delegating further.
	Spawn SpawnFunc
}

// ParallelSafe is implemented by tools whose calls may run concurrently with
// each other. Everything that touches the filesystem or the approver stays
// sequential; only delegation opts in.
type ParallelSafe interface {
	ParallelSafe() bool
}

// IsParallelSafe reports whether a tool opted into concurrent execution.
func IsParallelSafe(t Tool) bool {
	p, ok := t.(ParallelSafe)
	return ok && p.ParallelSafe()
}

func (e *Env) noteSnapshot(path string, original []byte, existed bool) {
	if e.Snapshot != nil {
		e.Snapshot(path, original, existed)
	}
}

func (e *Env) noteChange(path string) {
	if e.Changed != nil {
		e.Changed(path)
	}
}

func (e *Env) cap() int {
	if e.MaxBytes <= 0 {
		return 30000
	}
	return e.MaxBytes
}

// Clip lives in store.go, next to the overflow store it depends on.

type Registry struct {
	byName map[string]Tool
	order  []string
}

func NewRegistry() *Registry {
	return &Registry{byName: map[string]Tool{}}
}

func (r *Registry) Register(ts ...Tool) {
	for _, t := range ts {
		name := t.Spec().Name
		if _, dup := r.byName[name]; !dup {
			r.order = append(r.order, name)
		}
		r.byName[name] = t
	}
}

func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.byName[name]
	return t, ok
}

// Has reports whether a name is registered. Callers that only need to
// validate a name — recovering a tool call from message text, for one —
// should not have to hold a Tool they will never invoke.
func (r *Registry) Has(name string) bool {
	_, ok := r.byName[name]
	return ok
}

func (r *Registry) Specs() []Spec {
	out := make([]Spec, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.byName[n].Spec())
	}
	return out
}

func (r *Registry) Names() []string {
	out := append([]string(nil), r.order...)
	sort.Strings(out)
	return out
}

// ---------- argument parsing ----------

// ParseArgs decodes tool arguments defensively. Small models produce valid
// intent wrapped in invalid JSON often enough that a strict decoder throws
// away usable calls: double-encoded strings, markdown fences, and leading
// prose are all common. Recovering costs one function; not recovering costs a
// whole round trip on a model that generates at 7 tok/s.
func ParseArgs(raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		return json.Unmarshal([]byte("{}"), dst)
	}
	if err := json.Unmarshal(raw, dst); err == nil {
		return nil
	}

	s := strings.TrimSpace(string(raw))

	// Double-encoded: the arguments field is a JSON string holding JSON.
	if len(s) > 1 && s[0] == '"' {
		var inner string
		if err := json.Unmarshal([]byte(s), &inner); err == nil {
			if err := json.Unmarshal([]byte(inner), dst); err == nil {
				return nil
			}
			s = inner
		}
	}

	// Fenced: ```json { ... } ```
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
		s = strings.TrimSpace(s)
		if err := json.Unmarshal([]byte(s), dst); err == nil {
			return nil
		}
	}

	// Prose around an object: take the outermost balanced braces.
	if obj := outermostObject(s); obj != "" {
		if err := json.Unmarshal([]byte(obj), dst); err == nil {
			return nil
		}
	}

	return fmt.Errorf("could not parse arguments as JSON: %s", truncateStr(s, 200))
}

// outermostObject returns the span from the first '{' to its matching '}',
// ignoring braces inside string literals.
func outermostObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case c == '\\' && inStr:
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
			// braces inside strings are data, not structure
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------- schema helpers ----------

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		m["required"] = required
	} else {
		m["required"] = []string{}
	}
	m["additionalProperties"] = false
	return m
}

func str(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func integer(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func boolean(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}
