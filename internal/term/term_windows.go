//go:build windows

package term

import (
	"os"
	"syscall"
	"unsafe"
)

// Windows console modes. These are the raw constants from wincon.h rather than
// a dependency on x/sys/windows.
const (
	enableProcessedInput         = 0x0001
	enableLineInput              = 0x0002
	enableEchoInput              = 0x0004
	enableWindowInput            = 0x0008
	enableVirtualTerminalInput   = 0x0200
	enableProcessedOutput        = 0x0001
	enableVirtualTerminalProcess = 0x0004
	disableNewlineAutoReturnFlag = 0x0008
)

var (
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode             = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode             = kernel32.NewProc("SetConsoleMode")
	procGetConsoleScreenBufferInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")
)

type coord struct{ X, Y int16 }
type smallRect struct{ Left, Top, Right, Bottom int16 }

type consoleScreenBufferInfo struct {
	Size              coord
	CursorPosition    coord
	Attributes        uint16
	Window            smallRect
	MaximumWindowSize coord
}

func getMode(fd uintptr) (uint32, bool) {
	var mode uint32
	r, _, _ := procGetConsoleMode.Call(fd, uintptr(unsafe.Pointer(&mode)))
	return mode, r != 0
}

func setMode(fd uintptr, mode uint32) bool {
	r, _, _ := procSetConsoleMode.Call(fd, uintptr(mode))
	return r != 0
}

// isTTY uses GetConsoleMode as the probe: it succeeds only on a real console
// handle, and fails for pipes and redirected files.
func isTTY(f *os.File) bool {
	if f == nil {
		return false
	}
	_, ok := getMode(f.Fd())
	return ok
}

func size(f *os.File) (int, int, error) {
	if f == nil {
		return 0, 0, os.ErrInvalid
	}
	var info consoleScreenBufferInfo
	r, _, err := procGetConsoleScreenBufferInfo.Call(f.Fd(), uintptr(unsafe.Pointer(&info)))
	if r == 0 {
		return 0, 0, err
	}
	// The window rect, not the buffer size: the buffer is usually far taller
	// than the visible window, and drawing to buffer height would scroll.
	cols := int(info.Window.Right-info.Window.Left) + 1
	rows := int(info.Window.Bottom-info.Window.Top) + 1
	return cols, rows, nil
}

// enableANSI turns on virtual terminal processing, which every Windows 10+
// console supports but does not enable by default for a non-shell process.
func enableANSI(f *os.File) bool {
	mode, ok := getMode(f.Fd())
	if !ok {
		return false
	}
	if mode&enableVirtualTerminalProcess != 0 {
		return true
	}
	return setMode(f.Fd(), mode|enableVirtualTerminalProcess|enableProcessedOutput)
}

func makeRaw(in, out *os.File) (State, error) {
	st := State{fdIn: in.Fd(), fdOut: out.Fd()}

	inMode, ok := getMode(st.fdIn)
	if !ok {
		return st, os.ErrInvalid
	}
	st.inMode = inMode

	// Clear line buffering and echo so keystrokes arrive immediately; add
	// virtual-terminal input so arrows and page keys arrive as the same escape
	// sequences a Unix terminal sends, which is what lets one decoder serve
	// both platforms.
	raw := inMode &^ (enableLineInput | enableEchoInput | enableProcessedInput | enableWindowInput)
	raw |= enableVirtualTerminalInput
	if !setMode(st.fdIn, raw) {
		return st, os.ErrPermission
	}

	if outMode, ok := getMode(st.fdOut); ok {
		st.outMode = outMode
		setMode(st.fdOut, outMode|enableVirtualTerminalProcess|enableProcessedOutput)
	}
	st.valid = true
	return st, nil
}

func restoreState(st State) {
	if !st.valid {
		return
	}
	setMode(st.fdIn, st.inMode)
	if st.outMode != 0 {
		setMode(st.fdOut, st.outMode)
	}
}
