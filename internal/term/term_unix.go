//go:build linux || darwin

package term

import (
	"os"
	"syscall"
	"unsafe"
)

type winsize struct {
	Row, Col, Xpixel, Ypixel uint16
}

func ioctl(fd, req, arg uintptr) error {
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg)
	if e != 0 {
		return e
	}
	return nil
}

func isTTY(f *os.File) bool {
	if f == nil {
		return false
	}
	var t syscall.Termios
	return ioctl(f.Fd(), getTermios, uintptr(unsafe.Pointer(&t))) == nil
}

func size(f *os.File) (int, int, error) {
	if f == nil {
		return 0, 0, os.ErrInvalid
	}
	var ws winsize
	if err := ioctl(f.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws))); err != nil {
		return 0, 0, err
	}
	return int(ws.Col), int(ws.Row), nil
}

// enableANSI is a no-op: a Unix terminal that reports itself as a TTY and is
// not TERM=dumb already interprets escape sequences.
func enableANSI(f *os.File) bool { return true }

func makeRaw(in, out *os.File) (State, error) {
	st := State{fdIn: in.Fd(), fdOut: out.Fd()}

	var t syscall.Termios
	if err := ioctl(st.fdIn, getTermios, uintptr(unsafe.Pointer(&t))); err != nil {
		return st, err
	}
	st.unix = make([]byte, unsafe.Sizeof(t))
	copy(st.unix, (*[1 << 10]byte)(unsafe.Pointer(&t))[:unsafe.Sizeof(t)])

	raw := t
	// cfmakeraw, spelled out: no canonical mode, no echo, no signal
	// generation, no input translation.
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0

	if err := ioctl(st.fdIn, setTermios, uintptr(unsafe.Pointer(&raw))); err != nil {
		return st, err
	}
	st.valid = true
	return st, nil
}

func restoreState(st State) {
	if !st.valid || len(st.unix) == 0 {
		return
	}
	var t syscall.Termios
	copy((*[1 << 10]byte)(unsafe.Pointer(&t))[:unsafe.Sizeof(t)], st.unix)
	_ = ioctl(st.fdIn, setTermios, uintptr(unsafe.Pointer(&t)))
}
