//go:build darwin

package term

import "syscall"

// Darwin spells the termios ioctls TIOCGETA/TIOCSETA.
const (
	getTermios = syscall.TIOCGETA
	setTermios = syscall.TIOCSETA
)
