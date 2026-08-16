// Package selfcheck runs the routing and streaming invariants end to end
// against in-process stub servers.
//
// Why this exists alongside the _test.go files: Windows Smart App Control, in
// enforced mode, blocks `go test` binaries from executing while permitting
// ordinary `go build` output. Shipping the checks as a normal command keeps
// them runnable on a locked-down machine — and doubles as a diagnostic after
// you edit the config or add a provider.
package selfcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/VEER-TARGARYEN/forge/internal/config"
	"github.com/VEER-TARGARYEN/forge/internal/provider"
	"github.com/VEER-TARGARYEN/forge/internal/router"
)

type Check struct {
	Name    string
	Detail  string
	Passed  bool
	Elapsed time.Duration
}

type failure struct{ msg string }

func (f failure) Error() string { return f.msg }

func failf(format string, a ...any) error { return failure{fmt.Sprintf(format, a...)} }

type namedCheck struct {
	name string
	fn   func() (string, error)
}

// Run executes every check and reports results. It never panics out: a check
// that blows up is reported as a failure like any other.
func Run(w io.Writer) (checks []Check, ok bool) {
	cases := []namedCheck{
		{"stream: assembles split content", checkStreamContent},
		{"stream: rejoins fragmented tool-call JSON", checkToolCallFragments},
		{"stream: estimates usage when provider omits it", checkEstimatedUsage},
		{"stream: accepts array-form content", checkArrayContent},
		{"stream: preserves Gemini thought_signature on tool calls", checkThoughtSignature},
		{"stream: routes inline <think> to the reasoning channel", checkThinkSplit},
		{"stream: a <think> tag split across chunks still works", checkThinkAcrossChunks},
		{"timing: non-streaming reports no fabricated rate", checkNoFakeThroughput},
		{"errors: status+body classification", checkClassification},
		{"errors: a URL in the body cannot cause a quota misread", checkRateLimitNotQuota},
		{"router: fails over past a 429", checkFailover},
		{"router: waits out a short rate limit instead of demoting", checkRateLimitWait},
		{"router: honours Retry-After over the default", checkRetryAfter},
		{"router: keeps a cooling-down target parked", checkCooldownParks},
		{"router: skips unconfigured and oversized targets", checkSkips},
		{"router: aggregate error names every target", checkAggregateError},
		{"router: provider:model pin bypasses the chain", checkPin},
		{"health: quota gets a long cooldown", checkQuotaCooldown},
		{"health: context-length failure does not park a target", checkContextLengthNoPark},
		{"health: cooldowns survive a restart", checkHealthPersists},
		{"ledger: records successes and failures", checkLedger},
	}
	cases = append(cases, agentCases()...)
	cases = append(cases, contextCases()...)
	cases = append(cases, searchCases()...)
	cases = append(cases, verifyCases()...)
	cases = append(cases, subAgentCases()...)
	cases = append(cases, uiCases()...)
	cases = append(cases, embedCases()...)

	ok = true
	for _, c := range cases {
		start := time.Now()
		detail, err := safeRun(c.fn)
		el := time.Since(start)
		if err != nil {
			ok = false
			checks = append(checks, Check{Name: c.name, Detail: err.Error(), Elapsed: el})
			fmt.Fprintf(w, "  FAIL  %-52s %s\n", c.name, err.Error())
			continue
		}
		checks = append(checks, Check{Name: c.name, Detail: detail, Passed: true, Elapsed: el})
		fmt.Fprintf(w, "  ok    %-52s %s\n", c.name, detail)
	}
	return checks, ok
}

func safeRun(fn func() (string, error)) (detail string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = failf("panic: %v", r)
		}
	}()
	return fn()
}

// ---------- stub servers ----------

// stubServer answers in whichever format the caller asked for: JSON for a
// normal call, SSE when "stream":true is set.
func stubServer(status int, retryAfter, body string, hits *int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			fmt.Fprint(w, `{"data":[{"id":"m"}]}`)
			return
		}
		if hits != nil {
			atomic.AddInt32(hits, 1)
		}
		if status != 200 {
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			w.WriteHeader(status)
			fmt.Fprint(w, body)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(raw, &probe)
		if !probe.Stream {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fl.Flush()
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2,\"total_tokens\":9}}\n\n")
		fl.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
}

func sseServer(frames []string, gapsAt map[int]time.Duration) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i, f := range frames {
			if d, ok := gapsAt[i]; ok {
				time.Sleep(d)
			}
			fmt.Fprintf(w, "data: %s\n\n", f)
			fl.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
}

func client(name, base string) *provider.OpenAICompat {
	return provider.NewOpenAICompat(provider.OpenAIOpts{
		Name: name, BaseURL: base + "/v1", APIKey: "k", StreamUsage: true, RequiresKey: true,
	})
}

// ---------- provider-level checks ----------

var toolFrames = []string{
	`{"model":"test-model","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
	`{"choices":[{"index":0,"delta":{"content":"Hel"}}]}`,
	`{"choices":[{"index":0,"delta":{"content":"lo"}}]}`,
	`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":"}}]}}]}`,
	`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"main.go\"}"}}]}}]}`,
	`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}}`,
}

// Two gaps well above the ~0.6 ms clock granularity: one before first content
// so TTFT is measurable, one mid-stream so the generation span is separable.
var toolGaps = map[int]time.Duration{1: 20 * time.Millisecond, 4: 20 * time.Millisecond}

func checkStreamContent() (string, error) {
	srv := sseServer(toolFrames, toolGaps)
	defer srv.Close()

	var seen strings.Builder
	resp, err := client("t", srv.URL).Stream(context.Background(), "test-model",
		provider.Request{Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}}},
		func(c provider.Chunk) error { seen.WriteString(c.Content); return nil })
	if err != nil {
		return "", failf("stream: %v", err)
	}
	if resp.Content != "Hello" {
		return "", failf("content = %q, want %q", resp.Content, "Hello")
	}
	if seen.String() != "Hello" {
		return "", failf("callback saw %q, want %q", seen.String(), "Hello")
	}
	if !resp.Streamed || resp.TTFT <= 0 || resp.TTFT >= resp.Latency {
		return "", failf("ttft %v not inside (0, latency %v)", resp.TTFT, resp.Latency)
	}
	if resp.DecodeTPS() <= 0 || resp.PrefillTPS() <= 0 {
		return "", failf("throughput = %.1f decode / %.1f prefill, want both > 0", resp.DecodeTPS(), resp.PrefillTPS())
	}
	if resp.Usage.PromptTokens != 100 || resp.Usage.CompletionTokens != 20 || resp.Usage.Estimated {
		return "", failf("usage = %+v", resp.Usage)
	}
	return fmt.Sprintf("ttft %v, %.0f tok/s decode", resp.TTFT.Round(time.Millisecond), resp.DecodeTPS()), nil
}

func checkToolCallFragments() (string, error) {
	srv := sseServer(toolFrames, nil)
	defer srv.Close()

	resp, err := client("t", srv.URL).Stream(context.Background(), "test-model",
		provider.Request{Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}}}, nil)
	if err != nil {
		return "", failf("stream: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		return "", failf("got %d tool calls, want 1", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	const want = `{"path":"main.go"}`
	if tc.Function.Arguments != want {
		return "", failf("args = %q, want %q", tc.Function.Arguments, want)
	}
	// The fragments must rejoin into parseable JSON, not two half-objects.
	var parsed map[string]any
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &parsed); err != nil {
		return "", failf("rejoined args are not valid JSON: %v", err)
	}
	if tc.ID != "call_1" || tc.Function.Name != "read_file" {
		return "", failf("identity = %+v", tc)
	}
	if resp.FinishReason != "tool_calls" {
		return "", failf("finish = %q", resp.FinishReason)
	}
	return "2 fragments -> valid JSON", nil
}

func checkEstimatedUsage() (string, error) {
	srv := sseServer([]string{`{"choices":[{"index":0,"delta":{"content":"abc"}}]}`}, nil)
	defer srv.Close()

	resp, err := client("t", srv.URL).Stream(context.Background(), "m",
		provider.Request{Messages: []provider.Message{{Role: provider.RoleUser, Content: "hello world"}}}, nil)
	if err != nil {
		return "", failf("stream: %v", err)
	}
	if !resp.Usage.Estimated {
		return "", failf("usage should be flagged Estimated when the provider omits it")
	}
	if resp.Usage.TotalTokens <= 0 {
		return "", failf("estimated usage should still be non-zero")
	}
	return fmt.Sprintf("flagged, ~%d tok", resp.Usage.TotalTokens), nil
}

func checkArrayContent() (string, error) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":[{"type":"text","text":"part1 "},{"type":"text","text":"part2"}]},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`)
	}))
	defer srv.Close()

	resp, err := client("t", srv.URL).Chat(context.Background(), "m",
		provider.Request{Messages: []provider.Message{{Role: provider.RoleUser, Content: "x"}}})
	if err != nil {
		return "", failf("chat: %v", err)
	}
	if resp.Content != "part1 part2" {
		return "", failf("content = %q, want %q", resp.Content, "part1 part2")
	}
	return "concatenated", nil
}

func checkNoFakeThroughput() (string, error) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A loopback round trip can finish inside a single ~0.6 ms clock tick,
		// which makes time.Since legitimately return 0. Delaying past the tick
		// keeps the latency assertion deterministic instead of flaky.
		time.Sleep(5 * time.Millisecond)
		fmt.Fprint(w, `{"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":50,"completion_tokens":10,"total_tokens":60}}`)
	}))
	defer srv.Close()

	resp, err := client("t", srv.URL).Chat(context.Background(), "m",
		provider.Request{Messages: []provider.Message{{Role: provider.RoleUser, Content: "x"}}})
	if err != nil {
		return "", failf("chat: %v", err)
	}
	if resp.Streamed {
		return "", failf("Streamed should be false for a non-streaming call")
	}
	if resp.TTFT != 0 {
		return "", failf("TTFT = %v, want 0 (no first-token signal exists)", resp.TTFT)
	}
	if resp.PrefillTPS() != 0 || resp.DecodeTPS() != 0 {
		return "", failf("throughput = %v/%v, want 0/0", resp.PrefillTPS(), resp.DecodeTPS())
	}
	if resp.Latency < provider.ClockFloor {
		return "", failf("latency = %v, want at least the %v clock floor", resp.Latency, provider.ClockFloor)
	}
	return "reports n/a, not a guess", nil
}

// Reasoning models that inline <think>…</think> in the content stream — Qwen3,
// the DeepSeek-R1 distills — would otherwise put their scratchpad into the
// agent's message history, where it is re-sent on every subsequent turn. This
// is measured: qwen3.6-27b spends 182 tokens saying "ok".
// Gemini 3 returns a thought_signature inside each tool call's extra_content
// and hard-rejects (HTTP 400) the next request if the assistant turn is
// replayed without it. Observed live: the agent died on its second step until
// this round-tripped. The signature must survive both stream decoding and
// re-serialisation of the message.
func checkThoughtSignature() (string, error) {
	srv := sseServer([]string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"path\":"},"extra_content":{"google":{"thought_signature":"SIG-abc-123"}}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"main.go\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":10,"total_tokens":15}}`,
	}, nil)
	defer srv.Close()

	resp, err := client("g", srv.URL).Stream(context.Background(), "m",
		provider.Request{Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}}}, nil)
	if err != nil {
		return "", failf("stream: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		return "", failf("got %d tool calls, want 1", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if !strings.Contains(string(tc.ExtraContent), "SIG-abc-123") {
		return "", failf("thought_signature not captured: extra_content = %q", string(tc.ExtraContent))
	}
	// Arguments must still assemble correctly alongside the extra field.
	if tc.Function.Arguments != `{"path":"main.go"}` {
		return "", failf("args = %q", tc.Function.Arguments)
	}

	// The whole point is the round trip: re-serialising the assistant message
	// must put the signature back on the wire for the next request.
	msg := provider.Message{Role: provider.RoleAssistant, ToolCalls: resp.ToolCalls}
	raw, err := json.Marshal(msg)
	if err != nil {
		return "", failf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), "SIG-abc-123") || !strings.Contains(string(raw), "thought_signature") {
		return "", failf("signature dropped on re-serialisation: %s", raw)
	}
	// A tool call with no extra_content must not emit an empty field, or
	// stricter providers may object.
	plain, _ := json.Marshal(provider.Message{Role: provider.RoleAssistant,
		ToolCalls: []provider.ToolCall{{ID: "x", Type: "function"}}})
	if strings.Contains(string(plain), "extra_content") {
		return "", failf("empty extra_content leaked onto the wire: %s", plain)
	}
	return "captured, replayed, omitted when absent", nil
}

func checkThinkSplit() (string, error) {
	srv := sseServer([]string{
		`{"choices":[{"index":0,"delta":{"content":"<think>Let me consider. The user wants ok.</think>"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"ok"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":40,"total_tokens":45}}`,
	}, nil)
	defer srv.Close()

	var seen strings.Builder
	resp, err := client("t", srv.URL).Stream(context.Background(), "m",
		provider.Request{Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}}},
		func(c provider.Chunk) error { seen.WriteString(c.Content); return nil })
	if err != nil {
		return "", failf("stream: %v", err)
	}
	if resp.Content != "ok" {
		return "", failf("content = %q, want just the answer", resp.Content)
	}
	if seen.String() != "ok" {
		return "", failf("the callback saw %q; the scratchpad reached the caller", seen.String())
	}
	if !strings.Contains(resp.Reasoning, "Let me consider") {
		return "", failf("reasoning = %q, want the scratchpad", resp.Reasoning)
	}

	// An unterminated think block means the model ran out of budget mid-thought
	// and produced no answer; surfacing half a scratchpad as the answer would
	// be worse than surfacing nothing.
	c, r := provider.SplitThinking("<think>ran out of room")
	if c != "" {
		return "", failf("unterminated think produced content %q", c)
	}
	if r == "" {
		return "", failf("unterminated think discarded the reasoning")
	}
	// Content with no tags at all must pass through untouched.
	if c2, r2 := provider.SplitThinking("plain answer"); c2 != "plain answer" || r2 != "" {
		return "", failf("plain content was altered: %q / %q", c2, r2)
	}
	return "scratchpad separated from the answer", nil
}

func checkThinkAcrossChunks() (string, error) {
	// The tag arrives one byte at a time. A splitter that emitted content
	// eagerly would leak "<", "t", "h"… into the answer before recognising
	// the tag, and there is no un-printing a streamed character.
	var frames []string
	for _, piece := range []string{"<t", "hi", "nk>", "hid", "den", "</th", "ink>", "vis", "ible"} {
		frames = append(frames, fmt.Sprintf(`{"choices":[{"index":0,"delta":{"content":%q}}]}`, piece))
	}
	frames = append(frames,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":9,"total_tokens":10}}`)

	srv := sseServer(frames, nil)
	defer srv.Close()

	var seen strings.Builder
	resp, err := client("t", srv.URL).Stream(context.Background(), "m",
		provider.Request{Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}}},
		func(c provider.Chunk) error { seen.WriteString(c.Content); return nil })
	if err != nil {
		return "", failf("stream: %v", err)
	}
	if resp.Content != "visible" {
		return "", failf("content = %q, want %q", resp.Content, "visible")
	}
	if seen.String() != "visible" {
		return "", failf("callback saw %q; a partial tag leaked", seen.String())
	}
	if resp.Reasoning != "hidden" {
		return "", failf("reasoning = %q, want %q", resp.Reasoning, "hidden")
	}
	return "9 fragments, tag boundaries intact", nil
}

// Groq's 429 body ends with an upgrade link whose path contains "billing".
// Matched as a keyword, that reads as quota exhaustion and earns the one-hour
// cooldown instead of the fifty-one seconds the provider actually asked for —
// observed live, and it took a working model out of service for an hour.
func checkRateLimitNotQuota() (string, error) {
	groqBody := "Rate limit reached for model `openai/gpt-oss-120b` on tokens per minute (TPM): " +
		"Limit 8000, Used 7274, Requested 7590. Please try again in 51.48s. " +
		"Need more tokens? Upgrade to Dev Tier today at https://console.groq.com/settings/billing"

	kindOf := func(retryAfter string, body string) provider.ErrKind {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, body)
		}))
		defer srv.Close()
		_, err := client("g", srv.URL).Chat(context.Background(), "m",
			provider.Request{Messages: []provider.Message{{Role: provider.RoleUser, Content: "x"}}})
		return provider.KindOf(err)
	}

	// A short Retry-After is definitive regardless of the body text.
	if got := kindOf("52", groqBody); got != provider.ErrRateLimit {
		return "", failf("with Retry-After 52s, classified as %v — that is an hour-long block for a 52s throttle", got)
	}
	// And with no Retry-After at all, the URL must still not decide it.
	if got := kindOf("", groqBody); got != provider.ErrRateLimit {
		return "", failf("without Retry-After, the billing URL still forced %v", got)
	}
	// Genuine quota language, outside a URL, must still be recognised.
	if got := kindOf("", "You have exceeded your daily quota for this model."); got != provider.ErrQuota {
		return "", failf("real quota language classified as %v", got)
	}
	return "URL ignored, Retry-After wins, real quota still detected", nil
}

func checkClassification() (string, error) {
	cases := []struct {
		status int
		body   string
		want   provider.ErrKind
	}{
		{429, "rate limit reached for model", provider.ErrRateLimit},
		{429, "You exceeded your current quota for the day", provider.ErrQuota},
		{401, "invalid api key", provider.ErrAuth},
		{404, "not found", provider.ErrModelNotFound},
		{400, "This model's maximum context length is 8192 tokens", provider.ErrContextLength},
		{400, "invalid model: foo", provider.ErrModelNotFound},
		{400, "missing required field", provider.ErrBadRequest},
		{402, "payment required", provider.ErrQuota},
		{413, "payload too large", provider.ErrContextLength},
		// Groq dresses a per-minute throttle as a 413; compacting the
		// conversation would be the wrong fix for something that clears on
		// its own.
		{413, "Request too large on tokens per minute (TPM): Limit 8000", provider.ErrRateLimit},
		{503, "overloaded", provider.ErrServer},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			fmt.Fprint(w, c.body)
		}))
		_, err := client("t", srv.URL).Chat(context.Background(), "m",
			provider.Request{Messages: []provider.Message{{Role: provider.RoleUser, Content: "x"}}})
		srv.Close()
		if err == nil {
			return "", failf("status %d produced no error", c.status)
		}
		if got := provider.KindOf(err); got != c.want {
			return "", failf("status %d %q -> %v, want %v", c.status, c.body, got, c.want)
		}
	}
	return fmt.Sprintf("%d cases", len(cases)), nil
}

// ---------- router-level checks ----------

func testRouter(targets []config.Target, providers []config.Provider) (*router.Router, *router.Ledger, func(), error) {
	return testRouterPolicy(targets, providers, nil)
}

// testRouterPolicy builds a router whose retry policy can be adjusted, for the
// checks that exercise cooldown and wait behaviour specifically.
func testRouterPolicy(targets []config.Target, providers []config.Provider,
	tweak func(*config.Policy)) (*router.Router, *router.Ledger, func(), error) {
	dir, err := os.MkdirTemp("", "forge-selfcheck-*")
	if err != nil {
		return nil, nil, func() {}, err
	}
	cfg := &config.Config{
		Providers: providers,
		Classes:   map[string][]config.Target{"coder": targets},
		Defaults:  config.Defaults{Temperature: 0.2, MaxTokens: 256},
		Policy: config.Policy{
			RateLimitCooldownSec: 20, QuotaCooldownSec: 3600, AuthCooldownSec: 86400,
			ServerCooldownSec: 30, BadRequestCooldownSec: 600, MaxCooldownSec: 900,
			SameTargetRetries: 0,
		},
		Server:   config.Server{Addr: "127.0.0.1:0", DefaultClass: "coder"},
		StateDir: dir,
	}
	if tweak != nil {
		tweak(&cfg.Policy)
	}
	reg := provider.NewRegistry(cfg)
	health := router.NewHealth(dir, cfg.Policy)
	ledger, err := router.NewLedger(dir)
	if err != nil {
		return nil, nil, func() { os.RemoveAll(dir) }, err
	}
	cleanup := func() { ledger.Close(); os.RemoveAll(dir) }
	return router.New(cfg, reg, health, ledger), ledger, cleanup, nil
}

func ask(rt *router.Router, class, text string) (*provider.Response, error) {
	return rt.Chat(context.Background(), class, provider.Request{
		Messages: []provider.Message{{Role: provider.RoleUser, Content: text}},
	})
}

func checkFailover() (string, error) {
	var hitsA, hitsB int32
	a := stubServer(429, "7", `{"error":{"message":"rate limit reached"}}`, &hitsA)
	defer a.Close()
	b := stubServer(200, "", "", &hitsB)
	defer b.Close()

	rt, _, cleanup, err := testRouter(
		[]config.Target{{Provider: "a", Model: "m"}, {Provider: "b", Model: "m"}},
		[]config.Provider{
			{Name: "a", BaseURL: a.URL + "/v1", APIKey: "k"},
			{Name: "b", BaseURL: b.URL + "/v1", APIKey: "k"},
		})
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}

	resp, err := ask(rt, "coder", "hi")
	if err != nil {
		return "", failf("chat: %v", err)
	}
	if resp.Provider != "b" {
		return "", failf("served by %q, want fallback to b", resp.Provider)
	}
	if atomic.LoadInt32(&hitsA) != 1 || atomic.LoadInt32(&hitsB) != 1 {
		return "", failf("hits a=%d b=%d, want 1 and 1", hitsA, hitsB)
	}
	return "429 on a -> served by b", nil
}

// throttleOnce rate limits the first call with a short Retry-After, then
// serves normally — exactly what a per-minute token budget does mid-run.
func throttleOnce(retryAfter string, hits *int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			fmt.Fprint(w, `{"data":[{"id":"m"}]}`)
			return
		}
		if atomic.AddInt32(hits, 1) == 1 {
			w.Header().Set("Retry-After", retryAfter)
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"message":"Rate limit reached on tokens per minute (TPM). Please try again in 1s."}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`)
	}))
}

func checkRateLimitWait() (string, error) {
	var hits int32
	srv := throttleOnce("1", &hits)
	defer srv.Close()

	// One target only, so demoting it would end the run — which is exactly
	// what happened live before this existed.
	rt, _, cleanup, err := testRouterPolicy(
		[]config.Target{{Provider: "a", Model: "m"}},
		[]config.Provider{{Name: "a", BaseURL: srv.URL + "/v1", APIKey: "k"}},
		func(p *config.Policy) { p.RateLimitWaitSec = 30; p.RateLimitWaits = 3 })
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}

	start := time.Now()
	resp, err := rt.Chat(context.Background(), "coder", provider.Request{
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	elapsed := time.Since(start)
	if err != nil {
		return "", failf("a 1s throttle ended the run: %v", err)
	}
	if resp.Content != "ok" {
		return "", failf("content = %q", resp.Content)
	}
	if atomic.LoadInt32(&hits) != 2 {
		return "", failf("hit the target %d times, want 2 (throttled then served)", hits)
	}
	if elapsed < time.Second {
		return "", failf("returned in %v; the Retry-After was not honoured", elapsed)
	}
	// Waiting is not failing: the target must not be left cooling down.
	if ok, left := rt.Health().Available("a|m", time.Now()); !ok {
		return "", failf("target parked for %v after a successful wait-and-retry", left)
	}

	// A long Retry-After is a different thing and must still demote.
	var hits2 int32
	srv2 := throttleOnce("600", &hits2)
	defer srv2.Close()
	rt2, _, cleanup2, err := testRouterPolicy(
		[]config.Target{{Provider: "a", Model: "m"}},
		[]config.Provider{{Name: "a", BaseURL: srv2.URL + "/v1", APIKey: "k"}},
		func(p *config.Policy) { p.RateLimitWaitSec = 30 })
	defer cleanup2()
	if err != nil {
		return "", failf("setup: %v", err)
	}
	if _, err := rt2.Chat(context.Background(), "coder", provider.Request{
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	}); err == nil {
		return "", failf("a 600s Retry-After was waited out inline")
	}
	return fmt.Sprintf("1s waited (%v), 600s demoted", elapsed.Round(100*time.Millisecond)), nil
}

func checkRetryAfter() (string, error) {
	var hits int32
	a := stubServer(429, "7", `{"error":{"message":"rate limit"}}`, &hits)
	defer a.Close()
	b := stubServer(200, "", "", nil)
	defer b.Close()

	rt, _, cleanup, err := testRouter(
		[]config.Target{{Provider: "a", Model: "m"}, {Provider: "b", Model: "m"}},
		[]config.Provider{
			{Name: "a", BaseURL: a.URL + "/v1", APIKey: "k"},
			{Name: "b", BaseURL: b.URL + "/v1", APIKey: "k"},
		})
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}
	if _, err := ask(rt, "coder", "hi"); err != nil {
		return "", failf("chat: %v", err)
	}

	// Undershooting what a provider explicitly asked for is how a free tier
	// turns into a revoked free tier, so Retry-After must beat the default.
	ok, left := rt.Health().Available("a|m", time.Now())
	if ok {
		return "", failf("target a should be cooling down")
	}
	if left < 6*time.Second || left > 9*time.Second {
		return "", failf("cooldown %v, want ~8s from Retry-After: 7 (not the 20s default)", left)
	}
	return fmt.Sprintf("Retry-After 7s -> %v", left.Round(time.Second)), nil
}

func checkCooldownParks() (string, error) {
	var hitsA int32
	a := stubServer(429, "60", `{"error":{"message":"rate limit"}}`, &hitsA)
	defer a.Close()
	b := stubServer(200, "", "", nil)
	defer b.Close()

	rt, _, cleanup, err := testRouter(
		[]config.Target{{Provider: "a", Model: "m"}, {Provider: "b", Model: "m"}},
		[]config.Provider{
			{Name: "a", BaseURL: a.URL + "/v1", APIKey: "k"},
			{Name: "b", BaseURL: b.URL + "/v1", APIKey: "k"},
		})
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := ask(rt, "coder", "hi"); err != nil {
			return "", failf("chat %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&hitsA); got != 1 {
		return "", failf("target a hit %d times across 3 calls, want 1", got)
	}
	return "3 calls, 1 hit on the parked target", nil
}

func checkSkips() (string, error) {
	var hits int32
	b := stubServer(200, "", "", &hits)
	defer b.Close()

	rt, _, cleanup, err := testRouter(
		[]config.Target{
			{Provider: "nokey", Model: "m"},
			{Provider: "small", Model: "m", MaxContext: 10},
			{Provider: "b", Model: "m"},
		},
		[]config.Provider{
			{Name: "nokey", BaseURL: "http://127.0.0.1:1/v1"},
			{Name: "small", BaseURL: "http://127.0.0.1:1/v1", APIKey: "k"},
			{Name: "b", BaseURL: b.URL + "/v1", APIKey: "k"},
		})
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}

	resp, err := rt.Chat(context.Background(), "coder", provider.Request{
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: strings.Repeat("token ", 500)}},
		MaxTokens: 256,
	})
	if err != nil {
		return "", failf("chat: %v", err)
	}
	if resp.Provider != "b" {
		return "", failf("served by %q, want b", resp.Provider)
	}
	// Neither skipped target should have been dialled at all — the skip is a
	// pre-flight decision, not a failed round trip.
	if hits != 1 {
		return "", failf("b hits = %d, want 1", hits)
	}
	return "no-key + too-small skipped without a round trip", nil
}

func checkAggregateError() (string, error) {
	a := stubServer(401, "", `{"error":{"message":"bad key"}}`, nil)
	defer a.Close()
	b := stubServer(500, "", `{"error":{"message":"boom"}}`, nil)
	defer b.Close()

	rt, _, cleanup, err := testRouter(
		[]config.Target{{Provider: "a", Model: "m"}, {Provider: "b", Model: "m"}},
		[]config.Provider{
			{Name: "a", BaseURL: a.URL + "/v1", APIKey: "k"},
			{Name: "b", BaseURL: b.URL + "/v1", APIKey: "k"},
		})
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}

	_, err = ask(rt, "coder", "hi")
	if err == nil {
		return "", failf("expected an error when every target fails")
	}
	re, ok := err.(*router.RouteError)
	if !ok {
		return "", failf("error type %T, want *router.RouteError", err)
	}
	if len(re.Attempts) != 2 {
		return "", failf("recorded %d attempts, want 2", len(re.Attempts))
	}
	if re.Attempts[0].Kind != provider.ErrAuth || re.Attempts[1].Kind != provider.ErrServer {
		return "", failf("kinds = %v, %v; want auth, server", re.Attempts[0].Kind, re.Attempts[1].Kind)
	}
	msg := re.Error()
	if !strings.Contains(msg, "a|m") || !strings.Contains(msg, "b|m") {
		return "", failf("error message omits a target: %s", msg)
	}
	return "both reasons reported", nil
}

func checkPin() (string, error) {
	var hitsA int32
	a := stubServer(200, "", "", &hitsA)
	defer a.Close()
	b := stubServer(200, "", "", nil)
	defer b.Close()

	rt, _, cleanup, err := testRouter(
		[]config.Target{{Provider: "a", Model: "m"}},
		[]config.Provider{
			{Name: "a", BaseURL: a.URL + "/v1", APIKey: "k"},
			{Name: "b", BaseURL: b.URL + "/v1", APIKey: "k"},
		})
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}

	resp, err := ask(rt, "b:m", "hi")
	if err != nil {
		return "", failf("pinned chat: %v", err)
	}
	if resp.Provider != "b" {
		return "", failf("pin routed to %q, want b", resp.Provider)
	}
	if atomic.LoadInt32(&hitsA) != 0 {
		return "", failf("pin should not touch the chain, but a was hit %d times", hitsA)
	}
	return "chain untouched", nil
}

// ---------- health / ledger checks ----------

func policy() config.Policy {
	return config.Policy{
		RateLimitCooldownSec: 20, QuotaCooldownSec: 3600, AuthCooldownSec: 86400,
		ServerCooldownSec: 30, BadRequestCooldownSec: 600, MaxCooldownSec: 900,
	}
}

func checkQuotaCooldown() (string, error) {
	dir, err := os.MkdirTemp("", "forge-health-*")
	if err != nil {
		return "", failf("tempdir: %v", err)
	}
	defer os.RemoveAll(dir)

	h := router.NewHealth(dir, policy())
	cd := h.RecordFailure("p|m", provider.ErrQuota, 0, "quota", time.Now())
	if cd < time.Hour {
		return "", failf("quota cooldown = %v, want >= 1h", cd)
	}
	return cd.Round(time.Minute).String(), nil
}

func checkContextLengthNoPark() (string, error) {
	dir, err := os.MkdirTemp("", "forge-health-*")
	if err != nil {
		return "", failf("tempdir: %v", err)
	}
	defer os.RemoveAll(dir)

	now := time.Now()
	h := router.NewHealth(dir, policy())
	// Context-length says nothing about the target's health: the next request
	// may well fit, so it must not park the target.
	if cd := h.RecordFailure("q|m", provider.ErrContextLength, 0, "too long", now); cd != 0 {
		return "", failf("context-length cooldown = %v, want 0", cd)
	}
	if ok, _ := h.Available("q|m", now); !ok {
		return "", failf("context-length failure should leave the target available")
	}
	return "stays available", nil
}

func checkHealthPersists() (string, error) {
	dir, err := os.MkdirTemp("", "forge-health-*")
	if err != nil {
		return "", failf("tempdir: %v", err)
	}
	defer os.RemoveAll(dir)

	h1 := router.NewHealth(dir, policy())
	h1.RecordFailure("p|m", provider.ErrRateLimit, 0, "429", time.Now())
	if err := h1.Flush(); err != nil {
		return "", failf("flush: %v", err)
	}

	// A restart that forgets cooldowns re-hammers a provider that just rate
	// limited you — the fastest way to lose a free tier.
	h2 := router.NewHealth(dir, policy())
	ok, left := h2.Available("p|m", time.Now())
	if ok || left <= 0 {
		return "", failf("cooldown did not survive restart (available=%v, left=%v)", ok, left)
	}
	return fmt.Sprintf("%v remaining after reload", left.Round(time.Second)), nil
}

func checkLedger() (string, error) {
	a := stubServer(429, "5", `{"error":{"message":"rate limit"}}`, nil)
	defer a.Close()
	b := stubServer(200, "", "", nil)
	defer b.Close()

	rt, ledger, cleanup, err := testRouter(
		[]config.Target{{Provider: "a", Model: "m"}, {Provider: "b", Model: "m"}},
		[]config.Provider{
			{Name: "a", BaseURL: a.URL + "/v1", APIKey: "k"},
			{Name: "b", BaseURL: b.URL + "/v1", APIKey: "k"},
		})
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}
	if _, err := ask(rt, "coder", "hi"); err != nil {
		return "", failf("chat: %v", err)
	}

	stats, err := router.Summarize(ledger.Path(), time.Time{}, func(r router.Record) string { return r.Provider })
	if err != nil {
		return "", failf("summarize: %v", err)
	}
	byProvider := map[string]router.Stat{}
	for _, s := range stats {
		byProvider[s.Key] = s
	}
	if byProvider["a"].Failures != 1 {
		return "", failf("ledger recorded %d failures for a, want 1", byProvider["a"].Failures)
	}
	if byProvider["b"].Calls != 1 || byProvider["b"].Failures != 0 {
		return "", failf("ledger for b = %+v, want 1 call 0 failures", byProvider["b"])
	}
	if byProvider["b"].OutTok != 2 {
		return "", failf("ledger for b recorded %d output tokens, want 2", byProvider["b"].OutTok)
	}
	return "1 failure + 1 success with token counts", nil
}
