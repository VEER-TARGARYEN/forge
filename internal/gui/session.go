package gui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/VEER-TARGARYEN/forge/internal/agent"
	"github.com/VEER-TARGARYEN/forge/internal/approval"
	"github.com/VEER-TARGARYEN/forge/internal/tools"
)

// RunRequest is everything the browser can choose about a run. It mirrors the
// subset of `forge do` flags that make sense to expose in a UI; the rest keep
// their defaults rather than filling the screen with knobs nobody moves.
type RunRequest struct {
	Task     string `json:"task"`
	Dir      string `json:"dir"`
	Class    string `json:"class"`
	Approval string `json:"approval"`
	Protocol string `json:"protocol"`
	MaxSteps int    `json:"maxSteps"`
	NoVerify bool   `json:"noVerify"`
}

// Backend is what the command layer must provide. Everything here needs the
// wiring that `forge do` performs — workspace, tool registry, index, verifier,
// journal — which lives in package main and is not worth duplicating.
type Backend interface {
	// Run executes one agent session to completion, routing progress through
	// obs and gating mutations through ap.
	Run(ctx context.Context, req RunRequest, obs agent.Observer, ap tools.Approver) (*agent.Outcome, error)

	Search(ctx context.Context, dir, query string, hybrid bool, limit int) ([]SearchHit, error)
	RepoMap(dir string, tokens int) (RepoMapView, error)
	ReadFile(dir, rel string) (FileView, error)
	Verify(ctx context.Context, dir string) (VerifyView, error)
	Providers(ctx context.Context, probe bool) []ProviderView
	ResetHealth()
	Usage() UsageView
	Bootstrap() BootstrapView
}

// Session is one agent run and everything the browser needs to watch it.
type Session struct {
	ID      string     `json:"id"`
	Req     RunRequest `json:"request"`
	Created time.Time  `json:"created"`

	log *eventLog

	ctx    context.Context
	cancel context.CancelFunc

	policy   *approval.Policy
	approver *webApprover

	mu         sync.Mutex
	status     string
	step       int
	prompt     int
	completion int
	changed    map[string]bool
	outcome    *OutcomeView
	errText    string
	finished   time.Time
}

// SessionView is the JSON shape for listing and detail.
type SessionView struct {
	ID           string       `json:"id"`
	Task         string       `json:"task"`
	Dir          string       `json:"dir"`
	Class        string       `json:"class"`
	Approval     string       `json:"approval"`
	Protocol     string       `json:"protocol"`
	Status       string       `json:"status"`
	Step         int          `json:"step"`
	MaxSteps     int          `json:"maxSteps"`
	Prompt       int          `json:"promptTokens"`
	Completion   int          `json:"completionTokens"`
	Changed      []string     `json:"changed"`
	Created      int64        `json:"created"`
	ElapsedMS    int64        `json:"elapsedMs"`
	Outcome      *OutcomeView `json:"outcome,omitempty"`
	Error        string       `json:"error,omitempty"`
	LastSeq      int          `json:"lastSeq"`
	PendingCount int          `json:"pendingApprovals"`
}

const (
	StatusRunning   = "running"
	StatusDone      = "done"
	StatusError     = "error"
	StatusCancelled = "cancelled"
)

// Manager owns every session for the life of the process.
type Manager struct {
	be Backend

	mu    sync.Mutex
	byID  map[string]*Session
	order []string // newest first
}

func NewManager(be Backend) *Manager {
	return &Manager{be: be, byID: map[string]*Session{}}
}

func newID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Session ids are opaque handles, not secrets. A clock-derived
		// fallback keeps the GUI working if the entropy source hiccups.
		return fmt.Sprintf("s%d", time.Now().UnixNano())
	}
	return "s_" + hex.EncodeToString(b[:])
}

// Start creates a session and runs it in the background. It returns as soon as
// the session exists so the browser can subscribe to the stream immediately —
// waiting for the first model call would hide the opening seconds of the run.
func (m *Manager) Start(req RunRequest) (*Session, error) {
	mode, err := approval.ParseMode(req.Approval)
	if err != nil {
		return nil, err
	}
	if req.MaxSteps <= 0 {
		req.MaxSteps = 30
	}
	req.Approval = string(mode)

	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{
		ID:      newID(),
		Req:     req,
		Created: time.Now(),
		log:     newEventLog(),
		ctx:     ctx,
		cancel:  cancel,
		policy:  approval.NewPolicy(mode, nil),
		status:  StatusRunning,
		changed: map[string]bool{},
	}
	s.approver = newWebApprover(s, s.policy)

	m.mu.Lock()
	m.byID[s.ID] = s
	m.order = append([]string{s.ID}, m.order...)
	m.mu.Unlock()

	go s.run(m.be)
	return s, nil
}

func (s *Session) run(be Backend) {
	defer s.cancel()

	out, err := be.Run(s.ctx, s.Req, &observer{s}, s.approver)

	s.mu.Lock()
	s.finished = time.Now()
	switch {
	case s.ctx.Err() != nil:
		// A cancelled run surfaces as a failed model call, because that is
		// what the in-flight request became. Reporting it as an error would
		// tell the user something broke when they are the one who stopped it,
		// so the cancellation is checked first and wins.
		s.status = StatusCancelled
	case err != nil:
		s.status = StatusError
		s.errText = err.Error()
	default:
		s.status = StatusDone
	}
	if out != nil {
		s.outcome = viewOutcome(out)
		s.prompt = out.Usage.PromptTokens
		s.completion = out.Usage.CompletionTokens
		for _, f := range out.FilesChanged {
			s.changed[f] = true
		}
	}
	status, view, errText := s.status, s.outcome, s.errText
	s.mu.Unlock()

	switch status {
	case StatusError:
		s.log.emit(Event{Kind: KindError, Text: errText, Outcome: view})
	case StatusCancelled:
		s.log.emit(Event{Kind: KindCancelled, Text: "cancelled", Outcome: view})
	default:
		s.log.emit(Event{Kind: KindEnd, Outcome: view})
	}
	s.log.closeAll()
}

func viewOutcome(o *agent.Outcome) *OutcomeView {
	v := &OutcomeView{
		Steps:         o.Steps,
		StopReason:    o.StopReason,
		ElapsedMS:     o.Elapsed.Milliseconds(),
		PromptTok:     o.Usage.PromptTokens,
		CompletionTok: o.Usage.CompletionTokens,
		FilesChanged:  o.FilesChanged,
		FinalText:     o.FinalText,
		VerifyRan:     o.VerifyRan,
		Verified:      o.Verified,
		VerifySummary: o.VerifySummary,
		Repairs:       o.Repairs,
		SubAgents:     o.SubAgents,
		Compactions:   o.Compactions,
	}
	if v.FilesChanged == nil {
		v.FilesChanged = []string{}
	}
	return v
}

func (s *Session) setStep(n int) {
	s.mu.Lock()
	s.step = n
	s.mu.Unlock()
}

func (s *Session) addUsage(prompt, completion int) {
	s.mu.Lock()
	s.prompt += prompt
	s.completion += completion
	s.mu.Unlock()
}

func (s *Session) noteChanged(path string) {
	s.mu.Lock()
	s.changed[path] = true
	s.mu.Unlock()
}

// Cancel stops a run. Abort is set on the policy as well as cancelling the
// context, so a tool already blocked on an approval unblocks rather than
// waiting for a click that will never come.
func (s *Session) Cancel() {
	s.policy.Abort()
	s.cancel()
}

func (s *Session) Resolve(id, decision string) bool {
	return s.approver.resolve(id, decision)
}

func (s *Session) View() SessionView {
	s.mu.Lock()
	defer s.mu.Unlock()

	changed := make([]string, 0, len(s.changed))
	for f := range s.changed {
		changed = append(changed, f)
	}
	sort.Strings(changed)

	end := s.finished
	if end.IsZero() {
		end = time.Now()
	}

	s.approver.mu.Lock()
	pending := len(s.approver.pending)
	s.approver.mu.Unlock()

	return SessionView{
		ID:           s.ID,
		Task:         s.Req.Task,
		Dir:          s.Req.Dir,
		Class:        s.Req.Class,
		Approval:     s.Req.Approval,
		Protocol:     s.Req.Protocol,
		Status:       s.status,
		Step:         s.step,
		MaxSteps:     s.Req.MaxSteps,
		Prompt:       s.prompt,
		Completion:   s.completion,
		Changed:      changed,
		Created:      s.Created.UnixMilli(),
		ElapsedMS:    end.Sub(s.Created).Milliseconds(),
		Outcome:      s.outcome,
		Error:        s.errText,
		LastSeq:      s.log.lastSeq(),
		PendingCount: pending,
	}
}

func (l *eventLog) lastSeq() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seq
}

func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.byID[id]
	return s, ok
}

func (m *Manager) List() []SessionView {
	m.mu.Lock()
	ids := append([]string(nil), m.order...)
	byID := make(map[string]*Session, len(m.byID))
	for k, v := range m.byID {
		byID[k] = v
	}
	m.mu.Unlock()

	out := make([]SessionView, 0, len(ids))
	for _, id := range ids {
		if s, ok := byID[id]; ok {
			out = append(out, s.View())
		}
	}
	return out
}

// CancelAll stops every running session, so shutting the server down does not
// leave agents editing files with nobody watching.
func (m *Manager) CancelAll() {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.byID))
	for _, s := range m.byID {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()
	for _, s := range sessions {
		s.Cancel()
	}
}
