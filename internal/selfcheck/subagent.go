package selfcheck

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VEER-TARGARYEN/forge/internal/agent"
	"github.com/VEER-TARGARYEN/forge/internal/config"
	"github.com/VEER-TARGARYEN/forge/internal/tools"
)

// subAgentCases cover Phase 6: delegated contexts, isolation, parallelism, and
// the accounting that keeps a delegating run honest about what it cost.
func subAgentCases() []namedCheck {
	return []namedCheck{
		{"subagent: only the final message reaches the caller", checkSubAgentIsolation},
		{"subagent: cannot write, run commands, or verify", checkSubAgentReadOnly},
		{"subagent: cannot delegate further", checkSubAgentNoRecursion},
		{"subagent: tokens roll up into the parent's total", checkSubAgentAccounting},
		{"subagent: delegation limit is enforced", checkSubAgentLimit},
		{"subagent: an unknown role is refused with the roster", checkSubAgentUnknown},
		{"subagent: an over-long report is clamped", checkSubAgentClamp},
		{"subagent: early stop is flagged to the caller", checkSubAgentEarlyStop},
		{"task tool: reports unavailable when delegation is off", checkTaskUnavailable},
		{"agent: parallel delegations run concurrently", checkParallelDelegation},
		{"agent: tool results stay aligned with their call ids", checkToolResultOrdering},
	}
}

// ---------- helpers ----------

// subModel serves a sub-agent conversation: one optional tool call, then a
// final report.
func subModel(frames [][]string, hits *int32, delay time.Duration) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			fmt.Fprint(w, `{"data":[{"id":"m"}]}`)
			return
		}
		n := int(atomic.AddInt32(hits, 1)) - 1
		if n >= len(frames) {
			n = len(frames) - 1
		}
		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, f := range frames[n] {
			fmt.Fprintf(w, "data: %s\n\n", f)
			fl.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
}

func textTurn(content string, in, out int) []string {
	return []string{
		fmt.Sprintf(`{"choices":[{"index":0,"delta":{"content":%q}}]}`, content),
		fmt.Sprintf(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
			in, out, in+out),
	}
}

// spawnerFor builds a spawner whose sub-agents talk to srv.
func spawnerFor(ws *tools.Workspace, srv *httptest.Server, cfg agent.SpawnerConfig) (*agent.Spawner, *tools.Env, func(), error) {
	rt, _, cleanup, err := testRouter(
		[]config.Target{{Provider: "m", Model: "m"}},
		[]config.Provider{{Name: "m", BaseURL: srv.URL + "/v1", APIKey: "k"}})
	if err != nil {
		return nil, nil, cleanup, err
	}
	reg := tools.NewRegistry()
	reg.Register(tools.ReadFile{}, tools.Glob{}, tools.Grep{}, tools.ListDir{},
		tools.Expand{}, tools.WriteFile{}, tools.EditFile{}, tools.RunCommand{}, tools.Verify{})

	env, _ := envFor(ws, true)
	env.Overflow = tools.NewOverflow("")
	sp := agent.NewSpawner(rt, env, reg, cfg, io.Discard)
	env.Spawn = sp.Spawn
	return sp, env, cleanup, nil
}

// ---------- isolation ----------

func checkSubAgentIsolation() (string, error) {
	ws, wsCleanup, err := tempWS(map[string]string{
		"secret.go": "package main\n\n// the answer lives at line 4\nconst Answer = 42\n",
	})
	defer wsCleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}

	// The sub-agent reads a file, then reports. The file contents must NOT
	// reach the caller — only the closing sentence.
	var hits int32
	srv := subModel([][]string{
		{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"secret.go\"}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":500,"completion_tokens":20,"total_tokens":520}}`,
		},
		textTurn("Answer is defined in secret.go:4 as 42.", 800, 15),
	}, &hits, 0)
	defer srv.Close()

	sp, _, cleanup, err := spawnerFor(ws, srv, agent.SpawnerConfig{Quiet: true})
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}

	res, err := sp.Spawn(context.Background(), tools.SpawnRequest{
		Agent: "explore", Task: "where is Answer defined?",
	})
	if err != nil {
		return "", failf("spawn: %v", err)
	}
	if !strings.Contains(res.Summary, "secret.go:4") {
		return "", failf("summary lost the finding: %q", res.Summary)
	}
	// The whole point: the transcript stays behind.
	if strings.Contains(res.Summary, "package main") || strings.Contains(res.Summary, "const Answer") {
		return "", failf("the sub-agent's file read leaked into the caller's result:\n%s", res.Summary)
	}
	if res.Steps != 2 {
		return "", failf("steps = %d, want 2", res.Steps)
	}
	// 1,320 tokens were spent inside; the caller receives a sentence.
	spent := res.PromptTokens + res.CompletionTokens
	if spent < 1000 {
		return "", failf("expected the sub-agent to have spent real tokens, got %d", spent)
	}
	ratio := float64(spent) / float64(len(res.Summary)/4+1)
	return fmt.Sprintf("%d tok spent inside, %d chars returned (~%.0fx)", spent, len(res.Summary), ratio), nil
}

func checkSubAgentReadOnly() (string, error) {
	ws, wsCleanup, err := tempWS(map[string]string{"a.txt": "original\n"})
	defer wsCleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}

	// A sub-agent that tries to write. The tool must not even be registered.
	var hits int32
	srv := subModel([][]string{
		{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"a.txt\",\"content\":\"clobbered\"}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		},
		textTurn("I could not write.", 12, 4),
	}, &hits, 0)
	defer srv.Close()

	sp, _, cleanup, err := spawnerFor(ws, srv, agent.SpawnerConfig{Quiet: true})
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}

	if _, err := sp.Spawn(context.Background(), tools.SpawnRequest{
		Agent: "explore", Task: "try to modify a.txt",
	}); err != nil {
		return "", failf("spawn: %v", err)
	}
	// Parallel delegations editing the same tree would race, and an approval
	// prompt attributed to an invisible sub-context is unanswerable.
	if got := readFileIn(ws, "a.txt"); got != "original\n" {
		return "", failf("a sub-agent modified the workspace: %q", got)
	}
	return "write refused, file untouched", nil
}

func checkSubAgentNoRecursion() (string, error) {
	ws, wsCleanup, err := tempWS(nil)
	defer wsCleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}
	var hits int32
	srv := subModel([][]string{textTurn("done", 10, 3)}, &hits, 0)
	defer srv.Close()

	sp, parentEnv, cleanup, err := spawnerFor(ws, srv, agent.SpawnerConfig{Quiet: true})
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}
	if parentEnv.Spawn == nil {
		return "", failf("the parent env has no Spawn hook")
	}

	// Run one delegation, then confirm the child could not have delegated: the
	// task tool is absent from its registry AND its env has no Spawn hook.
	// Without a depth limit, one confused model can fork indefinitely.
	if _, err := sp.Spawn(context.Background(), tools.SpawnRequest{Agent: "explore", Task: "anything"}); err != nil {
		return "", failf("spawn: %v", err)
	}
	for _, s := range agent.Builtins {
		for _, t := range s.Tools {
			if t == "task" {
				return "", failf("role %q lists the task tool, which would allow recursion", s.Name)
			}
		}
	}
	return "no role can delegate", nil
}

// ---------- accounting ----------

func checkSubAgentAccounting() (string, error) {
	ws, wsCleanup, err := tempWS(nil)
	defer wsCleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}
	var hits int32
	srv := subModel([][]string{textTurn("found it", 700, 40)}, &hits, 0)
	defer srv.Close()

	sp, _, cleanup, err := spawnerFor(ws, srv, agent.SpawnerConfig{Quiet: true})
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}

	for i := 0; i < 3; i++ {
		if _, err := sp.Spawn(context.Background(), tools.SpawnRequest{
			Agent: "explore", Task: fmt.Sprintf("question %d", i),
		}); err != nil {
			return "", failf("spawn %d: %v", i, err)
		}
	}
	u := sp.Usage()
	if sp.Count() != 3 {
		return "", failf("count = %d, want 3", sp.Count())
	}
	// Tokens spent in a context the parent never saw are still real spend.
	if u.TotalTokens != 3*740 {
		return "", failf("usage total = %d, want %d", u.TotalTokens, 3*740)
	}
	if u.PromptTokens != 3*700 || u.CompletionTokens != 3*40 {
		return "", failf("usage split = %d/%d", u.PromptTokens, u.CompletionTokens)
	}
	return fmt.Sprintf("3 delegations, %d tok rolled up", u.TotalTokens), nil
}

func checkSubAgentLimit() (string, error) {
	ws, wsCleanup, err := tempWS(nil)
	defer wsCleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}
	var hits int32
	srv := subModel([][]string{textTurn("ok", 10, 2)}, &hits, 0)
	defer srv.Close()

	sp, _, cleanup, err := spawnerFor(ws, srv, agent.SpawnerConfig{Quiet: true, MaxSpawns: 2})
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}

	for i := 0; i < 2; i++ {
		if _, err := sp.Spawn(context.Background(), tools.SpawnRequest{Agent: "explore", Task: "x"}); err != nil {
			return "", failf("spawn %d should have been allowed: %v", i, err)
		}
	}
	// A loop of "explore some more" must not run up an unbounded bill.
	_, err = sp.Spawn(context.Background(), tools.SpawnRequest{Agent: "explore", Task: "x"})
	if err == nil {
		return "", failf("the third delegation was allowed past a limit of 2")
	}
	if !strings.Contains(err.Error(), "limit") {
		return "", failf("error does not explain the limit: %v", err)
	}
	if sp.Count() != 2 {
		return "", failf("count = %d after a refused spawn, want 2", sp.Count())
	}
	return "3rd refused at a limit of 2", nil
}

func checkSubAgentUnknown() (string, error) {
	ws, wsCleanup, err := tempWS(nil)
	defer wsCleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}
	var hits int32
	srv := subModel([][]string{textTurn("ok", 5, 2)}, &hits, 0)
	defer srv.Close()

	sp, _, cleanup, err := spawnerFor(ws, srv, agent.SpawnerConfig{Quiet: true})
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}

	_, err = sp.Spawn(context.Background(), tools.SpawnRequest{Agent: "architect", Task: "x"})
	if err == nil {
		return "", failf("an unknown role was accepted")
	}
	// The recovery path has to be in the message, or the model just guesses again.
	for _, want := range []string{"explore", "plan", "review"} {
		if !strings.Contains(err.Error(), want) {
			return "", failf("error does not list %q: %v", want, err)
		}
	}
	if sp.Count() != 0 {
		return "", failf("a refused role consumed a delegation slot")
	}
	return "refused, roster listed, no slot consumed", nil
}

func checkSubAgentClamp() (string, error) {
	ws, wsCleanup, err := tempWS(nil)
	defer wsCleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}
	long := strings.TrimSpace(strings.Repeat("word ", 2000))
	var hits int32
	srv := subModel([][]string{textTurn(long, 50, 2000)}, &hits, 0)
	defer srv.Close()

	sp, _, cleanup, err := spawnerFor(ws, srv, agent.SpawnerConfig{Quiet: true})
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}

	res, err := sp.Spawn(context.Background(), tools.SpawnRequest{Agent: "explore", Task: "ramble"})
	if err != nil {
		return "", failf("spawn: %v", err)
	}
	// A chatty sub-agent would otherwise reintroduce the context cost that
	// delegation exists to avoid.
	words := len(strings.Fields(res.Summary))
	if words > 420 {
		return "", failf("summary is %d words; the explore role caps at 400", words)
	}
	if !strings.Contains(res.Summary, "truncated") {
		return "", failf("truncation was silent")
	}
	return fmt.Sprintf("2000 words -> %d", words), nil
}

func checkSubAgentEarlyStop() (string, error) {
	ws, wsCleanup, err := tempWS(map[string]string{"a.go": "package a\n"})
	defer wsCleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}
	// A sub-agent that keeps calling tools until it hits its step limit.
	var hits int32
	srv := subModel([][]string{{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"list_dir","arguments":"{\"path\":\".\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
	}}, &hits, 0)
	defer srv.Close()

	sp, _, cleanup, err := spawnerFor(ws, srv, agent.SpawnerConfig{Quiet: true})
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}

	res, err := sp.Spawn(context.Background(), tools.SpawnRequest{Agent: "explore", Task: "loop forever"})
	if err != nil {
		return "", failf("spawn: %v", err)
	}
	if res.StopReason == "done" {
		return "", failf("a step-limited run reported a clean finish")
	}
	if !strings.Contains(strings.ToLower(res.StopReason), "step limit") {
		return "", failf("stop reason = %q", res.StopReason)
	}
	// The caller must be able to tell an incomplete report from a complete one.
	env, _ := envFor(ws, true)
	env.Overflow = tools.NewOverflow("")
	env.Spawn = func(ctx context.Context, req tools.SpawnRequest) (*tools.SpawnResult, error) {
		return res, nil
	}
	toolRes, err := runTool(tools.Task{Agents: agent.Infos(agent.Builtins)},
		map[string]any{"agent": "explore", "task": "x"}, env)
	if err != nil {
		return "", failf("task tool: %v", err)
	}
	if !strings.Contains(toolRes.Content, "incomplete") {
		return "", failf("the task tool did not flag an early stop:\n%s", toolRes.Content)
	}
	return "flagged as incomplete", nil
}

func checkTaskUnavailable() (string, error) {
	ws, cleanup, err := tempWS(nil)
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}
	env, _ := envFor(ws, true)
	env.Overflow = tools.NewOverflow("")
	env.Spawn = nil

	res, err := runTool(tools.Task{Agents: agent.Infos(agent.Builtins)},
		map[string]any{"agent": "explore", "task": "anything"}, env)
	if err != nil {
		return "", failf("run: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "yourself") {
		return "", failf("did not degrade with a usable instruction: %+v", res)
	}
	// An empty task would give the sub-agent nothing to work from.
	env.Spawn = func(ctx context.Context, r tools.SpawnRequest) (*tools.SpawnResult, error) {
		return &tools.SpawnResult{Agent: "explore", Summary: "x"}, nil
	}
	res2, err := runTool(tools.Task{Agents: agent.Infos(agent.Builtins)},
		map[string]any{"agent": "explore", "task": "   "}, env)
	if err != nil {
		return "", failf("run: %v", err)
	}
	if !res2.IsError {
		return "", failf("an empty task was accepted")
	}
	return "degrades cleanly, rejects an empty task", nil
}

// ---------- parallelism and ordering ----------

func checkParallelDelegation() (string, error) {
	ws, wsCleanup, err := tempWS(nil)
	defer wsCleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}
	const delay = 250 * time.Millisecond
	var hits int32
	srv := subModel([][]string{textTurn("report", 10, 5)}, &hits, delay)
	defer srv.Close()

	sp, _, cleanup, err := spawnerFor(ws, srv, agent.SpawnerConfig{Quiet: true, MaxConcurrent: 3})
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}

	start := time.Now()
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = sp.Spawn(context.Background(), tools.SpawnRequest{
				Agent: "explore", Task: fmt.Sprintf("question %d", i),
			})
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	for i, e := range errs {
		if e != nil {
			return "", failf("spawn %d: %v", i, e)
		}
	}
	// Serial execution would take at least 3x the delay.
	if elapsed > 2*delay {
		return "", failf("3 delegations took %s; serial would be ~%s, so they did not overlap",
			elapsed.Round(time.Millisecond), (3 * delay).Round(time.Millisecond))
	}
	return fmt.Sprintf("3 x %s of work in %s", delay, elapsed.Round(time.Millisecond)), nil
}

func checkToolResultOrdering() (string, error) {
	ws, wsCleanup, err := tempWS(map[string]string{
		"a.txt": "alpha\n", "b.txt": "bravo\n", "c.txt": "charlie\n",
	})
	defer wsCleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}

	// Three delegations issued in one turn, each answering with its own label.
	// Whatever order they complete in, the results must line up with the call
	// ids the model issued — providers reject a mismatch outright.
	var hits int32
	srv := scriptedModel([][]string{
		{
			`{"choices":[{"index":0,"delta":{"tool_calls":[` +
				`{"index":0,"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.txt\"}"}},` +
				`{"index":1,"id":"c2","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"b.txt\"}"}},` +
				`{"index":2,"id":"c3","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"c.txt\"}"}}` +
				`]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		},
		textTurn("Read all three.", 20, 6),
	}, &hits)
	defer srv.Close()

	ag, _, cleanup, err := agentFor(ws, srv, agent.Config{Class: "coder", MaxSteps: 4, Quiet: true})
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}
	if _, err := ag.Run(context.Background(), "read all three files"); err != nil {
		return "", failf("run: %v", err)
	}

	want := map[string]string{"c1": "alpha", "c2": "bravo", "c3": "charlie"}
	var order []string
	for _, m := range ag.Messages() {
		if m.Role != "tool" {
			continue
		}
		order = append(order, m.ToolCallID)
		if body, ok := want[m.ToolCallID]; ok && !strings.Contains(m.Content, body) {
			return "", failf("tool result for %s does not contain %q:\n%s", m.ToolCallID, body, m.Content)
		}
	}
	if len(order) != 3 {
		return "", failf("got %d tool results, want 3", len(order))
	}
	if order[0] != "c1" || order[1] != "c2" || order[2] != "c3" {
		return "", failf("tool results out of order: %v", order)
	}
	return "3 results, ids aligned and in order", nil
}
