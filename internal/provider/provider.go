// Package provider defines the wire-level contract every model backend must
// satisfy. Everything above this package (router, agent, bench) is written
// against these types only, so adding a backend never touches callers.
package provider

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// Message roles. These match the OpenAI chat schema because every free
// provider we care about speaks it, including llama-server and Ollama.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// FunctionCall is the payload of a tool invocation. Arguments stays a raw JSON
// string rather than a map because small models emit malformed JSON often
// enough that we want to hold the original text for repair.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`

	// ExtraContent carries provider-specific data that must survive the
	// round trip. Gemini 3 returns a `thought_signature` here on every tool
	// call and hard-rejects (HTTP 400) the next request if the assistant turn
	// is replayed without it. Preserved verbatim and echoed back; omitempty
	// keeps it off the wire for providers that never set it.
	ExtraContent json.RawMessage `json:"extra_content,omitempty"`
}

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// Request is backend-agnostic. Zero values mean "provider default" so callers
// only set what they care about.
type Request struct {
	Messages    []Message
	Tools       []Tool
	ToolChoice  string // "", "auto", "none", "required"
	Temperature float64
	TopP        float64
	MaxTokens   int
	Stop        []string
	Seed        *int

	// JSONSchema, when set, forces structured output. This is the main
	// reliability lever for 7B-class models: constrained decoding turns
	// "usually valid JSON" into "always valid JSON".
	JSONSchema map[string]any
	SchemaName string
}

// TokenEstimate is a cheap character-based approximation used for context-fit
// checks before a call. ~3.5 chars/token is a reasonable average for code plus
// prose; the per-message constant covers role and delimiter overhead.
func (r Request) TokenEstimate() int {
	n := 0
	for _, m := range r.Messages {
		n += len(m.Content)/7*2 + 8
		for _, tc := range m.ToolCalls {
			n += len(tc.Function.Arguments)/7*2 + len(tc.Function.Name)/4 + 8
		}
	}
	for _, t := range r.Tools {
		n += len(t.Function.Name)/4 + len(t.Function.Description)/4 + 40
	}
	return n
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// Estimated is true when the provider did not report usage and we fell
	// back to character counting. Bench output flags these so you never
	// compare a measured tok/s against a guessed one.
	Estimated bool `json:"estimated,omitempty"`
}

type Response struct {
	Content      string
	Reasoning    string
	ToolCalls    []ToolCall
	FinishReason string
	Usage        Usage
	Model        string
	Provider     string

	// Latency is wall time for the whole call. TTFT is time to first token,
	// which for a local model is essentially prefill time.
	//
	// Timing caveat: Go's monotonic clock is not arbitrarily fine-grained.
	// On Windows it ticks at roughly 0.5-1 ms, so a whole short stream can
	// complete inside a single tick and make TTFT == Latency. Streamed
	// records whether TTFT was actually observed, so a same-tick measurement
	// is never mistaken for a missing one.
	Latency  time.Duration
	TTFT     time.Duration
	Streamed bool
}

// PrefillTPS approximates prompt-processing throughput. For hosted providers
// this is polluted by network RTT and queueing; only trust it locally.
// Returns 0 when there is no meaningful measurement rather than a fabricated
// one — a non-streaming call has no first-token signal at all.
func (r *Response) PrefillTPS() float64 {
	if r == nil || !r.Streamed || r.TTFT <= 0 || r.Usage.PromptTokens == 0 {
		return 0
	}
	return float64(r.Usage.PromptTokens) / r.TTFT.Seconds()
}

// DecodeTPS is generation throughput, measured after the first token so
// prefill and queueing are excluded. This is the number that decides whether
// an agent loop is usable.
func (r *Response) DecodeTPS() float64 {
	if r == nil || !r.Streamed || r.TTFT <= 0 || r.Usage.CompletionTokens <= 1 {
		return 0
	}
	gen := r.Latency - r.TTFT
	// Below the clock floor the ratio is noise, not a measurement.
	if gen < ClockFloor {
		return 0
	}
	return float64(r.Usage.CompletionTokens-1) / gen.Seconds()
}

// ClockFloor is the smallest duration worth dividing by. Measured granularity
// on Windows is ~0.6 ms; 2 ms leaves margin without discarding real data,
// since any generation long enough to matter spans hundreds of ticks.
const ClockFloor = 2 * time.Millisecond

// Chunk is one streaming delta.
type Chunk struct {
	Content   string
	Reasoning string
	// ToolCallIndex is the slot this delta belongs to; providers stream tool
	// call arguments in fragments keyed by index.
	ToolCallIndex int
	ToolCallDelta *FunctionCall
	ToolCallID    string
}

type Provider interface {
	Name() string
	// Chat performs a non-streaming completion.
	Chat(ctx context.Context, model string, req Request) (*Response, error)
	// Stream performs a streaming completion, calling onChunk for each delta.
	// It still returns the fully assembled Response.
	Stream(ctx context.Context, model string, req Request, onChunk func(Chunk) error) (*Response, error)
	// ListModels queries the backend for available model ids. Used by
	// `forge models` so you never have to guess a model name.
	ListModels(ctx context.Context) ([]string, error)
	// Configured reports whether this provider has everything it needs
	// (typically an API key) to be attempted at all.
	Configured() bool
}

// EstimateUsage fills in a character-based usage estimate for providers that
// omit it. Marked Estimated so downstream accounting stays honest.
func EstimateUsage(req Request, out string) Usage {
	p := req.TokenEstimate()
	c := len(out) / 7 * 2
	return Usage{PromptTokens: p, CompletionTokens: c, TotalTokens: p + c, Estimated: true}
}

// JoinToolArgs concatenates streamed argument fragments in index order.
func JoinToolArgs(frags map[int]*ToolCall) []ToolCall {
	if len(frags) == 0 {
		return nil
	}
	max := -1
	for i := range frags {
		if i > max {
			max = i
		}
	}
	out := make([]ToolCall, 0, len(frags))
	for i := 0; i <= max; i++ {
		if tc, ok := frags[i]; ok {
			if tc.Type == "" {
				tc.Type = "function"
			}
			tc.Function.Arguments = strings.TrimSpace(tc.Function.Arguments)
			out = append(out, *tc)
		}
	}
	return out
}
