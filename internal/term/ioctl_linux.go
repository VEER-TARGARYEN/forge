//go:build linux

package term

import "syscall"

// Linux spells the termios ioctls TCGETS/TCSETS.
const (
	getTermios = syscall.TCGETS
	setTermios = syscall.TCSETS
)
