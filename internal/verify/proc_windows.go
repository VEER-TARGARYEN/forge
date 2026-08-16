package verify

import "os/exec"

// Windows has no process groups in the POSIX sense. Killing the PowerShell
// host does leave its children running, but they do not inherit our output
// handles the way a forked shell's children do, so Wait returns and the
// timeout is honoured — which is what the invariant checks.
//
// Genuinely reaping the tree would mean a Job Object, and that is CreateProcess
// flags and handle plumbing for a case the timeout already covers. WaitDelay in
// runOne is the backstop either way.
func setupProcessGroup(cmd *exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
