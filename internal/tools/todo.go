package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// TodoList is the agent's working plan for a multi-step task.
//
// It is not decoration. A small model loses the thread of a five-step task
// once tool results push the original instruction out of recent attention;
// re-stating the checklist every turn keeps the goal in view for a fraction of
// the tokens that re-reading the conversation would cost.
type TodoList struct {
	mu    sync.Mutex
	items []TodoItem
}

type TodoItem struct {
	Content string `json:"content"`
	Status  string `json:"status"` // pending | in_progress | done
}

func NewTodoList() *TodoList { return &TodoList{} }

func (t *TodoList) Set(items []TodoItem) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.items = items
}

func (t *TodoList) Items() []TodoItem {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]TodoItem(nil), t.items...)
}

func (t *TodoList) Render() string {
	items := t.Items()
	if len(items) == 0 {
		return "(no plan set)"
	}
	var sb strings.Builder
	for i, it := range items {
		mark := " "
		switch it.Status {
		case "done":
			mark = "x"
		case "in_progress":
			mark = ">"
		}
		fmt.Fprintf(&sb, "[%s] %d. %s\n", mark, i+1, it.Content)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// Remaining reports how many items are not yet done.
func (t *TodoList) Remaining() int {
	n := 0
	for _, it := range t.Items() {
		if it.Status != "done" {
			n++
		}
	}
	return n
}

type TodoWrite struct{}

func (TodoWrite) Spec() Spec {
	return Spec{
		Name: "todo_write",
		Description: "Record or update the plan for a multi-step task. Send the complete list every " +
			"time, with exactly one item marked in_progress. Use this for anything needing three or " +
			"more steps; skip it for single-step tasks.",
		Schema: obj(map[string]any{
			"todos": map[string]any{
				"type":        "array",
				"description": "The full checklist, in order.",
				"items": obj(map[string]any{
					"content": str("What this step does."),
					"status": map[string]any{
						"type": "string", "enum": []string{"pending", "in_progress", "done"},
						"description": "Step state.",
					},
				}, "content", "status"),
			},
		}, "todos"),
	}
}

func (TodoWrite) Mutates() bool { return false }

func (TodoWrite) Run(ctx context.Context, raw json.RawMessage, env *Env) (*Result, error) {
	var a struct {
		Todos []TodoItem `json:"todos"`
	}
	if err := ParseArgs(raw, &a); err != nil {
		return Errorf("%v", err), nil
	}
	if len(a.Todos) == 0 {
		return Errorf("todos must contain at least one item"), nil
	}
	inProgress := 0
	for i, it := range a.Todos {
		switch it.Status {
		case "pending", "in_progress", "done":
		case "":
			a.Todos[i].Status = "pending"
		default:
			return Errorf("item %d has status %q; use pending, in_progress, or done", i+1, it.Status), nil
		}
		if a.Todos[i].Status == "in_progress" {
			inProgress++
		}
	}
	if inProgress > 1 {
		return Errorf("%d items are in_progress; exactly one may be", inProgress), nil
	}
	env.Todos.Set(a.Todos)
	rendered := env.Todos.Render()
	return &Result{
		Content: "Plan updated:\n" + rendered,
		Display: rendered,
	}, nil
}
