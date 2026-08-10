// Package verify runs a project's own build, lint, and test commands, parses
// what failed out of the output, and reports it compactly.
//
// This is the phase that separates "the model wrote plausible code" from "the
// code works". The parsing matters as much as the running: handing a model
// 3,000 lines of test output costs a fortune in tokens and buries the three
// lines that identify the bug.
package verify

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Stage orders checks by how much they invalidate later ones. A build failure
// makes test results meaningless, so the runner stops at the first failing
// stage rather than reporting a cascade.
type Stage int

const (
	StageBuild Stage = iota
	StageLint
	StageTest
)

func (s Stage) String() string {
	switch s {
	case StageBuild:
		return "build"
	case StageLint:
		return "lint"
	default:
		return "test"
	}
}

type Check struct {
	Name    string
	Command string
	Stage   Stage
	// Parser selects the output parser; defaults to the project kind.
	Parser string
	// Optional checks report failures but do not fail the run. Linters land
	// here: a style complaint should not block an otherwise correct fix.
	Optional bool
}

type Project struct {
	Kind   string
	Checks []Check
}

func (p Project) Has() bool { return len(p.Checks) > 0 }

// Detect infers a project's verification commands from its marker files.
//
// The commands chosen are deliberately the cheap, whole-project ones. A
// verification pass that takes four minutes will not get run.
func Detect(root string) Project {
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(root, name))
		return err == nil
	}

	switch {
	case exists("go.mod"):
		return Project{Kind: "go", Checks: []Check{
			{Name: "build", Command: "go build ./...", Stage: StageBuild, Parser: "go"},
			{Name: "vet", Command: "go vet ./...", Stage: StageLint, Parser: "go", Optional: true},
			{Name: "test", Command: "go test ./...", Stage: StageTest, Parser: "gotest"},
		}}

	case exists("Cargo.toml"):
		return Project{Kind: "rust", Checks: []Check{
			{Name: "build", Command: "cargo build --quiet", Stage: StageBuild, Parser: "rust"},
			{Name: "test", Command: "cargo test --quiet", Stage: StageTest, Parser: "rust"},
		}}

	case exists("package.json"):
		return detectNode(root)

	case exists("pyproject.toml"), exists("setup.py"), exists("requirements.txt"), exists("tox.ini"):
		checks := []Check{{Name: "test", Command: "python -m pytest -q", Stage: StageTest, Parser: "pytest"}}
		if exists("ruff.toml") || exists(".ruff.toml") {
			checks = append([]Check{{
				Name: "lint", Command: "ruff check .", Stage: StageLint, Parser: "generic", Optional: true,
			}}, checks...)
		}
		return Project{Kind: "python", Checks: checks}

	case exists("Makefile"), exists("makefile"):
		return detectMake(root)
	}
	return Project{Kind: "unknown"}
}

func detectNode(root string) Project {
	p := Project{Kind: "node"}
	raw, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return p
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return p
	}

	// Prefer a typecheck over a full build: it is far faster and catches the
	// class of error a model actually makes.
	if _, ok := os.Stat(filepath.Join(root, "tsconfig.json")); ok == nil {
		p.Checks = append(p.Checks, Check{
			Name: "typecheck", Command: "npx --no-install tsc --noEmit", Stage: StageBuild, Parser: "tsc",
		})
	}
	if _, ok := pkg.Scripts["build"]; ok && len(p.Checks) == 0 {
		p.Checks = append(p.Checks, Check{
			Name: "build", Command: "npm run build --silent", Stage: StageBuild, Parser: "generic",
		})
	}
	if _, ok := pkg.Scripts["lint"]; ok {
		p.Checks = append(p.Checks, Check{
			Name: "lint", Command: "npm run lint --silent", Stage: StageLint, Parser: "generic", Optional: true,
		})
	}
	if _, ok := pkg.Scripts["test"]; ok {
		p.Checks = append(p.Checks, Check{
			Name: "test", Command: "npm test --silent", Stage: StageTest, Parser: "jest",
		})
	}
	return p
}

// detectMake only claims targets it can see declared, so it never invents a
// `make test` that does not exist.
func detectMake(root string) Project {
	p := Project{Kind: "make"}
	name := "Makefile"
	raw, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		raw, err = os.ReadFile(filepath.Join(root, "makefile"))
		if err != nil {
			return p
		}
	}
	targets := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		if len(line) == 0 || line[0] == '\t' || line[0] == '#' || line[0] == ' ' {
			continue
		}
		if i := strings.IndexByte(line, ':'); i > 0 {
			t := strings.TrimSpace(line[:i])
			if t != "" && !strings.ContainsAny(t, " \t$") {
				targets[t] = true
			}
		}
	}
	for _, spec := range []struct {
		target string
		stage  Stage
		opt    bool
	}{
		{"build", StageBuild, false},
		{"all", StageBuild, false},
		{"lint", StageLint, true},
		{"test", StageTest, false},
		{"check", StageTest, false},
	} {
		if targets[spec.target] {
			p.Checks = append(p.Checks, Check{
				Name: spec.target, Command: "make " + spec.target,
				Stage: spec.stage, Parser: "generic", Optional: spec.opt,
			})
			// One command per stage is enough; `all` is a fallback for `build`.
			if spec.target == "build" {
				break
			}
		}
	}
	return p
}
