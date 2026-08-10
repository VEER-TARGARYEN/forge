package tools

import (
	"context"
	"encoding/json"
	"strings"
)

// VerifyFunc runs the project's checks and returns a model-facing summary
// plus whether everything passed.
type VerifyFunc func(ctx context.Context, only string) (summary string, passed bool, err error)

// Verify lets the model check its own work mid-run rather than waiting for the
// harness to do it at the end.
//
// Exposing it as a tool matters: a model that can see a build break right
// after making it fixes the actual cause, while one that only learns at the
// end has four more edits layered on top and has to reason backwards.
type Verify struct{}

func (Verify) Spec() Spec {
	return Spec{
		Name: "verify",
		Description: "Run the project's build, lint, and test commands and report what failed. " +
			"Call this after making changes, and before saying you are done. " +
			"Output is summarised to the located failures, not the raw log.",
		Schema: obj(map[string]any{
			"only": str("Limit to one stage: build, lint, or test. Omit to run everything."),
		}),
	}
}

// Mutates is true: running a project's own build and test commands executes
// its code, which is the same risk class as run_command and belongs behind the
// same gate.
func (Verify) Mutates() bool { return true }

func (Verify) Run(ctx context.Context, raw json.RawMessage, env *Env) (*Result, error) {
	var a struct {
		Only string `json:"only"`
	}
	if err := ParseArgs(raw, &a); err != nil {
		return Errorf("%v", err), nil
	}
	if env.Verify == nil {
		return Errorf("no verification is configured for this project; " +
			"run the build or test command with run_command instead"), nil
	}
	a.Only = strings.ToLower(strings.TrimSpace(a.Only))
	switch a.Only {
	case "", "build", "lint", "test":
	default:
		return Errorf("only must be build, lint, or test"), nil
	}

	summary, passed, err := env.Verify(ctx, a.Only)
	if err != nil {
		return Errorf("verification could not run: %v", err), nil
	}
	display := "verify: passed"
	if !passed {
		display = "verify: FAILED"
	}
	body, note := env.Clip("verify", summary)
	return &Result{
		Content: body + note,
		Display: display,
		IsError: !passed,
	}, nil
}
