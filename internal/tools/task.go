package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// SpawnRequest asks for one delegation.
type SpawnRequest struct {
	Agent string
	Task  string
}

// SpawnResult is what comes back. Summary is the sub-agent's final message and
// the only part the caller ever sees.
type SpawnResult struct {
	Agent            string
	Summary          string
	Steps            int
	PromptTokens     int
	CompletionTokens int
	StopReason       string
}

type SpawnFunc func(ctx context.Context, req SpawnRequest) (*SpawnResult, error)

// AgentInfo describes one delegable role to the model.
type AgentInfo struct {
	Name        string
	Description string
}

// Task delegates work to a sub-agent running in its own context.
//
// The value is asymmetric and worth stating plainly in the tool description:
// the sub-agent may burn fifteen thousand tokens searching, and the caller
// pays only for the paragraph that comes back. A model that understands that
// will delegate exploration instead of doing it inline.
type Task struct {
	Agents []AgentInfo
}

func (t Task) Spec() Spec {
	var b strings.Builder
	b.WriteString("Delegate a self-contained piece of work to a sub-agent with its own context. " +
		"Only the sub-agent's final report comes back — everything it reads stays out of this " +
		"conversation, so a long search costs you a paragraph instead of thousands of tokens.\n\n" +
		"Use it for exploration, review, and planning. Do not use it for a single file read or " +
		"one grep: a round trip through another model costs more than doing that yourself.\n\n" +
		"Sub-agents are read-only and cannot see this conversation, so the task must be " +
		"self-contained — name the files, symbols, and question explicitly.\n\n" +
		"Available agents:\n")
	names := make([]string, 0, len(t.Agents))
	for _, a := range t.Agents {
		fmt.Fprintf(&b, "- %s: %s\n", a.Name, a.Description)
		names = append(names, a.Name)
	}

	return Spec{
		Name:        "task",
		Description: b.String(),
		Schema: obj(map[string]any{
			"agent": map[string]any{
				"type": "string", "enum": names,
				"description": "Which sub-agent to run.",
			},
			"task": str("The complete task. The sub-agent sees only this, so include every " +
				"file path, symbol, and constraint it needs."),
		}, "agent", "task"),
	}
}

// Mutates is false: sub-agents are read-only, so a delegation changes nothing.
func (Task) Mutates() bool { return false }

// ParallelSafe lets the agent run several delegations at once. Sub-agents
// cannot write, so concurrent ones cannot conflict.
func (Task) ParallelSafe() bool { return true }

func (Task) Run(ctx context.Context, raw json.RawMessage, env *Env) (*Result, error) {
	var a struct {
		Agent string `json:"agent"`
		Task  string `json:"task"`
	}
	if err := ParseArgs(raw, &a); err != nil {
		return Errorf("%v", err), nil
	}
	if env.Spawn == nil {
		return Errorf("delegation is not available in this run; do the work yourself"), nil
	}
	if strings.TrimSpace(a.Task) == "" {
		return Errorf("task is required and must be self-contained"), nil
	}

	res, err := env.Spawn(ctx, SpawnRequest{Agent: a.Agent, Task: a.Task})
	if err != nil {
		return Errorf("%v", err), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Report from the %s sub-agent (%d steps, %d tokens):\n\n%s",
		res.Agent, res.Steps, res.PromptTokens+res.CompletionTokens, res.Summary)
	// A sub-agent that ran out of budget probably has an incomplete answer,
	// and the caller needs to know that before relying on it.
	if res.StopReason != "" && res.StopReason != "done" {
		fmt.Fprintf(&b, "\n\n[the sub-agent stopped early: %s — treat this report as incomplete]", res.StopReason)
	}

	body, note := env.Clip("task "+res.Agent, b.String())
	return &Result{
		Content: body + note,
		Display: fmt.Sprintf("task %s → %d steps, %d tok", res.Agent, res.Steps,
			res.PromptTokens+res.CompletionTokens),
	}, nil
}
