package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// RunCommand executes a shell command inside the workspace.
//
// This is the tool with real blast radius, so it carries two defences the
// others do not: a hard timeout with process termination, and a destructive-
// pattern classifier that marks a command Risky so the approver can demand a
// stronger confirmation than a keystroke.
type RunCommand struct{}

func (RunCommand) Spec() Spec {
	return Spec{
		Name: "run_command",
		Description: "Run a shell command in the workspace and return its combined output. " +
			"Use it to build, run tests, or inspect the repository. " +
			"Do not use it to edit files — use SEARCH/REPLACE blocks or edit_file.",
		Schema: obj(map[string]any{
			"command":     str("The command line to run."),
			"timeout_sec": integer("Kill the command after this many seconds. Defaults to 120, max 900."),
			"cwd":         str("Working directory relative to the workspace root. Defaults to the root."),
		}, "command"),
	}
}

func (RunCommand) Mutates() bool { return true }

// destructive matches commands that are hard or impossible to undo. This is a
// heuristic, not a sandbox: it exists so a human sees a stronger prompt before
// a recursive delete or a force-push, not to make an untrusted model safe.
var destructive = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\brm\s+(-[a-z]*\s+)*-[a-z]*[rf]`),
	regexp.MustCompile(`(?i)\bremove-item\b.*-(recurse|force)`),
	regexp.MustCompile(`(?i)\bdel\s+/[sqf]`),
	regexp.MustCompile(`(?i)\brmdir\s+/s`),
	regexp.MustCompile(`(?i)\bgit\s+(push\s+.*--force|reset\s+--hard|clean\s+-[a-z]*[fd])`),
	regexp.MustCompile(`(?i)\bgit\s+checkout\s+--\s`),
	regexp.MustCompile(`(?i)\b(mkfs|fdisk|diskpart|format)\b`),
	regexp.MustCompile(`(?i)>\s*/dev/(sd|nvme|hd)`),
	regexp.MustCompile(`(?i)\b(shutdown|reboot|halt)\b`),
	regexp.MustCompile(`(?i)\bchmod\s+-R\s+777\b`),
	regexp.MustCompile(`(?i)\b(curl|wget|iwr|invoke-webrequest)\b.*\|\s*(sh|bash|pwsh|powershell)`),
	regexp.MustCompile(`(?i)\bnpm\s+publish\b|\bcargo\s+publish\b|\btwine\s+upload\b`),
	regexp.MustCompile(`(?i)\bdd\s+if=`),
}

// IsDestructive reports whether a command matches a known-dangerous shape.
func IsDestructive(cmd string) bool {
	for _, re := range destructive {
		if re.MatchString(cmd) {
			return true
		}
	}
	return false
}

func (RunCommand) Run(ctx context.Context, raw json.RawMessage, env *Env) (*Result, error) {
	var a struct {
		Command    string `json:"command"`
		TimeoutSec int    `json:"timeout_sec"`
		Cwd        string `json:"cwd"`
	}
	if err := ParseArgs(raw, &a); err != nil {
		return Errorf("%v", err), nil
	}
	a.Command = strings.TrimSpace(a.Command)
	if a.Command == "" {
		return Errorf("command is required"), nil
	}
	if a.TimeoutSec <= 0 {
		a.TimeoutSec = 120
	}
	if a.TimeoutSec > 900 {
		a.TimeoutSec = 900
	}
	dir := env.WS.Root()
	if a.Cwd != "" {
		resolved, err := env.WS.Resolve(a.Cwd)
		if err != nil {
			return Errorf("%v", err), nil
		}
		dir = resolved
	}

	risky := IsDestructive(a.Command)
	summary := fmt.Sprintf("run: %s", a.Command)
	if risky {
		summary = fmt.Sprintf("run (DESTRUCTIVE): %s", a.Command)
	}
	detail := fmt.Sprintf("%s\n\ncwd:     %s\ntimeout: %ds", a.Command, env.WS.Rel(dir), a.TimeoutSec)
	if err := env.Approver.Approve(ApprovalRequest{
		Tool: "run_command", Kind: "command", Summary: summary, Detail: detail, Risky: risky,
	}); err != nil {
		return Errorf("declined: %v", err), nil
	}

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(a.TimeoutSec)*time.Second)
	defer cancel()

	name, args := shellFor(a.Command)
	cmd := exec.CommandContext(runCtx, name, args...)
	cmd.Dir = dir
	// A tool that waits on stdin would hang until the timeout; give it EOF.
	cmd.Stdin = strings.NewReader("")

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	start := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(start)

	out := buf.String()
	body, note := env.Clip("run_command "+a.Command, out)

	var status string
	switch {
	case runCtx.Err() == context.DeadlineExceeded:
		status = fmt.Sprintf("TIMED OUT after %ds (process killed)", a.TimeoutSec)
	case runErr == nil:
		status = fmt.Sprintf("exit 0 in %s", elapsed.Round(time.Millisecond))
	default:
		code := -1
		var ee *exec.ExitError
		if ok := asExitError(runErr, &ee); ok {
			code = ee.ExitCode()
		}
		status = fmt.Sprintf("exit %d in %s", code, elapsed.Round(time.Millisecond))
	}

	if strings.TrimSpace(body) == "" {
		body = "(no output)"
	}
	return &Result{
		Content: fmt.Sprintf("$ %s\n%s\n\n%s%s", a.Command, status, body, note),
		Display: fmt.Sprintf("%s  [%s]", a.Command, status),
		IsError: runErr != nil,
	}, nil
}

// shellFor picks the host shell. PowerShell on Windows matches what the user
// types by hand there; /bin/sh everywhere else.
func shellFor(command string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "powershell", []string{"-NoProfile", "-NonInteractive", "-Command", command}
	}
	return "/bin/sh", []string{"-c", command}
}

func asExitError(err error, out **exec.ExitError) bool {
	for err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			*out = ee
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
