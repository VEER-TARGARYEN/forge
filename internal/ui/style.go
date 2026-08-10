// Package ui renders the agent's activity to a terminal.
//
// The layout logic here is deliberately pure — strings in, strings out — with
// terminal I/O confined to a thin edge in screen.go. That split is what makes
// wrapping, truncation, and paging testable without a TTY attached, which
// matters because those are the parts that actually contain bugs.
package ui

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Style emits ANSI attributes, or nothing at all when the output cannot
// interpret them. Every call site writes the same code either way.
type Style struct{ On bool }

const (
	ansiReset  = "\x1b[0m"
	ansiDim    = "\x1b[2m"
	ansiBold   = "\x1b[1m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiBlue   = "\x1b[34m"
	ansiCyan   = "\x1b[36m"
	ansiGrey   = "\x1b[90m"
	ansiInvert = "\x1b[7m"
)

func (s Style) wrap(code, text string) string {
	if !s.On || text == "" {
		return text
	}
	return code + text + ansiReset
}

func (s Style) Dim(t string) string    { return s.wrap(ansiDim, t) }
func (s Style) Bold(t string) string   { return s.wrap(ansiBold, t) }
func (s Style) Red(t string) string    { return s.wrap(ansiRed, t) }
func (s Style) Green(t string) string  { return s.wrap(ansiGreen, t) }
func (s Style) Yellow(t string) string { return s.wrap(ansiYellow, t) }
func (s Style) Blue(t string) string   { return s.wrap(ansiBlue, t) }
func (s Style) Cyan(t string) string   { return s.wrap(ansiCyan, t) }
func (s Style) Grey(t string) string   { return s.wrap(ansiGrey, t) }
func (s Style) Invert(t string) string { return s.wrap(ansiInvert, t) }

// DiffLine colours a unified-diff line by its marker.
func (s Style) DiffLine(line string) string {
	switch {
	case strings.HasPrefix(line, "+"):
		return s.Green(line)
	case strings.HasPrefix(line, "-"):
		return s.Red(line)
	case strings.HasPrefix(line, "@@"):
		return s.Cyan(line)
	default:
		return line
	}
}

// escState tracks position within an ANSI escape sequence.
//
// The subtlety that makes this a state machine rather than a boolean: a CSI
// sequence is ESC '[' params… final, where the final byte is in 0x40..0x7E —
// and '[' is itself 0x5B, inside that range. A naive "in escape until the next
// byte in 0x40..0x7E" ends the sequence at the '[' and then counts the
// parameters as visible text, so "\x1b[31m" measures 5 wide instead of 0.
type escState int

const (
	escNone   escState = iota
	escSeen            // just consumed ESC
	escCSI             // inside ESC [ …
	escOSC             // inside ESC ] …
	escOSCEsc          // inside OSC, just saw ESC (expecting '\' to terminate)
)

// step advances the state for one rune and reports whether that rune is part
// of an escape sequence, and therefore contributes no width.
func (e *escState) step(r rune) bool {
	switch *e {
	case escNone:
		if r == 0x1b {
			*e = escSeen
			return true
		}
		return false
	case escSeen:
		switch r {
		case '[':
			*e = escCSI
		case ']':
			*e = escOSC
		default:
			*e = escNone // a two-byte sequence, already complete
		}
		return true
	case escCSI:
		if r >= 0x40 && r <= 0x7e {
			*e = escNone
		}
		return true
	case escOSC:
		switch r {
		case 0x07: // BEL terminator
			*e = escNone
		case 0x1b: // possible ST terminator: ESC \
			*e = escOSCEsc
		}
		return true
	case escOSCEsc:
		*e = escNone
		return true
	}
	return false
}

// VisibleWidth is the display width of a string, ignoring escape sequences.
//
// Measuring the raw length would make every coloured line appear far wider
// than it is and wrap in the wrong place, so truncation has to skip escapes
// rather than count them.
func VisibleWidth(s string) int {
	w := 0
	var esc escState
	for _, r := range s {
		if esc.step(r) {
			continue
		}
		switch {
		case r == '\t':
			w += 4
		case unicode.IsControl(r):
			// contributes nothing
		default:
			w++
		}
	}
	return w
}

// TruncateVisible clips a string to a visible width, preserving escape
// sequences and appending a reset so colour never bleeds past the cut.
func TruncateVisible(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if VisibleWidth(s) <= max {
		return s
	}
	var b strings.Builder
	w := 0
	var esc escState
	sawEsc := false

	for _, r := range s {
		// Escape bytes are copied through verbatim and cost no width, so
		// colour survives the cut.
		if esc.step(r) {
			sawEsc = true
			b.WriteRune(r)
			continue
		}
		cw := 1
		if r == '\t' {
			cw = 4
		} else if unicode.IsControl(r) {
			cw = 0
		}
		// Reserve one column for the ellipsis.
		if w+cw > max-1 {
			b.WriteString("…")
			if sawEsc {
				b.WriteString(ansiReset)
			}
			return b.String()
		}
		b.WriteRune(r)
		w += cw
	}
	if sawEsc {
		b.WriteString(ansiReset)
	}
	return b.String()
}

// Wrap breaks text to a width, preferring word boundaries and preserving
// existing newlines. A word longer than the width is split rather than allowed
// to overflow.
func Wrap(text string, width int) []string {
	if width <= 0 {
		width = 80
	}
	var out []string
	for _, para := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(para) == "" {
			out = append(out, "")
			continue
		}
		line := ""
		for _, word := range strings.Fields(para) {
			switch {
			case line == "":
				line = word
			case VisibleWidth(line)+1+VisibleWidth(word) <= width:
				line += " " + word
			default:
				out = append(out, line)
				line = word
			}
			// A single token wider than the terminal still has to fit.
			for VisibleWidth(line) > width {
				cut := hardCut(line, width)
				out = append(out, cut)
				line = line[len(cut):]
			}
		}
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func hardCut(s string, width int) string {
	w, n := 0, 0
	for _, r := range s {
		if w >= width {
			break
		}
		w++
		n += utf8.RuneLen(r)
	}
	if n == 0 {
		n = len(s)
	}
	return s[:n]
}

// Spinner frames. Plain ASCII: a Windows console in a legacy code page renders
// braille spinner characters as boxes.
var spinnerFrames = []string{"-", "\\", "|", "/"}

func SpinnerFrame(tick int) string {
	if tick < 0 {
		tick = -tick
	}
	return spinnerFrames[tick%len(spinnerFrames)]
}
