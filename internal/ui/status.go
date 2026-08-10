package ui

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Status is the model behind the pinned bottom region: what the agent is doing
// right now, and what it has cost so far.
//
// The token counter is the point. On a free-tier stack the question that
// actually matters mid-run is "how much of my budget has this eaten", and
// discovering that only in the final summary is too late to intervene.
type Status struct {
	mu sync.Mutex

	Class    string
	Provider string
	Model    string

	Step     int
	MaxSteps int
	Activity string

	PromptTok     int
	CompletionTok int
	Budget        int

	SubAgents int
	Changed   int

	started time.Time
	tick    int
	// paused suppresses the spinner so a static frame renders identically in
	// a test.
	paused bool
}

func NewStatus(class string, maxSteps, budget int) *Status {
	return &Status{
		Class: class, MaxSteps: maxSteps, Budget: budget,
		Activity: "starting", started: time.Now(),
	}
}

func (s *Status) SetActivity(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Activity = fmt.Sprintf(format, args...)
}

func (s *Status) SetStep(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Step = n
}

func (s *Status) SetTarget(provider, model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Provider, s.Model = provider, model
}

func (s *Status) AddUsage(prompt, completion int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.PromptTok += prompt
	s.CompletionTok += completion
}

func (s *Status) SetCounts(subAgents, changed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.SubAgents, s.Changed = subAgents, changed
}

func (s *Status) Tick() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tick++
}

// Freeze stops the spinner, for a deterministic render.
func (s *Status) Freeze() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paused = true
}

// Elapsed since the run started.
func (s *Status) Elapsed() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.started)
}

// Render builds the pinned lines. Two rows: what is happening, and what it has
// cost.
func (s *Status) Render(cols int, st Style) []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	spin := SpinnerFrame(s.tick)
	if s.paused {
		spin = "-"
	}

	var top strings.Builder
	top.WriteString(st.Cyan(spin))
	top.WriteString(" ")
	if s.Step > 0 {
		fmt.Fprintf(&top, "%s ", st.Bold(fmt.Sprintf("step %d/%d", s.Step, s.MaxSteps)))
	}
	top.WriteString(s.Activity)

	var bot strings.Builder
	fmt.Fprintf(&bot, "  %s", st.Grey(roundDur(time.Since(s.started))))
	total := s.PromptTok + s.CompletionTok
	tokens := fmt.Sprintf("%s in + %s out", human(s.PromptTok), human(s.CompletionTok))
	if s.Budget > 0 {
		pct := float64(total) / float64(s.Budget) * 100
		frac := fmt.Sprintf("%s of %s (%.0f%%)", human(total), human(s.Budget), pct)
		// Colour the budget as it fills, so the warning arrives while there is
		// still room to act on it.
		switch {
		case pct >= 90:
			frac = st.Red(frac)
		case pct >= 70:
			frac = st.Yellow(frac)
		default:
			frac = st.Grey(frac)
		}
		fmt.Fprintf(&bot, "  %s  %s", st.Grey(tokens), frac)
	} else {
		fmt.Fprintf(&bot, "  %s", st.Grey(tokens))
	}
	if s.Provider != "" {
		fmt.Fprintf(&bot, "  %s", st.Grey(s.Provider+"/"+shorten(s.Model, 24)))
	}
	if s.Changed > 0 {
		fmt.Fprintf(&bot, "  %s", st.Green(fmt.Sprintf("%d changed", s.Changed)))
	}
	if s.SubAgents > 0 {
		fmt.Fprintf(&bot, "  %s", st.Blue(fmt.Sprintf("%d delegated", s.SubAgents)))
	}

	return []string{
		TruncateVisible(top.String(), cols),
		TruncateVisible(bot.String(), cols),
	}
}

func roundDur(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return d.Round(time.Second).String()
}

func human(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 1000000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%.2fM", float64(n)/1000000)
	}
}

func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Model ids carry their distinguishing part at the end far more often than
	// at the start, so keep the tail.
	return "…" + s[len(s)-n+1:]
}
