package provider

import "strings"

// Reasoning models split their scratchpad from their answer in two different
// ways, and only one of them is a protocol.
//
// The clean way is a separate `reasoning_content` field, which gpt-oss on Groq
// uses and which the wire decoder already handles. The other way is to emit
// <think>…</think> inline in the ordinary content stream — Qwen3, the
// DeepSeek-R1 distills, and several others do this. Left alone, that scratchpad
// lands in the agent's message history and is re-sent on every subsequent turn,
// so a single "ok" can cost a hundred and eighty tokens forever.
//
// thinkSplitter routes those spans to the reasoning channel instead, so
// everything downstream — the live display, the stored message, block parsing —
// sees only the answer.
const (
	thinkOpen  = "<think>"
	thinkClose = "</think>"
)

type thinkSplitter struct {
	inThink bool
	buf     string
}

// feed consumes a chunk and returns the text belonging to each channel.
//
// A tag can straddle a chunk boundary — "<thi" arriving in one frame and "nk>"
// in the next is routine — so any trailing bytes that could begin a tag are
// held back rather than emitted as content that would have to be un-printed.
func (t *thinkSplitter) feed(s string) (content, reasoning string) {
	if s == "" {
		return "", ""
	}
	t.buf += s

	var c, r strings.Builder
	for {
		if !t.inThink {
			i := strings.Index(t.buf, thinkOpen)
			if i < 0 {
				keep := partialSuffix(t.buf, thinkOpen)
				c.WriteString(t.buf[:len(t.buf)-keep])
				t.buf = t.buf[len(t.buf)-keep:]
				break
			}
			c.WriteString(t.buf[:i])
			t.buf = t.buf[i+len(thinkOpen):]
			t.inThink = true
			continue
		}

		i := strings.Index(t.buf, thinkClose)
		if i < 0 {
			keep := partialSuffix(t.buf, thinkClose)
			r.WriteString(t.buf[:len(t.buf)-keep])
			t.buf = t.buf[len(t.buf)-keep:]
			break
		}
		r.WriteString(t.buf[:i])
		t.buf = t.buf[i+len(thinkClose):]
		t.inThink = false
	}
	return c.String(), r.String()
}

// flush releases whatever is held at end of stream.
//
// An unterminated <think> is treated as reasoning rather than content: a model
// that ran out of budget mid-thought produced no answer, and surfacing its
// half-finished scratchpad as the answer would be worse than surfacing nothing.
func (t *thinkSplitter) flush() (content, reasoning string) {
	rest := t.buf
	t.buf = ""
	if t.inThink {
		return "", rest
	}
	return rest, ""
}

// partialSuffix reports how many trailing bytes of s could be the opening of
// tag, so they can be held until the next chunk resolves them.
func partialSuffix(s, tag string) int {
	max := len(tag) - 1
	if max > len(s) {
		max = len(s)
	}
	for n := max; n > 0; n-- {
		if strings.HasSuffix(s, tag[:n]) {
			return n
		}
	}
	return 0
}

// SplitThinking removes <think> spans from a complete string, returning the
// answer and the scratchpad separately. Used on the non-streaming path.
func SplitThinking(s string) (content, reasoning string) {
	var t thinkSplitter
	c, r := t.feed(s)
	c2, r2 := t.flush()
	return c + c2, r + r2
}
