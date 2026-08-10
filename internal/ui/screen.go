package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/VEER-TARGARYEN/forge/internal/term"
)

// Screen writes scrolling output above a pinned status region.
//
// Deliberately not a full-screen alternate-buffer TUI. A coding agent's
// transcript belongs in the terminal's own scrollback, where it can be
// scrolled, selected, and copied with the tools the user already has. Taking
// over the whole screen would trade all of that for a redraw loop.
//
// The mechanism is plain ANSI: before emitting new content, walk the cursor
// back over the status lines and erase to the end of the screen; afterwards,
// redraw them. Nothing else on the screen moves.
type Screen struct {
	mu sync.Mutex

	out    io.Writer
	Style  Style
	ansi   bool
	cols   int
	rows   int
	sizeOf func() (int, int)

	status      []string
	statusDrawn int
	partial     strings.Builder
}

// NewScreen builds a renderer for out. When ANSI is unavailable it degrades to
// plain sequential writes with no pinned region at all, which is exactly what
// you want when output is piped to a file.
func NewScreen(out *os.File) *Screen {
	ansi := term.SupportsANSI(out)
	s := &Screen{
		out:   out,
		ansi:  ansi,
		Style: Style{On: ansi},
		sizeOf: func() (int, int) {
			return term.Size(out)
		},
	}
	s.cols, s.rows = s.sizeOf()
	return s
}

// NewTestScreen renders to an arbitrary writer with ANSI forced on or off, so
// the control sequences the renderer emits can be asserted directly.
func NewTestScreen(out io.Writer, cols int, ansi bool) *Screen {
	if cols <= 0 {
		cols = 80
	}
	return &Screen{
		out: out, ansi: ansi, Style: Style{On: ansi},
		cols: cols, rows: 24,
		sizeOf: func() (int, int) { return cols, 24 },
	}
}

// NewPlainScreen renders without any terminal control, for tests and for
// non-interactive runs.
func NewPlainScreen(out io.Writer, cols int) *Screen {
	if cols <= 0 {
		cols = 80
	}
	return &Screen{
		out: out, ansi: false, Style: Style{On: false},
		cols: cols, rows: 24,
		sizeOf: func() (int, int) { return cols, 24 },
	}
}

func (s *Screen) ANSI() bool { return s.ansi }

func (s *Screen) Cols() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cols
}

// Refresh re-reads the terminal size. Called before drawing the status region
// so a resized window is picked up without a signal handler.
func (s *Screen) refreshSize() {
	if s.sizeOf == nil {
		return
	}
	if c, r := s.sizeOf(); c > 0 && r > 0 {
		s.cols, s.rows = c, r
	}
}

// Write emits content above the status region.
//
// Output is line-buffered: a partial line is held until its newline arrives,
// because the status region must start at a column-zero boundary. Streaming
// prose therefore appears a line at a time, which for an agent's output is
// indistinguishable from character-at-a-time and avoids an entire class of
// cursor-accounting bugs.
func (s *Screen) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.partial.Write(p)
	text := s.partial.String()
	idx := strings.LastIndexByte(text, '\n')
	if idx < 0 {
		return len(p), nil
	}
	complete := text[:idx+1]
	s.partial.Reset()
	s.partial.WriteString(text[idx+1:])

	s.eraseStatus()
	io.WriteString(s.out, complete)
	s.drawStatus()
	return len(p), nil
}

// Flush emits any held partial line.
func (s *Screen) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.partial.Len() == 0 {
		return
	}
	rest := s.partial.String()
	s.partial.Reset()
	s.eraseStatus()
	io.WriteString(s.out, rest)
	if !strings.HasSuffix(rest, "\n") {
		io.WriteString(s.out, "\n")
	}
	s.drawStatus()
}

// Printf writes a formatted line above the status region.
func (s *Screen) Printf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	s.Write([]byte(line))
}

// SetStatus replaces the pinned region.
func (s *Screen) SetStatus(lines ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eraseStatus()
	s.status = lines
	s.drawStatus()
}

// ClearStatus removes the pinned region entirely.
func (s *Screen) ClearStatus() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eraseStatus()
	s.status = nil
}

// eraseStatus walks back over the drawn status lines and clears to the end of
// the screen. Caller holds the lock.
func (s *Screen) eraseStatus() {
	if !s.ansi || s.statusDrawn == 0 {
		s.statusDrawn = 0
		return
	}
	// \r to column zero, up N lines, then erase everything below.
	fmt.Fprintf(s.out, "\r\x1b[%dA\x1b[0J", s.statusDrawn)
	s.statusDrawn = 0
}

func (s *Screen) drawStatus() {
	if !s.ansi || len(s.status) == 0 {
		return
	}
	s.refreshSize()
	for _, l := range s.status {
		// Truncate to the window: a status line that wraps would occupy two
		// rows, and the erase-by-count above would then leave debris.
		fmt.Fprintf(s.out, "%s\x1b[0K\n", TruncateVisible(l, s.cols))
	}
	s.statusDrawn = len(s.status)
}

// Raw writes directly, bypassing the status bookkeeping. Used by the pager,
// which owns the screen for the duration of its own draw.
func (s *Screen) Raw(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	io.WriteString(s.out, text)
}

// Suspend hides the status region and returns a function that restores it, so
// an interactive prompt can own the bottom of the screen.
func (s *Screen) Suspend() func() {
	s.mu.Lock()
	saved := s.status
	s.eraseStatus()
	s.status = nil
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.status = saved
		s.drawStatus()
	}
}
