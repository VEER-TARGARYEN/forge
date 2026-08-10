//go:build !windows && !linux && !darwin

package term

import "os"

// Everywhere else, forge still builds and runs — it just never claims a
// terminal, so the plain renderer is used and nothing tries to set raw mode.
// A build failure on an unusual GOOS would be a worse outcome than a
// non-interactive UI.

func isTTY(f *os.File) bool { return false }

func size(f *os.File) (int, int, error) { return 0, 0, os.ErrInvalid }

func enableANSI(f *os.File) bool { return false }

func makeRaw(in, out *os.File) (State, error) { return State{}, os.ErrInvalid }

func restoreState(st State) {}
