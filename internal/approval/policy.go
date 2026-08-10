package approval

import (
	"fmt"
	"strings"
	"sync"

	"github.com/VEER-TARGARYEN/forge/internal/tools"
)

// Decision is what policy says about a request, before any human is involved.
type Decision int

const (
	// Allow: permitted with no prompt.
	Allow Decision = iota
	// Prompt: policy defers to the human.
	Prompt
	// Deny: policy refuses outright.
	Deny
)

// Verdict carries the decision and the reason, which is shown either way —
// "auto (allowlisted)" is as much a thing the user should see as a refusal.
type Verdict struct {
	Decision Decision
	Reason   string
}

// Policy is the mode-driven half of approval, separated from prompting so the
// plain console and the terminal UI apply identical rules.
//
// Keeping this in one place is a correctness property, not tidiness: two
// implementations of "may this run" would eventually disagree, and the failure
// mode is a mutation happening that the user believed was gated.
type Policy struct {
	mu            sync.Mutex
	Mode          Mode
	AllowPrefixes []string
	alwaysTool    map[string]bool
	aborted       bool
}

func NewPolicy(mode Mode, extraAllow []string) *Policy {
	return &Policy{
		Mode:          mode,
		AllowPrefixes: append(append([]string(nil), DefaultAllowPrefixes...), extraAllow...),
		alwaysTool:    map[string]bool{},
	}
}

func (p *Policy) Abort() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.aborted = true
}

func (p *Policy) Aborted() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.aborted
}

// RememberAlways records an "always allow this tool" answer for the session.
func (p *Policy) RememberAlways(tool string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.alwaysTool[tool] = true
}

// Decide applies policy. Interactive reports whether a human could be asked;
// when false, anything that would prompt is denied rather than allowed.
func (p *Policy) Decide(req tools.ApprovalRequest, interactive bool) Verdict {
	p.mu.Lock()
	mode := p.Mode
	always := p.alwaysTool[req.Tool]
	prefixes := p.AllowPrefixes
	p.mu.Unlock()

	switch mode {
	case ReadOnly:
		return Verdict{Deny, fmt.Sprintf("approval mode is readonly; %s is not permitted", req.Tool)}

	case Yolo:
		if !req.Risky {
			return Verdict{Allow, "auto (yolo)"}
		}
		// Destructive operations prompt even here, by design.

	case AutoEdit:
		if req.Kind == "write" || req.Kind == "edit" {
			return Verdict{Allow, "auto (auto-edit)"}
		}
		if req.Kind == "command" && !req.Risky && allowlisted(prefixes, req.Detail) {
			return Verdict{Allow, "auto (allowlisted)"}
		}

	case Ask:
		if req.Kind == "command" && !req.Risky && allowlisted(prefixes, req.Detail) {
			return Verdict{Allow, "auto (allowlisted)"}
		}
	}

	if always && !req.Risky {
		return Verdict{Allow, "auto (always allowed this session)"}
	}
	if !interactive {
		return Verdict{Deny, fmt.Sprintf(
			"%s needs approval but stdin is not a terminal; re-run with -approval auto-edit or yolo", req.Tool)}
	}
	return Verdict{Prompt, ""}
}

// allowlisted matches a command against safe prefixes.
func allowlisted(prefixes []string, detail string) bool {
	// req.Detail for a command starts with the command line itself.
	cmd := strings.TrimSpace(detail)
	if i := strings.IndexByte(cmd, '\n'); i >= 0 {
		cmd = cmd[:i]
	}
	cmd = strings.ToLower(strings.TrimSpace(cmd))
	// Anything chaining or redirecting is no longer the allowlisted command.
	if strings.ContainsAny(cmd, "|&;><`$") {
		return false
	}
	for _, p := range prefixes {
		p = strings.ToLower(p)
		if cmd == p || strings.HasPrefix(cmd, p+" ") {
			return true
		}
	}
	return false
}
