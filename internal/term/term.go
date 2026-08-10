// Package term is a minimal, dependency-free terminal layer: TTY detection,
// window size, raw mode, and key decoding.
//
// The alternative was golang.org/x/term, which is a fine package and exactly
// the kind of thing this project does not take. What it does is small — a
// couple of console-mode syscalls per platform and an escape-sequence decoder
// — and the decoder is the part with actual logic, so it lives here in
// portable code where it can be tested without a terminal attached.
package term

import (
	"errors"
	"io"
	"os"
	"strings"
)

// Code identifies a non-printable key.
type Code int

const (
	KeyRune Code = iota
	KeyEnter
	KeyEsc
	KeyBackspace
	KeyTab
	KeyUp
	KeyDown
	KeyRight
	KeyLeft
	KeyHome
	KeyEnd
	KeyPgUp
	KeyPgDn
	KeyDelete
	KeyCtrlC
	KeyCtrlD
	KeyUnknown
)

func (c Code) String() string {
	switch c {
	case KeyRune:
		return "rune"
	case KeyEnter:
		return "enter"
	case KeyEsc:
		return "esc"
	case KeyBackspace:
		return "backspace"
	case KeyTab:
		return "tab"
	case KeyUp:
		return "up"
	case KeyDown:
		return "down"
	case KeyRight:
		return "right"
	case KeyLeft:
		return "left"
	case KeyHome:
		return "home"
	case KeyEnd:
		return "end"
	case KeyPgUp:
		return "pgup"
	case KeyPgDn:
		return "pgdn"
	case KeyDelete:
		return "delete"
	case KeyCtrlC:
		return "ctrl-c"
	case KeyCtrlD:
		return "ctrl-d"
	default:
		return "unknown"
	}
}

// Key is one decoded keystroke.
type Key struct {
	Code Code
	Rune rune
}

func (k Key) IsRune(r rune) bool { return k.Code == KeyRune && k.Rune == r }

// ByteReader is the input contract for the decoder: one byte at a time, so a
// test can drive it from a string and a terminal can drive it from a pipe.
type ByteReader interface {
	ReadByte() (byte, error)
}

// DecodeKey reads one keystroke.
//
// With virtual-terminal input enabled, Windows delivers the same escape
// sequences as a Unix terminal, so this decoder is shared. An unrecognised
// escape sequence is consumed whole and reported as KeyUnknown rather than
// leaking its bytes into the application as stray runes — a half-consumed
// sequence would put every subsequent keystroke out of frame.
func DecodeKey(r ByteReader) (Key, error) {
	b, err := r.ReadByte()
	if err != nil {
		return Key{}, err
	}
	switch b {
	case 0x03:
		return Key{Code: KeyCtrlC}, nil
	case 0x04:
		return Key{Code: KeyCtrlD}, nil
	case '\r', '\n':
		return Key{Code: KeyEnter}, nil
	case '\t':
		return Key{Code: KeyTab}, nil
	case 0x7f, 0x08:
		return Key{Code: KeyBackspace}, nil
	case 0x1b:
		return decodeEscape(r)
	}
	if b < 0x20 {
		return Key{Code: KeyUnknown}, nil
	}
	if b < 0x80 {
		return Key{Code: KeyRune, Rune: rune(b)}, nil
	}
	// UTF-8 continuation: gather the rest of the sequence so a multi-byte
	// character arrives as one rune.
	n := utf8Len(b)
	buf := make([]byte, 1, 4)
	buf[0] = b
	for i := 1; i < n; i++ {
		c, err := r.ReadByte()
		if err != nil {
			break
		}
		buf = append(buf, c)
	}
	rs := []rune(string(buf))
	if len(rs) == 0 {
		return Key{Code: KeyUnknown}, nil
	}
	return Key{Code: KeyRune, Rune: rs[0]}, nil
}

func utf8Len(b byte) int {
	switch {
	case b&0xE0 == 0xC0:
		return 2
	case b&0xF0 == 0xE0:
		return 3
	case b&0xF8 == 0xF0:
		return 4
	}
	return 1
}

// decodeEscape handles CSI and SS3 sequences. A lone ESC (no bytes following)
// is reported as KeyEsc.
func decodeEscape(r ByteReader) (Key, error) {
	b, err := r.ReadByte()
	if err != nil {
		// Nothing followed: the user pressed Escape.
		if errors.Is(err, io.EOF) {
			return Key{Code: KeyEsc}, nil
		}
		return Key{Code: KeyEsc}, nil
	}
	switch b {
	case '[':
		// CSI: parameters, then a final byte in @..~
		var params strings.Builder
		for {
			c, err := r.ReadByte()
			if err != nil {
				return Key{Code: KeyUnknown}, nil
			}
			if c >= 0x40 && c <= 0x7e {
				return csiKey(params.String(), c), nil
			}
			params.WriteByte(c)
			if params.Len() > 16 {
				return Key{Code: KeyUnknown}, nil
			}
		}
	case 'O':
		// SS3: used by some terminals for arrows and function keys.
		c, err := r.ReadByte()
		if err != nil {
			return Key{Code: KeyUnknown}, nil
		}
		return csiKey("", c), nil
	}
	return Key{Code: KeyEsc}, nil
}

func csiKey(params string, final byte) Key {
	switch final {
	case 'A':
		return Key{Code: KeyUp}
	case 'B':
		return Key{Code: KeyDown}
	case 'C':
		return Key{Code: KeyRight}
	case 'D':
		return Key{Code: KeyLeft}
	case 'H':
		return Key{Code: KeyHome}
	case 'F':
		return Key{Code: KeyEnd}
	case '~':
		switch params {
		case "1", "7":
			return Key{Code: KeyHome}
		case "3":
			return Key{Code: KeyDelete}
		case "4", "8":
			return Key{Code: KeyEnd}
		case "5":
			return Key{Code: KeyPgUp}
		case "6":
			return Key{Code: KeyPgDn}
		}
	}
	return Key{Code: KeyUnknown}
}

// ---------- capability probing ----------

// State holds whatever a platform needs to restore the terminal.
type State struct {
	inMode  uint32
	outMode uint32
	fdIn    uintptr
	fdOut   uintptr
	unix    []byte
	valid   bool
}

// Size reports the terminal dimensions, falling back to a sane default when it
// cannot be determined — a wrong-but-plausible width degrades gracefully,
// whereas zero would make every layout calculation collapse.
func Size(f *os.File) (cols, rows int) {
	c, r, err := size(f)
	if err != nil || c <= 0 || r <= 0 {
		return 80, 24
	}
	return c, r
}

// IsTTY reports whether f is an interactive terminal.
func IsTTY(f *os.File) bool { return isTTY(f) }

// SupportsANSI reports whether escape sequences will be interpreted.
//
// NO_COLOR is honoured because a user who set it has said what they want, and
// TERM=dumb is checked because emitting escapes there produces literal garbage
// rather than formatting.
func SupportsANSI(f *os.File) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	if !isTTY(f) {
		return false
	}
	return enableANSI(f)
}

// MakeRaw puts the terminal into raw mode and returns a restore function.
// The restore function is safe to call more than once.
func MakeRaw(in, out *os.File) (restore func(), err error) {
	st, err := makeRaw(in, out)
	if err != nil {
		return func() {}, err
	}
	done := false
	return func() {
		if done {
			return
		}
		done = true
		restoreState(st)
	}, nil
}
