package gui

import (
	"fmt"
	"sync"

	"github.com/VEER-TARGARYEN/forge/internal/approval"
	"github.com/VEER-TARGARYEN/forge/internal/tools"
)

// observer forwards agent progress into a session's event log.
//
// It exists so the agent package never learns what a browser is: the agent
// calls the same Observer interface the terminal renderer implements, and this
// type turns those calls into events on a wire.
type observer struct{ s *Session }

func (o *observer) SetStep(n int) {
	o.s.setStep(n)
	o.s.log.emit(Event{Kind: KindStep, Step: n})
}

func (o *observer) SetActivity(format string, args ...any) {
	o.s.log.emit(Event{Kind: KindActivity, Text: fmt.Sprintf(format, args...)})
}

func (o *observer) AddUsage(prov, model string, prompt, completion int) {
	o.s.addUsage(prompt, completion)
	o.s.log.emit(Event{
		Kind: KindUsage, Provider: prov, Model: model,
		Prompt: prompt, Completion: completion,
	})
}

func (o *observer) SetCounts(subAgents, changed int) {}

func (o *observer) OnText(delta string) { o.s.log.appendText(delta) }

func (o *observer) OnToolCall(id, name, args string) {
	o.s.log.emit(Event{Kind: KindToolCall, ID: id, Name: name, Args: args})
}

func (o *observer) OnToolResult(id string, ok bool, summary string) {
	o.s.log.emit(Event{Kind: KindToolRes, ID: id, OK: &ok, Summary: summary})
}

func (o *observer) OnFileChanged(path string) {
	o.s.noteChanged(path)
	o.s.log.emit(Event{Kind: KindFile, Path: path})
}

// webApprover gates mutations on an answer from the browser.
//
// The policy is consulted first, exactly as the console approver does, so
// auto-edit and yolo never make a round trip. Only a genuine Prompt verdict
// reaches the user, and then the calling tool blocks until an answer arrives
// or the run is cancelled — the same contract the terminal has, with a network
// hop in the middle.
type webApprover struct {
	s      *Session
	policy *approval.Policy

	mu      sync.Mutex
	pending map[string]chan string
	nextID  int
}

func newWebApprover(s *Session, p *approval.Policy) *webApprover {
	return &webApprover{s: s, policy: p, pending: map[string]chan string{}}
}

func (a *webApprover) Approve(req tools.ApprovalRequest) error {
	if a.policy.Aborted() {
		return &approval.Aborted{}
	}
	// Interactive is true: a browser is always able to answer, unlike a pipe.
	switch v := a.policy.Decide(req, true); v.Decision {
	case approval.Allow:
		return nil
	case approval.Deny:
		return &approval.Denied{Reason: v.Reason}
	}

	a.mu.Lock()
	a.nextID++
	id := fmt.Sprintf("ap_%d", a.nextID)
	ch := make(chan string, 1)
	a.pending[id] = ch
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		delete(a.pending, id)
		a.mu.Unlock()
	}()

	a.s.log.emit(Event{
		Kind: KindApproval, ID: id,
		Name: req.Tool, Summary: req.Summary, Detail: req.Detail,
		Path: req.Path, EditKind: req.Kind, Risky: req.Risky,
	})

	select {
	case answer := <-ch:
		a.s.log.emit(Event{Kind: KindApproved, ID: id, Decision: answer})
		switch answer {
		case "approve":
			return nil
		case "always":
			// "Always" is scoped to this tool for this session only, and the
			// policy still refuses to extend it to destructive calls.
			a.policy.RememberAlways(req.Tool)
			return nil
		case "abort":
			a.policy.Abort()
			return &approval.Aborted{}
		default:
			return &approval.Denied{Reason: "declined in the browser"}
		}
	case <-a.s.ctx.Done():
		// Cancelling a run must not leave a tool blocked on an answer that is
		// never coming.
		return &approval.Aborted{}
	}
}

// resolve delivers a decision from the browser. It reports whether the request
// was still outstanding, so a double click or a stale tab gets a clear 404
// rather than silently doing nothing.
func (a *webApprover) resolve(id, decision string) bool {
	a.mu.Lock()
	ch, ok := a.pending[id]
	if ok {
		delete(a.pending, id)
	}
	a.mu.Unlock()
	if !ok {
		return false
	}
	ch <- decision
	return true
}
