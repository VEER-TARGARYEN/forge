package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/VEER-TARGARYEN/forge/internal/approval"
	"github.com/VEER-TARGARYEN/forge/internal/term"
	"github.com/VEER-TARGARYEN/forge/internal/tools"
	"github.com/VEER-TARGARYEN/forge/internal/ui"
)

// uiLabel describes which renderer was actually chosen, so a silent fallback
// to plain is visible rather than mysterious.
func uiLabel(d *display) string {
	if d.rich {
		return "rich (live status, scrollable approvals)"
	}
	if !term.IsTTY(os.Stdin) {
		return "plain (stdin is not a terminal)"
	}
	return "plain"
}

// UIMode selects the renderer.
type UIMode string

const (
	UIAuto  UIMode = "auto"
	UIRich  UIMode = "rich"
	UIPlain UIMode = "plain"
)

func parseUIMode(s string) (UIMode, error) {
	switch UIMode(strings.ToLower(strings.TrimSpace(s))) {
	case UIAuto, "":
		return UIAuto, nil
	case UIRich:
		return UIRich, nil
	case UIPlain:
		return UIPlain, nil
	}
	return "", fmt.Errorf("unknown ui mode %q (use auto, rich, or plain)", s)
}

// display bundles everything the run needs to render itself, so cmdDo does not
// branch on rich-vs-plain at every call site.
type display struct {
	rich     bool
	screen   *ui.Screen
	status   *ui.Status
	approver tools.Approver
	out      io.Writer

	restore  func()
	stopTick chan struct{}
	once     sync.Once
}

// newDisplay decides how to render.
//
// Rich mode needs three separate things to be true: stdout must accept ANSI,
// stdin must be a terminal we can read keys from, and raw mode must actually
// engage. Any one of them failing drops to plain rather than producing a
// half-working interface — a pinned status region on a terminal that ignores
// escape sequences is worse than no status region at all.
func newDisplay(mode UIMode, policy *approval.Policy, class string, maxSteps, budget int) *display {
	d := &display{out: os.Stderr}

	stdinTTY := term.IsTTY(os.Stdin)
	wantRich := mode == UIRich || (mode == UIAuto && stdinTTY && term.SupportsANSI(os.Stderr))

	if !wantRich {
		d.screen = ui.NewPlainScreen(os.Stderr, 100)
		d.approver = &approval.Console{
			Policy: policy, In: os.Stdin, Out: os.Stderr, Interactive: stdinTTY,
		}
		d.out = d.screen
		return d
	}

	screen := ui.NewScreen(os.Stderr)
	if !screen.ANSI() {
		d.screen = ui.NewPlainScreen(os.Stderr, 100)
		d.approver = &approval.Console{
			Policy: policy, In: os.Stdin, Out: os.Stderr, Interactive: stdinTTY,
		}
		d.out = d.screen
		return d
	}

	restore, err := term.MakeRaw(os.Stdin, os.Stderr)
	if err != nil {
		// Without raw mode, single-keystroke answers are impossible. The
		// line-based console still works, so use it.
		d.screen = screen
		d.approver = &approval.Console{
			Policy: policy, In: os.Stdin, Out: os.Stderr, Interactive: stdinTTY,
		}
		d.out = d.screen
		return d
	}

	d.rich = true
	d.screen = screen
	d.restore = restore
	d.status = ui.NewStatus(class, maxSteps, budget)
	d.approver = ui.NewApprover(policy, screen, true)
	d.out = screen
	d.startTicker()
	return d
}

// startTicker animates the spinner and the elapsed clock.
//
// Four frames a second: fast enough to read as alive, slow enough that a
// terminal over SSH is not repainting constantly.
func (d *display) startTicker() {
	d.stopTick = make(chan struct{})
	go func() {
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-d.stopTick:
				return
			case <-t.C:
				d.status.Tick()
				d.refresh()
			}
		}
	}()
}

func (d *display) refresh() {
	if !d.rich || d.status == nil {
		return
	}
	d.screen.SetStatus(d.status.Render(d.screen.Cols(), d.screen.Style)...)
}

// Writer is where the agent's transcript goes.
func (d *display) Writer() io.Writer { return d.out }

// Printf writes one line. Both modes go through the screen so line handling is
// identical: the plain screen is a pass-through that still terminates lines,
// which a bare Fprintf does not.
func (d *display) Printf(format string, args ...any) {
	d.screen.Printf(format, args...)
}

func (d *display) SetActivity(format string, args ...any) {
	if d.status != nil {
		d.status.SetActivity(format, args...)
		d.refresh()
	}
}

func (d *display) SetStep(n int) {
	if d.status != nil {
		d.status.SetStep(n)
		d.refresh()
	}
}

func (d *display) AddUsage(provider, model string, prompt, completion int) {
	if d.status != nil {
		d.status.SetTarget(provider, model)
		d.status.AddUsage(prompt, completion)
		d.refresh()
	}
}

func (d *display) SetCounts(subAgents, changed int) {
	if d.status != nil {
		d.status.SetCounts(subAgents, changed)
		d.refresh()
	}
}

// Close tears the UI down. Safe to call more than once, and deferred early so
// a panic or a signal still restores the terminal — leaving a shell in raw
// mode is the one failure a TUI must never have.
func (d *display) Close() {
	d.once.Do(func() {
		if d.stopTick != nil {
			close(d.stopTick)
		}
		if d.screen != nil {
			d.screen.Flush()
			d.screen.ClearStatus()
		}
		if d.restore != nil {
			d.restore()
		}
	})
}
