package verify

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Failure is one located problem extracted from tool output.
type Failure struct {
	File    string
	Line    int
	Col     int
	Test    string
	Message string
}

func (f Failure) String() string {
	var b strings.Builder
	switch {
	case f.File != "" && f.Line > 0:
		fmt.Fprintf(&b, "%s:%d", f.File, f.Line)
		if f.Col > 0 {
			fmt.Fprintf(&b, ":%d", f.Col)
		}
	case f.File != "":
		b.WriteString(f.File)
	case f.Test != "":
		b.WriteString(f.Test)
	default:
		b.WriteString("(no location)")
	}
	if f.Test != "" && f.File != "" {
		fmt.Fprintf(&b, " [%s]", f.Test)
	}
	if f.Message != "" {
		fmt.Fprintf(&b, "  %s", f.Message)
	}
	return b.String()
}

var (
	// go build / go vet: path/file.go:12:5: message  (column optional)
	goDiag = regexp.MustCompile(`^\s*(\S+\.go):(\d+)(?::(\d+))?:\s+(.+)$`)
	// go test: --- FAIL: TestName (0.00s)
	goTestFail = regexp.MustCompile(`^\s*--- FAIL: (\S+)`)
	// go test assertion: file_test.go:42: message
	goTestLoc = regexp.MustCompile(`^\s+(\S+\.go):(\d+):\s*(.*)$`)
	// rustc: error[E0308]: mismatched types   /   --> src/main.rs:10:5
	rustMsg  = regexp.MustCompile(`^(error|warning)(\[\w+\])?:\s+(.+)$`)
	rustLoc  = regexp.MustCompile(`^\s*-->\s+(\S+?):(\d+):(\d+)`)
	rustTest = regexp.MustCompile(`^\s*(?:test\s+)?(\S+)\s+\.\.\.\s+FAILED`)
	// tsc: path/file.ts(12,5): error TS2345: message
	tscDiag = regexp.MustCompile(`^\s*(\S+?)\((\d+),(\d+)\):\s+error\s+\S+:\s+(.+)$`)
	// pytest: FAILED tests/test_x.py::test_name - AssertionError: ...
	pytestFailed = regexp.MustCompile(`^FAILED\s+(\S+?)(?:::(\S+))?\s*(?:-\s*(.+))?$`)
	// pytest traceback location: path/file.py:42: AssertionError
	pytestLoc = regexp.MustCompile(`^(\S+\.py):(\d+):\s*(.+)$`)
	// jest: ● Suite › test name   /   at fn (path/file.ts:12:5)
	jestTest = regexp.MustCompile(`^\s*●\s+(.+?)\s*$`)
	jestLoc  = regexp.MustCompile(`^\s*at .*\((\S+?):(\d+):(\d+)\)`)
	// generic: anything shaped like path:line: message
	genericDiag = regexp.MustCompile(`^\s*(\S+?):(\d+)(?::(\d+))?:\s+(.+)$`)
)

// maxFailures caps how many problems are reported. A model fixes the first few
// and re-runs; two hundred cascading errors from one missing import are noise
// that would fill the context window for nothing.
const maxFailures = 12

// Parse extracts located failures from command output.
//
// Every parser degrades to the generic path/line form rather than returning
// nothing, so an unrecognised toolchain still produces something actionable.
func Parse(parser, output string) []Failure {
	lines := strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n")
	var out []Failure

	switch parser {
	case "go":
		out = parseGo(lines)
	case "gotest":
		out = parseGoTest(lines)
	case "rust":
		out = parseRust(lines)
	case "tsc":
		out = parseTSC(lines)
	case "pytest":
		out = parsePytest(lines)
	case "jest":
		out = parseJest(lines)
	default:
		out = parseGeneric(lines)
	}
	if len(out) == 0 {
		out = parseGeneric(lines)
	}
	return dedupe(out)
}

func parseGo(lines []string) []Failure {
	var out []Failure
	for _, l := range lines {
		if m := goDiag.FindStringSubmatch(l); m != nil {
			out = append(out, Failure{
				File: m[1], Line: atoi(m[2]), Col: atoi(m[3]), Message: strings.TrimSpace(m[4]),
			})
		}
	}
	return out
}

// parseGoTest attributes each assertion line to the test that was running,
// which is the pairing a model needs: a bare "file:42: want 3 got 4" does not
// say which case produced it.
func parseGoTest(lines []string) []Failure {
	var out []Failure
	current := ""
	for _, l := range lines {
		if m := goTestFail.FindStringSubmatch(l); m != nil {
			current = m[1]
			continue
		}
		if m := goTestLoc.FindStringSubmatch(l); m != nil {
			out = append(out, Failure{
				File: m[1], Line: atoi(m[2]), Test: current, Message: strings.TrimSpace(m[3]),
			})
			continue
		}
		// A compile error inside a test package surfaces in the same stream.
		if m := goDiag.FindStringSubmatch(l); m != nil {
			out = append(out, Failure{
				File: m[1], Line: atoi(m[2]), Col: atoi(m[3]), Message: strings.TrimSpace(m[4]),
			})
		}
	}
	// A test that failed with no located assertion still needs reporting.
	seen := map[string]bool{}
	for _, f := range out {
		seen[f.Test] = true
	}
	for _, l := range lines {
		if m := goTestFail.FindStringSubmatch(l); m != nil && !seen[m[1]] {
			out = append(out, Failure{Test: m[1], Message: "failed"})
			seen[m[1]] = true
		}
	}
	return out
}

func parseRust(lines []string) []Failure {
	var out []Failure
	pending := ""
	for _, l := range lines {
		if m := rustMsg.FindStringSubmatch(l); m != nil {
			if m[1] == "error" {
				pending = strings.TrimSpace(m[3])
			}
			continue
		}
		if m := rustLoc.FindStringSubmatch(l); m != nil && pending != "" {
			out = append(out, Failure{
				File: m[1], Line: atoi(m[2]), Col: atoi(m[3]), Message: pending,
			})
			pending = ""
			continue
		}
		if m := rustTest.FindStringSubmatch(l); m != nil {
			out = append(out, Failure{Test: m[1], Message: "failed"})
		}
	}
	if pending != "" {
		out = append(out, Failure{Message: pending})
	}
	return out
}

func parseTSC(lines []string) []Failure {
	var out []Failure
	for _, l := range lines {
		if m := tscDiag.FindStringSubmatch(l); m != nil {
			out = append(out, Failure{
				File: m[1], Line: atoi(m[2]), Col: atoi(m[3]), Message: strings.TrimSpace(m[4]),
			})
		}
	}
	return out
}

func parsePytest(lines []string) []Failure {
	var out []Failure
	for _, l := range lines {
		if m := pytestFailed.FindStringSubmatch(l); m != nil {
			out = append(out, Failure{
				File: m[1], Test: m[2], Message: strings.TrimSpace(m[3]),
			})
			continue
		}
		if m := pytestLoc.FindStringSubmatch(l); m != nil {
			out = append(out, Failure{
				File: m[1], Line: atoi(m[2]), Message: strings.TrimSpace(m[3]),
			})
		}
	}
	return out
}

func parseJest(lines []string) []Failure {
	var out []Failure
	current := ""
	for _, l := range lines {
		if m := jestTest.FindStringSubmatch(l); m != nil {
			current = strings.TrimSpace(m[1])
			continue
		}
		if m := jestLoc.FindStringSubmatch(l); m != nil && current != "" {
			out = append(out, Failure{
				File: m[1], Line: atoi(m[2]), Col: atoi(m[3]), Test: current,
			})
			current = ""
		}
	}
	return out
}

func parseGeneric(lines []string) []Failure {
	var out []Failure
	for _, l := range lines {
		low := strings.ToLower(l)
		if !strings.Contains(low, "error") && !strings.Contains(low, "fail") &&
			!strings.Contains(low, "warning") {
			continue
		}
		if m := genericDiag.FindStringSubmatch(l); m != nil {
			out = append(out, Failure{
				File: m[1], Line: atoi(m[2]), Col: atoi(m[3]), Message: strings.TrimSpace(m[4]),
			})
			continue
		}
		if t := strings.TrimSpace(l); t != "" && len(t) < 300 {
			out = append(out, Failure{Message: t})
		}
	}
	return out
}

// dedupe collapses repeats and caps the list. Toolchains routinely report the
// same location several times with different phrasing.
func dedupe(in []Failure) []Failure {
	seen := map[string]bool{}
	out := make([]Failure, 0, len(in))
	for _, f := range in {
		key := fmt.Sprintf("%s|%d|%s|%s", f.File, f.Line, f.Test, f.Message)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, f)
		if len(out) >= maxFailures {
			break
		}
	}
	return out
}

func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}
