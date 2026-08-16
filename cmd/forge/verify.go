package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/VEER-TARGARYEN/forge/internal/agent"
	"github.com/VEER-TARGARYEN/forge/internal/provider"
	"github.com/VEER-TARGARYEN/forge/internal/tools"
	"github.com/VEER-TARGARYEN/forge/internal/verify"
)

// verifier adapts project detection and the runner to the callbacks the agent
// and the verify tool expect.
type verifier struct {
	root    string
	checks  []verify.Check
	opts    verify.Options
	out     io.Writer
	lastRep *verify.Report
}

func newVerifier(root string, override string, timeout time.Duration) *verifier {
	v := &verifier{root: root, opts: verify.Options{Timeout: timeout}, out: os.Stderr}
	if override != "" {
		// An explicit command replaces detection entirely, and runs at the
		// test stage so a failure is always fatal.
		v.checks = []verify.Check{{
			Name: "custom", Command: override, Stage: verify.StageTest, Parser: "generic",
		}}
		return v
	}
	v.checks = verify.Detect(root).Checks
	return v
}

func (v *verifier) enabled() bool { return len(v.checks) > 0 }

func (v *verifier) filtered(only string) []verify.Check {
	if only == "" {
		return v.checks
	}
	var out []verify.Check
	for _, c := range v.checks {
		if c.Stage.String() == only {
			out = append(out, c)
		}
	}
	return out
}

// forAgent is the harness-side callback: run everything, report the summary.
func (v *verifier) forAgent(ctx context.Context) (string, bool, int, error) {
	if !v.enabled() {
		return "", false, 0, fmt.Errorf("no verification commands detected for this project")
	}
	rep := verify.Run(ctx, v.root, v.checks, v.opts)
	v.lastRep = rep
	fmt.Fprintf(v.out, "  ⋯ %s\n", rep.Short())
	return rep.Summary(), rep.Passed, len(rep.Failures()), nil
}

// forTool is the model-side callback, which additionally accepts a stage filter.
func (v *verifier) forTool(ctx context.Context, only string) (string, bool, error) {
	checks := v.filtered(only)
	if len(checks) == 0 {
		if !v.enabled() {
			return "", false, fmt.Errorf("no verification commands detected for this project")
		}
		return "", false, fmt.Errorf("no %s stage is configured for this project", only)
	}
	rep := verify.Run(ctx, v.root, checks, v.opts)
	v.lastRep = rep
	fmt.Fprintf(v.out, "  ⋯ %s\n", rep.Short())
	return rep.Summary(), rep.Passed, nil
}

// subStats returns the delegation accounting callback, or nil when no spawner
// was wired.
func subStats(s *agent.Spawner) func() (provider.Usage, int) {
	if s == nil {
		return nil
	}
	return func() (provider.Usage, int) { return s.Usage(), s.Count() }
}

// verifyHook returns the agent's verification callback, or nil to disable the
// self-repair loop entirely.
func verifyHook(v *verifier, disabled bool) agent.VerifyFunc {
	if disabled || !v.enabled() {
		return nil
	}
	return v.forAgent
}

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	dir := fs.String("dir", ".", "project root")
	only := fs.String("only", "", "run one stage: build, lint, or test")
	cmd := fs.String("cmd", "", "run this command instead of the detected checks")
	timeout := fs.Duration("timeout", 5*time.Minute, "per-check timeout")
	full := fs.Bool("full", false, "print raw output as well as the summary")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ws, err := tools.NewWorkspace(*dir)
	if err != nil {
		return err
	}
	proj := verify.Detect(ws.Root())
	v := newVerifier(ws.Root(), *cmd, *timeout)
	if !v.enabled() {
		return fmt.Errorf("no build or test commands detected in %s\n"+
			"pass -cmd \"your command\" to verify anyway", ws.Root())
	}

	checks := v.filtered(strings.ToLower(*only))
	if len(checks) == 0 {
		return fmt.Errorf("no %q stage among the detected checks", *only)
	}

	fmt.Printf("project: %s\n", proj.Kind)
	for _, c := range checks {
		opt := ""
		if c.Optional {
			opt = "  (optional)"
		}
		fmt.Printf("  %-10s %s%s\n", c.Stage, c.Command, opt)
	}
	fmt.Println()

	rep := verify.Run(context.Background(), ws.Root(), checks, verify.Options{Timeout: *timeout})
	fmt.Print(rep.Summary())

	if *full {
		for _, r := range rep.Results {
			if r.Passed {
				continue
			}
			fmt.Printf("\n--- raw output: %s ---\n%s\n", r.Check.Name, r.Output)
		}
	}
	if !rep.Passed {
		return fmt.Errorf("verification failed")
	}
	return nil
}
