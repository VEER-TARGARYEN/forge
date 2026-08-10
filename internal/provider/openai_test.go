package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sseServer emits a realistic streaming response: role frame, split content,
// a tool call whose JSON arguments arrive in fragments, a finish frame, and a
// trailing usage-only frame. Every one of those shapes appears in the wild.
func sseServer(t *testing.T) *httptest.Server {
	t.Helper()
	frames := []string{
		`{"model":"test-model","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"Hel"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"lo"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"main.go\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}}`,
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			fmt.Fprint(w, `{"data":[{"id":"test-model"}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i, f := range frames {
			// Two gaps, both far above the ~0.6 ms clock granularity: one
			// before the first content frame so TTFT is measurable, and one
			// mid-stream so the generation span is distinguishable from TTFT.
			// Without the second gap the whole stream lands in a single clock
			// tick and TTFT == Latency.
			if i == 1 || i == 4 {
				time.Sleep(20 * time.Millisecond)
			}
			fmt.Fprintf(w, "data: %s\n\n", f)
			fl.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
}

func TestStreamAssemblesContentAndToolCalls(t *testing.T) {
	srv := sseServer(t)
	defer srv.Close()

	c := NewOpenAICompat(OpenAIOpts{Name: "test", BaseURL: srv.URL + "/v1", APIKey: "k", StreamUsage: true})

	var seen strings.Builder
	resp, err := c.Stream(context.Background(), "test-model", Request{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}, func(ch Chunk) error {
		seen.WriteString(ch.Content)
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	if got := resp.Content; got != "Hello" {
		t.Errorf("content = %q, want %q", got, "Hello")
	}
	if got := seen.String(); got != "Hello" {
		t.Errorf("streamed callback content = %q, want %q", got, "Hello")
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "read_file" {
		t.Errorf("tool call identity = %+v", tc)
	}
	// The whole point of index-keyed accumulation: fragments must rejoin
	// into valid JSON, not two half-objects.
	if want := `{"path":"main.go"}`; tc.Function.Arguments != want {
		t.Errorf("tool args = %q, want %q", tc.Function.Arguments, want)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("finish = %q", resp.FinishReason)
	}
	if resp.Usage.PromptTokens != 100 || resp.Usage.CompletionTokens != 20 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if resp.Usage.Estimated {
		t.Error("usage should be reported, not estimated")
	}
	if !resp.Streamed {
		t.Error("Streamed should be true for a streaming call")
	}
	if resp.TTFT <= 0 || resp.TTFT >= resp.Latency {
		t.Errorf("ttft %v not within (0, latency %v)", resp.TTFT, resp.Latency)
	}
	if resp.DecodeTPS() <= 0 {
		t.Errorf("decode tps = %v, want > 0", resp.DecodeTPS())
	}
	if resp.PrefillTPS() <= 0 {
		t.Errorf("prefill tps = %v, want > 0", resp.PrefillTPS())
	}
}

// A non-streaming call genuinely has no first-token signal. Reporting zero is
// correct; synthesising a TTFT from total latency would put a fabricated
// prefill rate straight into the benchmark table.
func TestNonStreamingReportsNoThroughputRatherThanAFakeOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A loopback round trip can finish inside a single ~0.6 ms clock tick,
		// which makes time.Since legitimately return 0. Delaying past the tick
		// keeps the latency assertion deterministic instead of flaky.
		time.Sleep(5 * time.Millisecond)
		fmt.Fprint(w, `{"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":50,"completion_tokens":10,"total_tokens":60}}`)
	}))
	defer srv.Close()

	c := NewOpenAICompat(OpenAIOpts{Name: "ns", BaseURL: srv.URL + "/v1", APIKey: "k"})
	resp, err := c.Chat(context.Background(), "m", Request{Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Streamed {
		t.Error("Streamed should be false for a non-streaming call")
	}
	if resp.TTFT != 0 {
		t.Errorf("TTFT = %v, want 0 (no first-token signal exists)", resp.TTFT)
	}
	if resp.PrefillTPS() != 0 || resp.DecodeTPS() != 0 {
		t.Errorf("throughput = %v/%v, want 0/0", resp.PrefillTPS(), resp.DecodeTPS())
	}
	if resp.Latency < ClockFloor {
		t.Errorf("latency = %v, want at least the %v clock floor", resp.Latency, ClockFloor)
	}
}

// A stream that completes inside one clock tick must report "unmeasurable",
// not a division-by-noise result.
func TestSubTickStreamReportsUnmeasurableRate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":50,\"total_tokens\":55}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	c := NewOpenAICompat(OpenAIOpts{Name: "fast", BaseURL: srv.URL + "/v1", APIKey: "k", StreamUsage: true})
	resp, err := c.Stream(context.Background(), "m", Request{Messages: []Message{{Role: RoleUser, Content: "x"}}}, nil)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if got := resp.Latency - resp.TTFT; got < ClockFloor {
		if resp.DecodeTPS() != 0 {
			t.Errorf("decode span %v is below the %v floor but reported %v tok/s",
				got, ClockFloor, resp.DecodeTPS())
		}
	}
}

func TestStreamEstimatesUsageWhenProviderOmitsIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"abc\"}}]}\n\n")
		fl.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	c := NewOpenAICompat(OpenAIOpts{Name: "noUsage", BaseURL: srv.URL + "/v1", APIKey: "k"})
	resp, err := c.Stream(context.Background(), "m", Request{
		Messages: []Message{{Role: RoleUser, Content: "hello world"}},
	}, nil)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if !resp.Usage.Estimated {
		t.Error("usage should be flagged Estimated when the provider omits it")
	}
	if resp.Usage.TotalTokens <= 0 {
		t.Error("estimated usage should still be non-zero")
	}
}

func TestFlexContentAcceptsArrayForm(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":[{"type":"text","text":"part1 "},{"type":"text","text":"part2"}]},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`)
	}))
	defer srv.Close()

	c := NewOpenAICompat(OpenAIOpts{Name: "arr", BaseURL: srv.URL + "/v1", APIKey: "k"})
	resp, err := c.Chat(context.Background(), "m", Request{Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Content != "part1 part2" {
		t.Errorf("content = %q", resp.Content)
	}
}

func TestErrorClassification(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   ErrKind
	}{
		{429, "rate limit reached for model", ErrRateLimit},
		{429, "You exceeded your current quota for the day", ErrQuota},
		{401, "invalid api key", ErrAuth},
		{403, "forbidden", ErrAuth},
		{404, "not found", ErrModelNotFound},
		{400, "This model's maximum context length is 8192 tokens", ErrContextLength},
		{400, "invalid model: foo", ErrModelNotFound},
		{400, "missing required field", ErrBadRequest},
		{402, "payment required", ErrQuota},
		{413, "payload too large", ErrContextLength},
		{500, "internal", ErrServer},
		{503, "overloaded", ErrServer},
	}
	for _, c := range cases {
		if got := classifyHTTP(c.status, c.body, 0); got != c.want {
			t.Errorf("classifyHTTP(%d, %q) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		val  string
		want time.Duration
	}{
		{"5", 5 * time.Second},
		{"2.5", 2500 * time.Millisecond},
		{"1m30s", 90 * time.Second},
		{"", 0},
	}
	for _, c := range cases {
		h := http.Header{}
		if c.val != "" {
			h.Set("Retry-After", c.val)
		}
		if got := parseRetryAfter(h); got != c.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", c.val, got, c.want)
		}
	}
}

func TestConfiguredRequiresKeyOnlyWhenDeclared(t *testing.T) {
	needsKey := NewOpenAICompat(OpenAIOpts{Name: "hosted", BaseURL: "http://x/v1", RequiresKey: true})
	if needsKey.Configured() {
		t.Error("hosted provider without a key should report unconfigured")
	}
	local := NewOpenAICompat(OpenAIOpts{Name: "local", BaseURL: "http://x/v1", RequiresKey: false})
	if !local.Configured() {
		t.Error("local provider without a key should report configured")
	}
}
