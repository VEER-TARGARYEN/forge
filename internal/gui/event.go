// Package gui serves the browser front end and the JSON/SSE API behind it.
//
// The split of responsibility here matters. This package owns transport —
// sessions, the event log, server-sent events, approval round trips — and
// knows nothing about how an agent is wired together. The command layer
// supplies a Backend that knows how to build and run one. That keeps the
// considerable wiring in cmdDo from being duplicated, and keeps HTTP concerns
// out of the agent.
package gui

import (
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// Event is one thing that happened in a session, as the browser sees it.
//
// It is deliberately one flat struct rather than a union: the front end
// switches on Kind and reads the fields that kind uses, and a flat shape
// survives JSON round trips without a discriminated-union decoder on either
// side. Everything optional is omitempty so the wire stays small — a long run
// is mostly text deltas, and those carry two fields.
type Event struct {
	Seq  int    `json:"seq"`
	Kind string `json:"kind"`
	T    int64  `json:"t"` // unix millis

	Text string `json:"text,omitempty"`
	Step int    `json:"step,omitempty"`

	// Tool call and result.
	ID      string `json:"id,omitempty"`
	Name    string `json:"name,omitempty"`
	Args    string `json:"args,omitempty"`
	OK      *bool  `json:"ok,omitempty"`
	Summary string `json:"summary,omitempty"`

	Path string `json:"path,omitempty"`

	// Usage.
	Provider   string `json:"provider,omitempty"`
	Model      string `json:"model,omitempty"`
	Prompt     int    `json:"prompt,omitempty"`
	Completion int    `json:"completion,omitempty"`

	// Approval.
	Detail   string `json:"detail,omitempty"`
	EditKind string `json:"editKind,omitempty"`
	Risky    bool   `json:"risky,omitempty"`
	Decision string `json:"decision,omitempty"`

	// Terminal event.
	Outcome *OutcomeView `json:"outcome,omitempty"`
}

// Event kinds. Kept as constants because both sides switch on them and a typo
// in a string literal would fail silently at runtime.
const (
	KindText      = "text"
	KindStep      = "step"
	KindActivity  = "activity"
	KindToolCall  = "tool.call"
	KindToolRes   = "tool.result"
	KindFile      = "file"
	KindUsage     = "usage"
	KindApproval  = "approval"
	KindApproved  = "approval.resolved"
	KindEnd       = "end"
	KindError     = "error"
	KindCancelled = "cancelled"
)

// OutcomeView is the run summary, shaped for display.
type OutcomeView struct {
	Steps         int      `json:"steps"`
	StopReason    string   `json:"stopReason"`
	ElapsedMS     int64    `json:"elapsedMs"`
	PromptTok     int      `json:"promptTokens"`
	CompletionTok int      `json:"completionTokens"`
	FilesChanged  []string `json:"filesChanged"`
	FinalText     string   `json:"finalText,omitempty"`
	VerifyRan     bool     `json:"verifyRan"`
	Verified      bool     `json:"verified"`
	VerifySummary string   `json:"verifySummary,omitempty"`
	Repairs       int      `json:"repairs"`
	SubAgents     int      `json:"subAgents"`
	Compactions   int      `json:"compactions"`
}

// eventLog is an append-only log with replay, and the fan-out to live
// subscribers.
//
// Replay is what makes reconnection work: a browser that reloads, sleeps, or
// loses the stream reconnects with the last sequence number it saw and gets
// everything since. Without it, a dropped connection would silently lose the
// middle of a run, which is exactly when you are watching.
type eventLog struct {
	mu   sync.Mutex
	log  []Event
	seq  int
	subs map[chan Event]struct{}

	// pending coalesces streamed prose. A model emits text a few characters
	// at a time; one event per delta would put thousands of records in the log
	// and thousands of frames on the wire to say one paragraph.
	pending strings.Builder
	closed  bool
}

func newEventLog() *eventLog {
	return &eventLog{subs: map[chan Event]struct{}{}}
}

// coalesceAt is the byte threshold for flushing buffered prose. Small enough
// that streaming still looks live, large enough that it is not one event per
// token.
const coalesceAt = 48

// appendText buffers a prose delta, flushing on a newline or once enough has
// accumulated to be worth a frame.
func (l *eventLog) appendText(s string) {
	l.mu.Lock()
	l.pending.WriteString(s)
	flush := l.pending.Len() >= coalesceAt || strings.ContainsAny(s, "\n")
	l.mu.Unlock()
	if flush {
		l.flushText()
	}
}

func (l *eventLog) flushText() {
	l.mu.Lock()
	if l.pending.Len() == 0 {
		l.mu.Unlock()
		return
	}
	text := l.pending.String()
	l.pending.Reset()
	l.mu.Unlock()
	l.emit(Event{Kind: KindText, Text: text})
}

// emit stamps, records, and fans out. Any buffered prose is flushed first so
// events never arrive out of order relative to the text around them.
func (l *eventLog) emit(ev Event) {
	if ev.Kind != KindText {
		l.flushText()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.seq++
	ev.Seq = l.seq
	ev.T = time.Now().UnixMilli()
	l.log = append(l.log, ev)

	for ch := range l.subs {
		select {
		case ch <- ev:
		default:
			// A subscriber that cannot keep up is dropped rather than allowed
			// to block the agent. It reconnects with ?from=<seq> and replays
			// the gap, so nothing is actually lost.
			delete(l.subs, ch)
			close(ch)
		}
	}
}

// since returns every event after seq, for replay on connect.
func (l *eventLog) since(seq int) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	if seq <= 0 {
		return append([]Event(nil), l.log...)
	}
	for i, ev := range l.log {
		if ev.Seq > seq {
			return append([]Event(nil), l.log[i:]...)
		}
	}
	return nil
}

// subscribe registers a live channel and returns it with the backlog after
// seq, taken under one lock so no event can slip between the two.
func (l *eventLog) subscribe(seq int) ([]Event, chan Event) {
	l.mu.Lock()
	defer l.mu.Unlock()

	var backlog []Event
	if seq <= 0 {
		backlog = append([]Event(nil), l.log...)
	} else {
		for i, ev := range l.log {
			if ev.Seq > seq {
				backlog = append([]Event(nil), l.log[i:]...)
				break
			}
		}
	}

	ch := make(chan Event, 512)
	if l.closed {
		close(ch)
		return backlog, ch
	}
	l.subs[ch] = struct{}{}
	return backlog, ch
}

func (l *eventLog) unsubscribe(ch chan Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.subs[ch]; ok {
		delete(l.subs, ch)
		close(ch)
	}
}

// closeAll ends every live stream once a run is over, so browsers stop waiting
// on a session that will never speak again.
func (l *eventLog) closeAll() {
	l.flushText()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	for ch := range l.subs {
		delete(l.subs, ch)
		close(ch)
	}
}

func (e Event) encode() []byte {
	b, err := json.Marshal(e)
	if err != nil {
		// A field that will not marshal is a bug, not a runtime condition.
		// Report it in-band rather than dropping the event silently.
		b, _ = json.Marshal(Event{Seq: e.Seq, Kind: KindError, Text: "event encode failed"})
	}
	return b
}
