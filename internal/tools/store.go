package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Overflow holds the full text of tool results that were too large to put in
// context, so the model can pull the parts it actually needs.
//
// This is the difference between a grep that costs 12,000 tokens and one that
// costs 300. Clipping alone loses information; clipping with a handle keeps it
// retrievable at the model's discretion, which is the whole point — the model
// knows which 40 lines matter and the truncator does not.
type Overflow struct {
	mu    sync.Mutex
	dir   string
	seq   int
	items map[string][]string
}

func NewOverflow(dir string) *Overflow {
	return &Overflow{dir: dir, items: map[string][]string{}}
}

// Put stores content and returns a short handle. Also written to disk when a
// directory is configured, so a run can be inspected after the fact.
func (o *Overflow) Put(label, content string) (id string, totalLines int) {
	if o == nil {
		return "", 0
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seq++
	id = fmt.Sprintf("r%d", o.seq)
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	o.items[id] = lines

	if o.dir != "" {
		if err := os.MkdirAll(o.dir, 0o755); err == nil {
			meta, _ := json.Marshal(map[string]any{"id": id, "label": label, "lines": len(lines)})
			_ = os.WriteFile(filepath.Join(o.dir, id+".txt"),
				append(append(meta, '\n'), []byte(content)...), 0o600)
		}
	}
	return id, len(lines)
}

// Slice returns a window of a stored result.
func (o *Overflow) Slice(id string, offset, limit int) (string, int, error) {
	if o == nil {
		return "", 0, fmt.Errorf("no results are stored")
	}
	o.mu.Lock()
	lines, ok := o.items[id]
	o.mu.Unlock()
	if !ok {
		return "", 0, fmt.Errorf("no stored result with id %q", id)
	}
	total := len(lines)
	start := 0
	if offset > 0 {
		start = offset - 1
	}
	if start >= total {
		return "", total, fmt.Errorf("id %q has %d lines; offset %d is past the end", id, total, offset)
	}
	if limit <= 0 {
		limit = 200
	}
	end := start + limit
	if end > total {
		end = total
	}
	var sb strings.Builder
	for i := start; i < end; i++ {
		fmt.Fprintf(&sb, "%6d\t%s\n", i+1, lines[i])
	}
	return sb.String(), total, nil
}

// Clip trims content to the per-result budget. When it does not fit, the full
// text is parked in the overflow store and the returned note tells the model
// exactly how to retrieve the rest.
func (e *Env) Clip(label, content string) (shown, note string) {
	limit := e.cap()
	if len(content) <= limit {
		return content, ""
	}
	cut := content[:limit]
	if i := strings.LastIndexByte(cut, '\n'); i > limit/2 {
		cut = cut[:i+1]
	}
	shownLines := strings.Count(cut, "\n")

	id, total := e.Overflow.Put(label, content)
	if id == "" {
		return cut, fmt.Sprintf("\n[truncated after %d of %d lines]\n", shownLines, strings.Count(content, "\n"))
	}
	return cut, fmt.Sprintf(
		"\n[showing lines 1-%d of %d. Call expand(id=%q, offset=%d) for the rest.]\n",
		shownLines, total, id, shownLines+1)
}

// ---------- expand tool ----------

type Expand struct{}

func (Expand) Spec() Spec {
	return Spec{
		Name: "expand",
		Description: "Read more of a tool result that was truncated. Pass the id from the " +
			"truncation notice. Cheaper than re-running the original tool with a wider limit.",
		Schema: obj(map[string]any{
			"id":     str("The result id from a truncation notice, e.g. 'r3'."),
			"offset": integer("1-based line to start at."),
			"limit":  integer("Maximum lines to return. Defaults to 200."),
		}, "id"),
	}
}

func (Expand) Mutates() bool { return false }

func (Expand) Run(ctx context.Context, raw json.RawMessage, env *Env) (*Result, error) {
	var a struct {
		ID     string `json:"id"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := ParseArgs(raw, &a); err != nil {
		return Errorf("%v", err), nil
	}
	body, total, err := env.Overflow.Slice(a.ID, a.Offset, a.Limit)
	if err != nil {
		return Errorf("%v", err), nil
	}
	start := a.Offset
	if start <= 0 {
		start = 1
	}
	end := start + strings.Count(body, "\n") - 1
	out := fmt.Sprintf("%s lines %d-%d of %d:\n%s", a.ID, start, end, total, body)
	if end < total {
		out += fmt.Sprintf("\n[%d more lines. expand(id=%q, offset=%d)]\n", total-end, a.ID, end+1)
	}
	return &Result{
		Content: out,
		Display: fmt.Sprintf("expand %s lines %d-%d of %d", a.ID, start, end, total),
	}, nil
}
