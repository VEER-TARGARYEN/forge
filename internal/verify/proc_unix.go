//go:build !windows

package verify

import (
	"os/exec"
	"syscall"
)

// setupProcessGroup puts the check in its own process group.
//
// Checks run through `/bin/sh -c`, so the thing that actually hangs is a
// grandchild. Killing the shell alone leaves it running, and — worse — it
// still holds the write end of the pipe feeding our output buffer, so
// cmd.Wait blocks until it finishes anyway. A timeout that waits for the
// process it just timed out on is not a timeout.
//
// Putting the shell in a new group means one kill reaches everything it
// started.
func setupProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup signals the whole group. The negative pid is what makes
// kill(2) address the group rather than the single process.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		// The group may already be gone, or setpgid may not have taken; fall
		// back to the single process rather than leaving it running.
		return cmd.Process.Kill()
	}
	return nil
}
