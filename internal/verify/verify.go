package verify

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

type CheckResult struct {
	Check    Check
	ExitCode int
	Passed   bool
	TimedOut bool
	Output   string
	Failures []Failure
	Duration time.Duration
}

type Report struct {
	Kind     string
	Results  []CheckResult
	Passed   bool
	Duration time.Duration
	// Skipped names checks that never ran because an earlier stage failed.
	Skipped []string
}

// Failures flattens every located problem across the checks that ran.
func (r *Report) Failures() []Failure {
	var out []Failure
	for _, c := range r.Results {
		if !c.Passed {
			out = append(out, c.Failures...)
		}
	}
	return out
}

// FailedCheck returns the first check that actually failed the run, skipping
// optional ones.
func (r *Report) FailedCheck() *CheckResult {
	for i := range r.Results {
		if !r.Results[i].Passed && !r.Results[i].Check.Optional {
			return &r.Results[i]
		}
	}
	return nil
}

type Options struct {
	Timeout time.Duration
	// MaxOutput bounds the raw text retained per check.
	MaxOutput int
}

// Run executes checks in stage order, stopping at the first failing required
// stage.
//
// Stopping early is not just a speed optimisation: running tests against a
// tree that does not compile produces failures that describe the build error
// in a much less useful way, and a model shown both will often chase the wrong
// one.
func Run(ctx context.Context, root string, checks []Check, opts Options) *Report {
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Minute
	}
	if opts.MaxOutput <= 0 {
		opts.MaxOutput = 200000
	}
	start := time.Now()
	rep := &Report{Passed: true}

	stopped := false
	for _, c := range checks {
		if stopped {
			rep.Skipped = append(rep.Skipped, c.Name)
			continue
		}
		res := runOne(ctx, root, c, opts)
		rep.Results = append(rep.Results, res)
		if !res.Passed && !c.Optional {
			rep.Passed = false
			stopped = true
		}
	}
	rep.Duration = time.Since(start)
	return rep
}

func runOne(ctx context.Context, root string, c Check, opts Options) CheckResult {
	runCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	name, args := shellFor(c.Command)
	cmd := exec.CommandContext(runCtx, name, args...)
	cmd.Dir = root
	cmd.Stdin = strings.NewReader("")

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	// Two separate guards, because a hung check has two ways to outlive its
	// deadline.
	//
	// Cancel kills the whole process group rather than just the shell we
	// started, so the command the shell forked actually dies.
	//
	// WaitDelay bounds Wait itself. Writing to a bytes.Buffer means Go copies
	// through an os.Pipe, and Wait will not return while any process still
	// holds the write end — so a grandchild that escaped the kill would
	// otherwise block us for as long as it runs. Without this the timeout
	// silently waits out the very process it timed out on: 135/136 on Linux,
	// and green on Windows, which is exactly how it stayed hidden.
	setupProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 2 * time.Second

	start := time.Now()
	err := cmd.Run()
	res := CheckResult{Check: c, Duration: time.Since(start), Output: buf.String()}

	if len(res.Output) > opts.MaxOutput {
		res.Output = res.Output[:opts.MaxOutput] + "\n[output truncated]"
	}
	switch {
	case runCtx.Err() == context.DeadlineExceeded:
		res.TimedOut = true
		res.ExitCode = -1
	case err == nil:
		res.Passed = true
	default:
		res.ExitCode = -1
		var ee *exec.ExitError
		if errorsAs(err, &ee) {
			res.ExitCode = ee.ExitCode()
		}
	}
	if !res.Passed {
		parser := c.Parser
		if parser == "" {
			parser = "generic"
		}
		res.Failures = Parse(parser, res.Output)
	}
	return res
}

// Summary renders a report for a model: the failing command, the located
// problems, and nothing else.
//
// This is the token argument for the whole package. A failing `go test ./...`
// on a real repository emits hundreds of lines; what the model needs is the
// three that say where the bug is.
func (r *Report) Summary() string {
	var b strings.Builder
	if r.Passed {
		fmt.Fprintf(&b, "VERIFICATION PASSED (%s)\n", r.Duration.Round(time.Millisecond))
		for _, c := range r.Results {
			mark := "ok"
			if !c.Passed {
				mark = "warn"
			}
			fmt.Fprintf(&b, "  %-5s %-10s %s\n", mark, c.Check.Name, c.Check.Command)
		}
		return b.String()
	}

	failed := r.FailedCheck()
	if failed == nil {
		return "VERIFICATION PASSED\n"
	}
	fmt.Fprintf(&b, "VERIFICATION FAILED at the %s stage\n", failed.Check.Stage)
	fmt.Fprintf(&b, "$ %s\n", failed.Check.Command)
	if failed.TimedOut {
		fmt.Fprintf(&b, "timed out\n")
	} else {
		fmt.Fprintf(&b, "exit %d\n", failed.ExitCode)
	}

	if len(failed.Failures) > 0 {
		fmt.Fprintf(&b, "\n%d problem(s):\n", len(failed.Failures))
		for _, f := range failed.Failures {
			fmt.Fprintf(&b, "  %s\n", f.String())
		}
	} else {
		// Nothing parsed out; the tail of the output is the best available
		// signal, and it is where most toolchains put the summary.
		fmt.Fprintf(&b, "\ncould not locate specific failures; last lines of output:\n")
		fmt.Fprintf(&b, "%s\n", tail(failed.Output, 25))
	}
	if len(r.Skipped) > 0 {
		fmt.Fprintf(&b, "\nnot run: %s\n", strings.Join(r.Skipped, ", "))
	}
	return b.String()
}

// Short renders a one-line status, for the human-facing transcript.
func (r *Report) Short() string {
	if r.Passed {
		n := 0
		for _, c := range r.Results {
			if c.Passed {
				n++
			}
		}
		return fmt.Sprintf("passed %d check(s) in %s", n, r.Duration.Round(time.Millisecond))
	}
	failed := r.FailedCheck()
	if failed == nil {
		return "passed"
	}
	return fmt.Sprintf("%s failed (%d problem(s))", failed.Check.Name, len(failed.Failures))
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func shellFor(command string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "powershell", []string{"-NoProfile", "-NonInteractive", "-Command", command}
	}
	return "/bin/sh", []string{"-c", command}
}

func errorsAs(err error, target **exec.ExitError) bool {
	for err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			*target = ee
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
