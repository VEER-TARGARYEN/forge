// Package approval gates every state-changing tool call behind a policy and,
// where the policy requires it, a human keystroke.
//
// The workspace root bounds *where* the agent can act; this bounds *what* it
// may do without being asked. The two are deliberately separate: a permissive
// approval mode never widens the path sandbox.
package approval

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/VEER-TARGARYEN/forge/internal/tools"
)

type Mode string

const (
	// ReadOnly denies every mutation. Useful for "explain this codebase".
	ReadOnly Mode = "readonly"
	// Ask prompts for every mutation except allowlisted inspection commands.
	Ask Mode = "ask"
	// AutoEdit approves file writes and edits inside the workspace without
	// asking, but still prompts for shell commands. This is the practical
	// default once you trust the loop: edits are visible in git, commands
	// are not.
	AutoEdit Mode = "auto-edit"
	// Yolo approves everything except commands flagged destructive, which
	// always prompt. An unattended agent should still not be able to
	// force-push or recursively delete without a human in the loop.
	Yolo Mode = "yolo"
)

func ParseMode(s string) (Mode, error) {
	switch Mode(strings.ToLower(strings.TrimSpace(s))) {
	case ReadOnly:
		return ReadOnly, nil
	case Ask, "":
		return Ask, nil
	case AutoEdit:
		return AutoEdit, nil
	case Yolo:
		return Yolo, nil
	}
	return "", fmt.Errorf("unknown approval mode %q (use readonly, ask, auto-edit, or yolo)", s)
}

// DefaultAllowPrefixes are inspection commands safe to run unprompted. They
// read state without changing it. Build and test commands are deliberately
// absent — they execute project code, which is a different risk class — but
// they are the obvious things to add to your own allowlist once you trust a
// repository.
var DefaultAllowPrefixes = []string{
	"git status", "git diff", "git log", "git show", "git branch", "git remote -v",
	"git rev-parse", "git ls-files",
	"ls", "dir", "pwd", "cat", "head", "tail", "wc", "file", "stat",
	"go list", "go env", "go version", "go vet", "gofmt -l",
	"node --version", "npm --version", "python --version", "python3 --version",
	"cargo --version", "rustc --version", "tsc --noEmit",
	"echo", "which", "where",
}

type Denied struct{ Reason string }

func (d *Denied) Error() string { return d.Reason }

// Aborted signals the user asked to stop the whole run, not just this call.
type Aborted struct{}

func (*Aborted) Error() string { return "aborted by user" }

// Console is the plain-text approver, used when there is no usable terminal
// or when the rich UI is turned off.
type Console struct {
	*Policy
	In  io.Reader
	Out io.Writer
	// Interactive is false when stdin is not a terminal. A non-interactive
	// run cannot prompt, so anything that would prompt is denied rather than
	// silently allowed.
	Interactive bool

	reader *bufio.Reader
}

func NewConsole(mode Mode, extraAllow []string) *Console {
	interactive := false
	if fi, err := os.Stdin.Stat(); err == nil {
		interactive = fi.Mode()&os.ModeCharDevice != 0
	}
	return &Console{
		Policy:      NewPolicy(mode, extraAllow),
		In:          os.Stdin,
		Out:         os.Stderr,
		Interactive: interactive,
	}
}

func (c *Console) Approve(req tools.ApprovalRequest) error {
	if c.Aborted() {
		return &Aborted{}
	}
	switch v := c.Decide(req, c.Interactive); v.Decision {
	case Allow:
		c.note(req, v.Reason)
		return nil
	case Deny:
		return &Denied{v.Reason}
	}
	return c.prompt(req)
}

func (c *Console) note(req tools.ApprovalRequest, why string) {
	fmt.Fprintf(c.Out, "  · %s  [%s]\n", req.Summary, why)
}

func (c *Console) prompt(req tools.ApprovalRequest) error {
	if c.reader == nil {
		c.reader = bufio.NewReader(c.In)
	}
	fmt.Fprintf(c.Out, "\n")
	if req.Risky {
		fmt.Fprintf(c.Out, "  !! DESTRUCTIVE  %s\n", req.Summary)
	} else {
		fmt.Fprintf(c.Out, "  ?  %s\n", req.Summary)
	}
	if req.Detail != "" {
		fmt.Fprintf(c.Out, "%s\n", indent(req.Detail, "     "))
	}

	if req.Risky {
		// A single keystroke is too cheap for an irreversible action.
		fmt.Fprintf(c.Out, "     type 'yes' to run, anything else to skip: ")
		line, err := c.reader.ReadString('\n')
		if err != nil {
			return &Denied{"no input available"}
		}
		if strings.TrimSpace(strings.ToLower(line)) == "yes" {
			return nil
		}
		return &Denied{"user declined a destructive operation"}
	}

	for {
		fmt.Fprintf(c.Out, "     [y] run  [n] skip  [a] always allow %s  [q] abort run: ", req.Tool)
		line, err := c.reader.ReadString('\n')
		if err != nil {
			return &Denied{"no input available"}
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes", "":
			return nil
		case "n", "no":
			return &Denied{"user skipped this call"}
		case "a", "always":
			c.RememberAlways(req.Tool)
			return nil
		case "q", "quit", "abort":
			c.Abort()
			return &Aborted{}
		}
	}
}

func indent(s, pad string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = pad + l
	}
	return strings.Join(lines, "\n")
}

// ---------- non-interactive approvers ----------

// Static is an approver with a fixed answer, for tests and headless runs.
type Static struct {
	Allow  bool
	Reason string
	// Calls records what was asked, so a test can assert the gate was
	// actually consulted rather than bypassed.
	Calls []tools.ApprovalRequest
}

func (s *Static) Approve(req tools.ApprovalRequest) error {
	s.Calls = append(s.Calls, req)
	if s.Allow {
		return nil
	}
	reason := s.Reason
	if reason == "" {
		reason = "denied by policy"
	}
	return &Denied{reason}
}
