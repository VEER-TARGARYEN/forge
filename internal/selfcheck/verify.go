package selfcheck

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/VEER-TARGARYEN/forge/internal/agent"
	"github.com/VEER-TARGARYEN/forge/internal/checkpoint"
	"github.com/VEER-TARGARYEN/forge/internal/tools"
	"github.com/VEER-TARGARYEN/forge/internal/verify"
)

// verifyCases cover Phase 5: project detection, failure parsing, staged
// execution, the undo journal, and the self-repair loop.
func verifyCases() []namedCheck {
	return []namedCheck{
		{"detect: go, rust, python, make projects", checkDetectKinds},
		{"detect: node checks come from package.json scripts", checkDetectNode},
		{"detect: unknown project claims no checks", checkDetectUnknown},
		{"parse: go build diagnostics", checkParseGo},
		{"parse: go test failures carry their test name", checkParseGoTest},
		{"parse: tsc, rust, pytest, jest", checkParseOthers},
		{"parse: dedupes and caps the failure list", checkParseDedupe},
		{"verify: stops at the first failing required stage", checkVerifyStops},
		{"verify: an optional check does not fail the run", checkVerifyOptional},
		{"verify: a hung command is killed at the timeout", checkVerifyTimeout},
		{"verify: summary is far smaller than the raw output", checkVerifySummarySize},
		{"journal: keeps the first original, not the latest", checkJournalFirstWins},
		{"journal: revert restores edits and deletes new files", checkJournalRevert},
		{"tools: a declined edit records no snapshot", checkSnapshotOnlyOnWrite},
		{"agent: passing verification ends the run", checkAgentVerifyPass},
		{"agent: failing verification is handed back for repair", checkAgentRepair},
		{"agent: repair attempts are bounded", checkAgentRepairBounded},
		{"agent: no file changes means no verification run", checkAgentNoChangesNoVerify},
	}
}

// ---------- detection ----------

func checkDetectKinds() (string, error) {
	cases := []struct {
		files map[string]string
		kind  string
		want  string // a command that must appear
	}{
		{map[string]string{"go.mod": "module x\n"}, "go", "go build ./..."},
		{map[string]string{"Cargo.toml": "[package]\nname=\"x\"\n"}, "rust", "cargo build"},
		{map[string]string{"pyproject.toml": "[project]\nname=\"x\"\n"}, "python", "pytest"},
		{map[string]string{"Makefile": "build:\n\techo hi\ntest:\n\techo t\n"}, "make", "make build"},
	}
	for _, c := range cases {
		ws, cleanup, err := tempWS(c.files)
		if err != nil {
			cleanup()
			return "", failf("setup: %v", err)
		}
		p := verify.Detect(ws.Root())
		cleanup()
		if p.Kind != c.kind {
			return "", failf("kind = %q, want %q", p.Kind, c.kind)
		}
		joined := ""
		for _, ch := range p.Checks {
			joined += ch.Command + " | "
		}
		if !strings.Contains(joined, c.want) {
			return "", failf("%s checks = %q, want one containing %q", c.kind, joined, c.want)
		}
	}
	return fmt.Sprintf("%d project kinds", len(cases)), nil
}

func checkDetectNode() (string, error) {
	ws, cleanup, err := tempWS(map[string]string{
		"package.json": `{"scripts":{"build":"webpack","test":"jest"}}`,
	})
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}
	p := verify.Detect(ws.Root())
	if p.Kind != "node" {
		return "", failf("kind = %q", p.Kind)
	}
	var names []string
	for _, c := range p.Checks {
		names = append(names, c.Name)
	}
	if !containsStr(names, "build") || !containsStr(names, "test") {
		return "", failf("checks = %v, want build and test", names)
	}
	// A script that is not declared must not be invented.
	if containsStr(names, "lint") {
		return "", failf("invented a lint script that package.json does not declare")
	}
	return strings.Join(names, "+"), nil
}

func checkDetectUnknown() (string, error) {
	ws, cleanup, err := tempWS(map[string]string{"notes.txt": "nothing here\n"})
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}
	p := verify.Detect(ws.Root())
	if p.Has() {
		return "", failf("claimed %d checks for a project with no markers", len(p.Checks))
	}
	if p.Kind != "unknown" {
		return "", failf("kind = %q, want unknown", p.Kind)
	}
	return "no checks claimed", nil
}

// ---------- parsing ----------

func checkParseGo() (string, error) {
	out := `# vtest
./main.go:11:2: undefined: undefinedHelper
./internal/x/y.go:42:16: cannot use n (variable of type int) as string value
./internal/x/y.go:50: missing return`
	fs := verify.Parse("go", out)
	if len(fs) != 3 {
		return "", failf("parsed %d failures, want 3: %+v", len(fs), fs)
	}
	if fs[0].File != "./main.go" || fs[0].Line != 11 || fs[0].Col != 2 {
		return "", failf("failure 0 = %+v", fs[0])
	}
	if !strings.Contains(fs[0].Message, "undefined: undefinedHelper") {
		return "", failf("message = %q", fs[0].Message)
	}
	// The column-less form must still parse.
	if fs[2].Line != 50 || fs[2].Col != 0 {
		return "", failf("column-less diagnostic = %+v", fs[2])
	}
	return "3 diagnostics, columns optional", nil
}

func checkParseGoTest() (string, error) {
	out := `--- FAIL: TestAdd (0.00s)
    main_test.go:7: Add(2,3) = -1, want 5
--- FAIL: TestSubtract (0.00s)
    main_test.go:15: Subtract(5,3) = 8, want 2
--- FAIL: TestPanics (0.00s)
FAIL
FAIL	vtest	0.123s`
	fs := verify.Parse("gotest", out)
	if len(fs) < 3 {
		return "", failf("parsed %d failures, want at least 3: %+v", len(fs), fs)
	}
	// Attribution is the point: a bare "main_test.go:7: want 5" does not say
	// which case produced it.
	if fs[0].Test != "TestAdd" || fs[0].Line != 7 {
		return "", failf("failure 0 = %+v, want TestAdd at line 7", fs[0])
	}
	if fs[1].Test != "TestSubtract" || fs[1].Line != 15 {
		return "", failf("failure 1 = %+v, want TestSubtract at line 15", fs[1])
	}
	// A test that failed with no located assertion must still be reported.
	found := false
	for _, f := range fs {
		if f.Test == "TestPanics" {
			found = true
		}
	}
	if !found {
		return "", failf("TestPanics failed with no assertion line and was dropped")
	}
	return "3 tests, assertions attributed", nil
}

func checkParseOthers() (string, error) {
	tsc := verify.Parse("tsc", "src/app.ts(12,5): error TS2345: Argument of type 'string' is not assignable.")
	if len(tsc) != 1 || tsc[0].File != "src/app.ts" || tsc[0].Line != 12 || tsc[0].Col != 5 {
		return "", failf("tsc = %+v", tsc)
	}

	rust := verify.Parse("rust", "error[E0308]: mismatched types\n  --> src/main.rs:10:5\n   |\n")
	if len(rust) != 1 || rust[0].File != "src/main.rs" || rust[0].Line != 10 {
		return "", failf("rust = %+v", rust)
	}
	if !strings.Contains(rust[0].Message, "mismatched types") {
		return "", failf("rust message = %q", rust[0].Message)
	}

	py := verify.Parse("pytest", "FAILED tests/test_api.py::test_login - AssertionError: 401 != 200")
	if len(py) != 1 || py[0].File != "tests/test_api.py" || py[0].Test != "test_login" {
		return "", failf("pytest = %+v", py)
	}

	jest := verify.Parse("jest", "  ● Auth › rejects a bad token\n\n    at Object.<anonymous> (src/auth.test.ts:22:9)")
	if len(jest) != 1 || jest[0].File != "src/auth.test.ts" || jest[0].Line != 22 {
		return "", failf("jest = %+v", jest)
	}
	if !strings.Contains(jest[0].Test, "rejects a bad token") {
		return "", failf("jest test name = %q", jest[0].Test)
	}
	return "tsc, rust, pytest, jest", nil
}

func checkParseDedupe() (string, error) {
	var sb strings.Builder
	// The same diagnostic repeated, then 40 distinct ones.
	for i := 0; i < 5; i++ {
		sb.WriteString("./a.go:1:1: repeated problem\n")
	}
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&sb, "./b.go:%d:1: distinct problem %d\n", i+1, i)
	}
	fs := verify.Parse("go", sb.String())

	repeats := 0
	for _, f := range fs {
		if strings.Contains(f.Message, "repeated problem") {
			repeats++
		}
	}
	if repeats != 1 {
		return "", failf("the same diagnostic appears %d times after dedupe", repeats)
	}
	// A cascade of two hundred errors from one missing import is noise that
	// would fill the context window for nothing.
	if len(fs) > 12 {
		return "", failf("returned %d failures, want the list capped at 12", len(fs))
	}
	return fmt.Sprintf("45 lines -> %d failures", len(fs)), nil
}

// ---------- running ----------

func passCmd() string { return "exit 0" }
func failCmd() string { return "exit 3" }

func sleepCmd(seconds int) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("Start-Sleep -Seconds %d", seconds)
	}
	return fmt.Sprintf("sleep %d", seconds)
}

func checkVerifyStops() (string, error) {
	ws, cleanup, err := tempWS(nil)
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}
	checks := []verify.Check{
		{Name: "build", Command: failCmd(), Stage: verify.StageBuild, Parser: "generic"},
		{Name: "test", Command: passCmd(), Stage: verify.StageTest, Parser: "generic"},
	}
	rep := verify.Run(context.Background(), ws.Root(), checks, verify.Options{Timeout: 30 * time.Second})

	if rep.Passed {
		return "", failf("run passed despite a failing build")
	}
	if len(rep.Results) != 1 {
		return "", failf("ran %d checks, want 1 (test must be skipped)", len(rep.Results))
	}
	// Running tests against a tree that does not compile produces failures
	// describing the build error less usefully, and a model shown both often
	// chases the wrong one.
	if !containsStr(rep.Skipped, "test") {
		return "", failf("skipped = %v, want it to name test", rep.Skipped)
	}
	if fc := rep.FailedCheck(); fc == nil || fc.Check.Name != "build" {
		return "", failf("FailedCheck did not identify build")
	}
	if !strings.Contains(rep.Summary(), "not run: test") {
		return "", failf("summary does not report the skipped stage:\n%s", rep.Summary())
	}
	return "build failed, test skipped", nil
}

func checkVerifyOptional() (string, error) {
	ws, cleanup, err := tempWS(nil)
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}
	checks := []verify.Check{
		{Name: "build", Command: passCmd(), Stage: verify.StageBuild, Parser: "generic"},
		{Name: "lint", Command: failCmd(), Stage: verify.StageLint, Parser: "generic", Optional: true},
		{Name: "test", Command: passCmd(), Stage: verify.StageTest, Parser: "generic"},
	}
	rep := verify.Run(context.Background(), ws.Root(), checks, verify.Options{Timeout: 30 * time.Second})

	// A style complaint should not block an otherwise correct fix.
	if !rep.Passed {
		return "", failf("an optional lint failure failed the whole run")
	}
	if len(rep.Results) != 3 {
		return "", failf("ran %d checks, want all 3", len(rep.Results))
	}
	if rep.Results[1].Passed {
		return "", failf("the lint check was recorded as passing")
	}
	if rep.FailedCheck() != nil {
		return "", failf("FailedCheck returned an optional check")
	}
	return "lint failed, run still passed", nil
}

func checkVerifyTimeout() (string, error) {
	ws, cleanup, err := tempWS(nil)
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}
	checks := []verify.Check{
		{Name: "hang", Command: sleepCmd(30), Stage: verify.StageTest, Parser: "generic"},
	}
	start := time.Now()
	rep := verify.Run(context.Background(), ws.Root(), checks, verify.Options{Timeout: 2 * time.Second})
	elapsed := time.Since(start)

	if rep.Passed {
		return "", failf("a hung command was recorded as passing")
	}
	if elapsed > 15*time.Second {
		return "", failf("took %s; the timeout did not kill the process", elapsed.Round(time.Second))
	}
	if !rep.Results[0].TimedOut {
		return "", failf("TimedOut was not set")
	}
	if !strings.Contains(rep.Summary(), "timed out") {
		return "", failf("summary does not mention the timeout:\n%s", rep.Summary())
	}
	return fmt.Sprintf("killed after %s", elapsed.Round(time.Millisecond)), nil
}

func checkVerifySummarySize() (string, error) {
	// A realistic failing build: a handful of real diagnostics buried in
	// hundreds of lines of progress output.
	var raw strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&raw, "compiling module number %d of the project tree\n", i)
	}
	raw.WriteString("./internal/router/router.go:142:5: undefined: fooBar\n")
	raw.WriteString("./internal/router/router.go:150:2: missing return\n")
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&raw, "note: some additional diagnostic context line %d\n", i)
	}

	fs := verify.Parse("go", raw.String())
	if len(fs) != 2 {
		return "", failf("parsed %d failures from the noise, want 2: %+v", len(fs), fs)
	}
	rendered := ""
	for _, f := range fs {
		rendered += f.String() + "\n"
	}
	// This ratio is the entire token argument for the package.
	ratio := float64(raw.Len()) / float64(len(rendered))
	if ratio < 20 {
		return "", failf("summary is only %.1fx smaller than the raw output", ratio)
	}
	return fmt.Sprintf("%d bytes -> %d bytes (%.0fx smaller)", raw.Len(), len(rendered), ratio), nil
}

// ---------- journal ----------

func checkJournalFirstWins() (string, error) {
	ws, cleanup, err := tempWS(map[string]string{"a.txt": "original\n"})
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}
	j := checkpoint.New(ws.Root())

	// Three successive edits in one run. Reverting must restore the state at
	// the start of the run, not the state before the most recent edit.
	j.Record("a.txt", []byte("original\n"), true)
	j.Record("a.txt", []byte("first edit\n"), true)
	j.Record("a.txt", []byte("second edit\n"), true)

	if j.Len() != 1 {
		return "", failf("recorded %d entries for one file", j.Len())
	}
	if err := os.WriteFile(filepath.Join(ws.Root(), "a.txt"), []byte("third edit\n"), 0o644); err != nil {
		return "", failf("write: %v", err)
	}
	if _, err := j.Revert(); err != nil {
		return "", failf("revert: %v", err)
	}
	if got := readFileIn(ws, "a.txt"); got != "original\n" {
		return "", failf("after revert the file is %q, want the pre-run original", got)
	}
	return "restored to pre-run state", nil
}

func checkJournalRevert() (string, error) {
	ws, cleanup, err := tempWS(map[string]string{
		"keep.txt":   "untouched\n",
		"edited.txt": "before\n",
	})
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}
	j := checkpoint.New(ws.Root())

	j.Record("edited.txt", []byte("before\n"), true)
	_ = os.WriteFile(filepath.Join(ws.Root(), "edited.txt"), []byte("after\n"), 0o644)

	j.Record("created/new.txt", nil, false)
	_ = os.MkdirAll(filepath.Join(ws.Root(), "created"), 0o755)
	_ = os.WriteFile(filepath.Join(ws.Root(), "created", "new.txt"), []byte("brand new\n"), 0o644)

	restored, err := j.Revert()
	if err != nil {
		return "", failf("revert: %v", err)
	}
	if len(restored) != 2 {
		return "", failf("restored %d files, want 2", len(restored))
	}
	if got := readFileIn(ws, "edited.txt"); got != "before\n" {
		return "", failf("edited.txt = %q, want the original", got)
	}
	if _, err := os.Stat(filepath.Join(ws.Root(), "created", "new.txt")); !os.IsNotExist(err) {
		return "", failf("a file created during the run survived the revert")
	}
	// A file the agent never touched must be left completely alone.
	if got := readFileIn(ws, "keep.txt"); got != "untouched\n" {
		return "", failf("an untouched file was modified: %q", got)
	}
	return "edit restored, new file deleted, untouched file left alone", nil
}

func checkSnapshotOnlyOnWrite() (string, error) {
	ws, cleanup, err := tempWS(map[string]string{"a.txt": "keep me\n"})
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}
	j := checkpoint.New(ws.Root())

	// Denying approver: the edit never happens, so nothing may be journalled.
	env, _ := envFor(ws, false)
	env.Snapshot = j.Record
	tools.ApplyBlocks([]tools.Block{{Path: "a.txt", Search: "keep me\n", Replace: "clobbered\n"}}, env)
	if j.Len() != 0 {
		return "", failf("a declined edit recorded %d journal entries", j.Len())
	}

	// Allowing approver: the edit happens, so it must be journalled.
	env2, _ := envFor(ws, true)
	env2.Snapshot = j.Record
	rs := tools.ApplyBlocks([]tools.Block{{Path: "a.txt", Search: "keep me\n", Replace: "changed\n"}}, env2)
	if !rs[0].OK {
		return "", failf("edit failed: %s", rs[0].Message)
	}
	if j.Len() != 1 {
		return "", failf("an applied edit recorded %d journal entries, want 1", j.Len())
	}
	if _, err := j.Revert(); err != nil {
		return "", failf("revert: %v", err)
	}
	if got := readFileIn(ws, "a.txt"); got != "keep me\n" {
		return "", failf("revert produced %q", got)
	}
	return "declined records nothing, applied records once", nil
}

// ---------- self-repair loop ----------

// editThenDone scripts a model that makes one edit and then declares victory.
func editThenDone() [][]string {
	block := "main.go\n<<<<<<< SEARCH\nconst v = 1\n=======\nconst v = 2\n>>>>>>> REPLACE\n"
	return [][]string{
		{
			fmt.Sprintf(`{"choices":[{"index":0,"delta":{"content":"%s"}}]}`,
				strings.ReplaceAll(block, "\n", "\\n")),
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`,
		},
		{
			`{"choices":[{"index":0,"delta":{"content":"Done, bumped the constant."}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":6,"total_tokens":18}}`,
		},
	}
}

func checkAgentVerifyPass() (string, error) {
	ws, wsCleanup, err := tempWS(map[string]string{"main.go": "package main\n\nconst v = 1\n"})
	defer wsCleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}
	var hits int32
	srv := scriptedModel(editThenDone(), &hits)
	defer srv.Close()

	verifyCalls := 0
	ag, _, cleanup, err := agentFor(ws, srv, agent.Config{
		Class: "coder", MaxSteps: 6, Quiet: true, MaxRepairs: 3,
		Verify: func(ctx context.Context) (string, bool, int, error) {
			verifyCalls++
			return "VERIFICATION PASSED", true, 0, nil
		},
	})
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}

	out, err := ag.Run(context.Background(), "bump the constant")
	if err != nil {
		return "", failf("run: %v", err)
	}
	if verifyCalls != 1 {
		return "", failf("verify ran %d times, want 1", verifyCalls)
	}
	if !out.VerifyRan || !out.Verified {
		return "", failf("outcome says ran=%v verified=%v", out.VerifyRan, out.Verified)
	}
	if out.Repairs != 0 {
		return "", failf("repairs = %d on a passing run", out.Repairs)
	}
	if out.StopReason != "done" {
		return "", failf("stop reason = %q", out.StopReason)
	}
	return "verified on the first attempt", nil
}

func checkAgentRepair() (string, error) {
	ws, wsCleanup, err := tempWS(map[string]string{"main.go": "package main\n\nconst v = 1\n"})
	defer wsCleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}
	var hits int32
	srv := scriptedModel(editThenDone(), &hits)
	defer srv.Close()

	calls := 0
	ag, _, cleanup, err := agentFor(ws, srv, agent.Config{
		Class: "coder", MaxSteps: 8, Quiet: true, MaxRepairs: 3,
		Verify: func(ctx context.Context) (string, bool, int, error) {
			calls++
			if calls == 1 {
				return "VERIFICATION FAILED at the build stage\n\n1 problem(s):\n  main.go:3:7  undefined: v", false, 1, nil
			}
			return "VERIFICATION PASSED", true, 0, nil
		},
	})
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}

	out, err := ag.Run(context.Background(), "bump the constant")
	if err != nil {
		return "", failf("run: %v", err)
	}
	if out.Repairs != 1 {
		return "", failf("repairs = %d, want 1", out.Repairs)
	}
	if !out.Verified {
		return "", failf("run did not end verified after a successful repair")
	}
	// The failure detail must reach the model, not just a "it failed" flag.
	var gotFailure bool
	for _, m := range ag.Messages() {
		if m.Role == "user" && strings.Contains(m.Content, "main.go:3:7") {
			gotFailure = true
		}
	}
	if !gotFailure {
		return "", failf("the located failure was never handed back to the model")
	}
	return "failed once, repaired, verified", nil
}

func checkAgentRepairBounded() (string, error) {
	ws, wsCleanup, err := tempWS(map[string]string{"main.go": "package main\n\nconst v = 1\n"})
	defer wsCleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}
	var hits int32
	srv := scriptedModel(editThenDone(), &hits)
	defer srv.Close()

	calls := 0
	ag, _, cleanup, err := agentFor(ws, srv, agent.Config{
		Class: "coder", MaxSteps: 20, Quiet: true, MaxRepairs: 2,
		Verify: func(ctx context.Context) (string, bool, int, error) {
			calls++
			return "VERIFICATION FAILED\n  main.go:3:7  still broken", false, 1, nil
		},
	})
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}

	out, err := ag.Run(context.Background(), "bump the constant")
	if err != nil {
		return "", failf("run: %v", err)
	}
	// Beyond a few rounds a model that has not fixed it is thrashing, and each
	// round costs a full test run.
	if out.Repairs != 2 {
		return "", failf("repairs = %d, want the configured cap of 2", out.Repairs)
	}
	if calls != 3 {
		return "", failf("verify ran %d times, want 3 (initial + 2 repairs)", calls)
	}
	if out.Verified {
		return "", failf("outcome claims verified after a permanently failing check")
	}
	if !strings.Contains(out.StopReason, "verification failed") {
		return "", failf("stop reason = %q", out.StopReason)
	}
	return "stopped after 2 repairs, reported unverified", nil
}

func checkAgentNoChangesNoVerify() (string, error) {
	ws, wsCleanup, err := tempWS(map[string]string{"main.go": "package main\n"})
	defer wsCleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}
	// A model that answers a question without editing anything.
	var hits int32
	srv := scriptedModel([][]string{{
		`{"choices":[{"index":0,"delta":{"content":"The constant is 1."}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":5,"total_tokens":13}}`,
	}}, &hits)
	defer srv.Close()

	calls := 0
	ag, _, cleanup, err := agentFor(ws, srv, agent.Config{
		Class: "coder", MaxSteps: 4, Quiet: true,
		Verify: func(ctx context.Context) (string, bool, int, error) {
			calls++
			return "", true, 0, nil
		},
	})
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}

	out, err := ag.Run(context.Background(), "what is the constant?")
	if err != nil {
		return "", failf("run: %v", err)
	}
	// Nothing changed, so nothing can have broken; a full test run to confirm
	// that is pure latency.
	if calls != 0 {
		return "", failf("verify ran %d times on a read-only run", calls)
	}
	// And "not run" must never be reported as "verified".
	if out.Verified || out.VerifyRan {
		return "", failf("outcome says ran=%v verified=%v for a run with no checks",
			out.VerifyRan, out.Verified)
	}
	if out.StopReason != "done" {
		return "", failf("stop reason = %q", out.StopReason)
	}
	return "skipped, and not reported as verified", nil
}
