// Package compact shrinks a conversation that is approaching the context
// window by replacing its middle with a summary.
//
// The head (system prompt and original task) and the tail (recent turns) stay
// verbatim. Only the middle — the exploration that has already served its
// purpose — gets collapsed. That ordering matters: the model needs the goal
// and the current state exactly, and the path between them only in outline.
package compact

import (
	"context"
	"fmt"
	"strings"

	"github.com/VEER-TARGARYEN/forge/internal/provider"
	"github.com/VEER-TARGARYEN/forge/internal/router"
)

type Options struct {
	// Class is the routing class used for the summarisation call. Default
	// "cheap": this is exactly the kind of call not worth a hosted quota.
	Class string
	// KeepTail is how many recent messages survive verbatim.
	KeepTail int
	// MinCollapse is the fewest middle messages worth summarising. Below
	// this, a summarisation round trip costs more than it saves.
	MinCollapse int
	// MaxRenderChars bounds the transcript handed to the summariser, so
	// compaction itself cannot overflow the window it is trying to relieve.
	MaxRenderChars int
}

func (o *Options) applyDefaults() {
	if o.Class == "" {
		o.Class = "cheap"
	}
	if o.KeepTail <= 0 {
		o.KeepTail = 6
	}
	if o.MinCollapse <= 0 {
		o.MinCollapse = 4
	}
	if o.MaxRenderChars <= 0 {
		o.MaxRenderChars = 48000
	}
}

type Result struct {
	Messages     []provider.Message
	Collapsed    int
	BeforeTokens int
	AfterTokens  int
	Summary      string
}

const summaryPrompt = `You are compacting an engineering session transcript so the work can continue
with less context. Produce a dense brief, not prose. Cover only what a
colleague resuming this task would need:

FILES: paths touched or read, and the one fact about each that mattered.
CHANGES: what was actually edited, and to what effect.
COMMANDS: what was run and the outcome (exit status, failing test names).
FAILURES: what went wrong and whether it was resolved.
OPEN: what is still not done.

Rules:
- Keep exact file paths, symbol names, error strings, and line numbers. These
  are the parts that cannot be reconstructed.
- Drop reasoning, restatements, and anything already superseded.
- If something was tried and abandoned, say so in one line, so it is not retried.
- No preamble and no closing remarks. Under 500 words.`

// Run compacts msgs. When there is not enough middle to be worth collapsing it
// returns the input unchanged with Collapsed == 0.
func Run(ctx context.Context, rt *router.Router, msgs []provider.Message, opts Options) (*Result, error) {
	opts.applyDefaults()
	before := estimate(msgs)

	headEnd := headBoundary(msgs)
	tailStart := tailBoundary(msgs, headEnd, opts.KeepTail)

	if tailStart-headEnd < opts.MinCollapse {
		return &Result{Messages: msgs, BeforeTokens: before, AfterTokens: before}, nil
	}

	middle := msgs[headEnd:tailStart]
	transcript := render(middle, opts.MaxRenderChars)

	resp, err := rt.Chat(ctx, opts.Class, provider.Request{
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: summaryPrompt},
			{Role: provider.RoleUser, Content: transcript},
		},
		Temperature: 0.1,
		MaxTokens:   1200,
	})
	if err != nil {
		return nil, fmt.Errorf("summarise for compaction: %w", err)
	}
	summary := strings.TrimSpace(resp.Content)
	if summary == "" {
		return nil, fmt.Errorf("summariser returned nothing")
	}

	out := make([]provider.Message, 0, headEnd+1+len(msgs)-tailStart)
	out = append(out, msgs[:headEnd]...)
	out = append(out, provider.Message{
		Role: provider.RoleUser,
		Content: fmt.Sprintf(
			"[%d earlier messages were compacted to save context. Summary of the work so far:]\n\n%s",
			len(middle), summary),
	})
	out = append(out, msgs[tailStart:]...)

	return &Result{
		Messages:     out,
		Collapsed:    len(middle),
		BeforeTokens: before,
		AfterTokens:  estimate(out),
		Summary:      summary,
	}, nil
}

// headBoundary returns the index just past the messages that must never be
// compacted: the system prompt and the original task. Losing either one is how
// an agent forgets what it was asked to do.
func headBoundary(msgs []provider.Message) int {
	i := 0
	for i < len(msgs) && msgs[i].Role == provider.RoleSystem {
		i++
	}
	if i < len(msgs) && msgs[i].Role == provider.RoleUser {
		i++
	}
	return i
}

// tailBoundary picks where the verbatim tail starts, then slides it forward
// off any tool message.
//
// A tool result whose originating assistant tool_calls message was compacted
// away is a dangling reference, and providers reject the request outright. The
// boundary must land on a message that stands alone.
func tailBoundary(msgs []provider.Message, headEnd, keepTail int) int {
	start := len(msgs) - keepTail
	if start < headEnd {
		start = headEnd
	}
	for start < len(msgs) && msgs[start].Role == provider.RoleTool {
		start++
	}
	return start
}

// render turns messages into a transcript for the summariser. When it exceeds
// the cap the oldest part is dropped, because the recent middle is closer to
// the current state and therefore more load-bearing.
func render(msgs []provider.Message, maxChars int) string {
	var parts []string
	for _, m := range msgs {
		var b strings.Builder
		fmt.Fprintf(&b, "[%s]", strings.ToUpper(m.Role))
		if m.Name != "" {
			fmt.Fprintf(&b, " %s", m.Name)
		}
		b.WriteString("\n")
		if c := strings.TrimSpace(m.Content); c != "" {
			b.WriteString(c)
			b.WriteString("\n")
		}
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(&b, "called %s(%s)\n", tc.Function.Name, clip(tc.Function.Arguments, 300))
		}
		parts = append(parts, b.String())
	}

	full := strings.Join(parts, "\n")
	if len(full) <= maxChars {
		return full
	}
	dropped := 0
	for len(full) > maxChars && len(parts) > 1 {
		parts = parts[1:]
		dropped++
		full = strings.Join(parts, "\n")
	}
	return fmt.Sprintf("[%d oldest messages omitted from this transcript]\n\n%s", dropped, full)
}

func clip(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func estimate(msgs []provider.Message) int {
	return provider.Request{Messages: msgs}.TokenEstimate()
}
