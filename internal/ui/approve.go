package ui

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/VEER-TARGARYEN/forge/internal/approval"
	"github.com/VEER-TARGARYEN/forge/internal/term"
	"github.com/VEER-TARGARYEN/forge/internal/tools"
)

// Approver is the terminal approval surface.
//
// It shares its policy with the plain console rather than reimplementing it,
// so the two can never disagree about whether something is gated. All this
// adds is the presentation: a scrollable diff, single-keystroke answers, and a
// destructive path that still demands a typed word.
type Approver struct {
	policy *approval.Policy
	screen *Screen
	in     *bufio.Reader
	rows   func() int
	// interactive is false when there is no usable terminal; the policy then
	// denies anything that would need a prompt.
	interactive bool
	// drawn is the height of the review pane currently on screen.
	drawn int
}

func NewApprover(policy *approval.Policy, screen *Screen, interactive bool) *Approver {
	return &Approver{
		policy: policy, screen: screen,
		in:          bufio.NewReader(os.Stdin),
		interactive: interactive,
		rows: func() int {
			_, r := term.Size(os.Stdout)
			return r
		},
	}
}

// SetInput redirects keystroke reading, for tests.
func (a *Approver) SetInput(r *bufio.Reader, rows func() int) {
	a.in = r
	a.rows = rows
}

func (a *Approver) Approve(req tools.ApprovalRequest) error {
	if a.policy.Aborted() {
		return &approval.Aborted{}
	}
	switch v := a.policy.Decide(req, a.interactive); v.Decision {
	case approval.Allow:
		a.screen.Printf("  %s %s", a.screen.Style.Grey("·"),
			a.screen.Style.Grey(req.Summary+"  ["+v.Reason+"]"))
		return nil
	case approval.Deny:
		return &approval.Denied{Reason: v.Reason}
	}
	return a.prompt(req)
}

// prompt runs the interactive review.
func (a *Approver) prompt(req tools.ApprovalRequest) error {
	st := a.screen.Style
	restore := a.screen.Suspend()
	defer restore()

	title := "  " + req.Summary
	if req.Risky {
		title = "  " + st.Red("DESTRUCTIVE") + "  " + req.Summary
	}

	pager := &Pager{
		Title: title,
		Lines: strings.Split(strings.TrimRight(req.Detail, "\n"), "\n"),
	}
	if req.Detail == "" {
		pager.Lines = nil
	}

	for {
		rows := a.viewportRows()
		pager.Footer = a.footer(req, st)

		a.draw(pager, rows, st)

		key, err := term.DecodeKey(a.in)
		if err != nil {
			a.clear(rows)
			return &approval.Denied{Reason: "no input available"}
		}

		switch {
		case key.Code == term.KeyUp:
			pager.Scroll(-1, rows)
		case key.Code == term.KeyDown:
			pager.Scroll(1, rows)
		case key.Code == term.KeyPgUp:
			pager.ScrollPage(-1, rows)
		case key.Code == term.KeyPgDn, key.IsRune(' '):
			pager.ScrollPage(1, rows)
		case key.Code == term.KeyHome:
			pager.ToTop()
		case key.Code == term.KeyEnd:
			pager.ToEnd(rows)

		case key.Code == term.KeyCtrlC, key.IsRune('q'):
			a.clear(rows)
			a.policy.Abort()
			return &approval.Aborted{}

		case key.IsRune('n'), key.IsRune('N'), key.Code == term.KeyEsc:
			a.clear(rows)
			return &approval.Denied{Reason: "user skipped this call"}

		case key.IsRune('y'), key.IsRune('Y'), key.Code == term.KeyEnter:
			if req.Risky {
				// A single keystroke is too cheap for an irreversible action.
				ok := a.confirmTyped(rows, st)
				a.clear(rows)
				if !ok {
					return &approval.Denied{Reason: "user declined a destructive operation"}
				}
				return nil
			}
			a.clear(rows)
			return nil

		case key.IsRune('a'), key.IsRune('A'):
			if req.Risky {
				// "Always" must never cover destructive operations; the whole
				// point of the flag is that each one gets its own decision.
				continue
			}
			a.clear(rows)
			a.policy.RememberAlways(req.Tool)
			return nil
		}
	}
}

func (a *Approver) footer(req tools.ApprovalRequest, st Style) string {
	keys := []string{
		st.Bold("y") + " run",
		st.Bold("n") + " skip",
	}
	if !req.Risky {
		keys = append(keys, st.Bold("a")+" always "+req.Tool)
	}
	keys = append(keys, st.Bold("q")+" abort")
	nav := st.Grey("↑↓ pgup/pgdn scroll")
	return "  " + strings.Join(keys, st.Grey("  ·  ")) + "   " + nav
}

// viewportRows leaves the transcript visible above the review pane rather than
// taking the whole window: the surrounding context is often what makes a diff
// judgeable.
func (a *Approver) viewportRows() int {
	rows := a.rows()
	if rows <= 0 {
		rows = 24
	}
	use := rows * 2 / 3
	if use < 8 {
		use = minInt(8, rows-1)
	}
	if use > 30 {
		use = 30
	}
	if use < ChromeLines+1 {
		use = ChromeLines + 1
	}
	return use
}

func (a *Approver) draw(p *Pager, rows int, st Style) {
	lines := p.Render(a.screen.Cols(), rows, st)
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\x1b[0K\n")
	}
	// Walk back to the top of the pane so the next frame overwrites this one
	// in place instead of scrolling the transcript away.
	fmt.Fprintf(&b, "\x1b[%dA\r", len(lines))
	a.screen.Raw(b.String())
	a.drawn = len(lines)
}

// clear erases the review pane once a decision is made.
func (a *Approver) clear(rows int) {
	if a.drawn == 0 {
		return
	}
	a.screen.Raw("\x1b[0J")
	a.drawn = 0
}

// confirmTyped demands the word "yes" for a destructive action.
func (a *Approver) confirmTyped(rows int, st Style) bool {
	a.screen.Raw("\x1b[0J" + "  " + st.Red("type 'yes' to run this: "))
	a.drawn = 0

	var typed strings.Builder
	for {
		key, err := term.DecodeKey(a.in)
		if err != nil {
			a.screen.Raw("\r\n")
			return false
		}
		switch {
		case key.Code == term.KeyEnter:
			a.screen.Raw("\r\n")
			return strings.EqualFold(strings.TrimSpace(typed.String()), "yes")
		case key.Code == term.KeyCtrlC, key.Code == term.KeyEsc:
			a.screen.Raw("\r\n")
			return false
		case key.Code == term.KeyBackspace:
			s := typed.String()
			if s != "" {
				typed.Reset()
				typed.WriteString(s[:len(s)-1])
				a.screen.Raw("\b \b")
			}
		case key.Code == term.KeyRune:
			typed.WriteRune(key.Rune)
			a.screen.Raw(string(key.Rune))
		}
		if typed.Len() > 16 {
			a.screen.Raw("\r\n")
			return false
		}
	}
}
