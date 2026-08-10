package ui

import (
	"fmt"
	"strings"
)

// Pager lays out scrollable content inside a fixed viewport.
//
// It exists because the previous approval flow dumped a diff into scrollback:
// on anything longer than a screen, the top scrolled away before you could
// read it, and the one thing you were being asked to judge was the part you
// could not see.
//
// All of this is pure — content and a viewport in, lines out — so the paging
// arithmetic, which is where the off-by-ones live, is testable with no
// terminal attached.
type Pager struct {
	Title  string
	Lines  []string
	Footer string

	// Top is the first content line shown, zero-based.
	Top int
}

// ChromeLines is the number of viewport rows spent on the title, separator,
// and footer.
const ChromeLines = 3

// Viewport returns how many content lines fit in a window of the given height.
func (p *Pager) Viewport(rows int) int {
	n := rows - ChromeLines
	if n < 1 {
		return 1
	}
	return n
}

// MaxTop is the largest scroll offset that still fills the viewport, so
// scrolling to the end never leaves a screen of blank space below the content.
func (p *Pager) MaxTop(rows int) int {
	m := len(p.Lines) - p.Viewport(rows)
	if m < 0 {
		return 0
	}
	return m
}

// Scroll moves the viewport by delta lines and clamps to the content.
func (p *Pager) Scroll(delta, rows int) {
	p.Top += delta
	p.clamp(rows)
}

// ScrollPage moves by whole screens, keeping one line of overlap so the reader
// has an anchor across the jump.
func (p *Pager) ScrollPage(dir, rows int) {
	step := p.Viewport(rows) - 1
	if step < 1 {
		step = 1
	}
	p.Top += dir * step
	p.clamp(rows)
}

func (p *Pager) ToTop()              { p.Top = 0 }
func (p *Pager) ToEnd(rows int)      { p.Top = p.MaxTop(rows) }
func (p *Pager) AtEnd(rows int) bool { return p.Top >= p.MaxTop(rows) }

func (p *Pager) clamp(rows int) {
	if max := p.MaxTop(rows); p.Top > max {
		p.Top = max
	}
	if p.Top < 0 {
		p.Top = 0
	}
}

// Render produces exactly the lines to draw, already truncated to width.
//
// Returning a fixed number of rows matters: the caller erases and redraws a
// region of known height, and a short frame would leave the previous frame's
// last lines on screen.
func (p *Pager) Render(cols, rows int, st Style) []string {
	if cols < 20 {
		cols = 20
	}
	view := p.Viewport(rows)
	p.clamp(rows)

	out := make([]string, 0, rows)

	// Title, with a scroll position when the content does not fit.
	title := p.Title
	if len(p.Lines) > view {
		shownTo := p.Top + view
		if shownTo > len(p.Lines) {
			shownTo = len(p.Lines)
		}
		title = fmt.Sprintf("%s  (%d-%d of %d)", p.Title, p.Top+1, shownTo, len(p.Lines))
	}
	out = append(out, TruncateVisible(st.Bold(title), cols))
	out = append(out, st.Grey(strings.Repeat("─", minInt(cols, 78))))

	for i := 0; i < view; i++ {
		idx := p.Top + i
		if idx >= len(p.Lines) {
			out = append(out, "")
			continue
		}
		out = append(out, TruncateVisible(st.DiffLine(p.Lines[idx]), cols))
	}

	footer := p.Footer
	if len(p.Lines) > view && !p.AtEnd(rows) {
		footer += st.Yellow("   ↓ more below")
	}
	out = append(out, TruncateVisible(footer, cols))
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
