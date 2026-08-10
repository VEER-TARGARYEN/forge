package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/VEER-TARGARYEN/forge/internal/fsx"
)

// MatchGlob reports whether name matches pattern, supporting ** for "any
// number of path segments". Go's path.Match has no ** , and ** is the pattern
// people actually reach for ("**/*.go"), so it is implemented here rather
// than pulling in a dependency.
func MatchGlob(pattern, name string) bool {
	pattern = filepath.ToSlash(pattern)
	name = filepath.ToSlash(name)
	// A bare "*.go" should match at any depth; that is what users mean.
	if !strings.Contains(pattern, "/") {
		return matchSegments([]string{"**", pattern}, strings.Split(name, "/"))
	}
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			// ** may consume any number of segments, including none.
			for i := 0; i <= len(name); i++ {
				if matchSegments(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		ok, err := path.Match(pat[0], name[0])
		if err != nil || !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

// walkFiles visits every non-skipped file under root.
var walkFiles = fsx.WalkFiles

// ---------- glob ----------

type Glob struct{}

func (Glob) Spec() Spec {
	return Spec{
		Name: "glob",
		Description: "Find files by name pattern. Supports ** for any depth, for example " +
			"'**/*.go' or 'internal/**/*_test.go'. Returns paths sorted by modification time, newest first.",
		Schema: obj(map[string]any{
			"pattern": str("Glob pattern, e.g. '**/*.go'."),
			"path":    str("Directory to search under. Defaults to the workspace root."),
			"limit":   integer("Maximum paths to return. Defaults to 200."),
		}, "pattern"),
	}
}

func (Glob) Mutates() bool { return false }

func (Glob) Run(ctx context.Context, raw json.RawMessage, env *Env) (*Result, error) {
	var a struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
		Limit   int    `json:"limit"`
	}
	if err := ParseArgs(raw, &a); err != nil {
		return Errorf("%v", err), nil
	}
	if a.Pattern == "" {
		return Errorf("pattern is required"), nil
	}
	if a.Path == "" {
		a.Path = "."
	}
	if a.Limit <= 0 {
		a.Limit = 200
	}
	root, err := env.WS.Resolve(a.Path)
	if err != nil {
		return Errorf("%v", err), nil
	}

	type hit struct {
		rel  string
		mod  int64
		size int64
	}
	var hits []hit
	err = walkFiles(root, func(abs string, d fs.DirEntry) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel := env.WS.Rel(abs)
		if !MatchGlob(a.Pattern, rel) && !MatchGlob(a.Pattern, filepath.Base(abs)) {
			return nil
		}
		var mod, size int64
		if info, err := d.Info(); err == nil {
			mod, size = info.ModTime().UnixNano(), info.Size()
		}
		hits = append(hits, hit{rel, mod, size})
		return nil
	})
	if err != nil && ctx.Err() != nil {
		return Errorf("cancelled"), nil
	}
	if len(hits) == 0 {
		return Textf("No files match %q under %s.", a.Pattern, a.Path), nil
	}
	// Newest first: recently touched files are usually the relevant ones.
	sort.Slice(hits, func(i, j int) bool { return hits[i].mod > hits[j].mod })

	total := len(hits)
	if len(hits) > a.Limit {
		hits = hits[:a.Limit]
	}
	var sb strings.Builder
	for _, h := range hits {
		fmt.Fprintf(&sb, "%s  (%s)\n", h.rel, humanSize(h.size))
	}
	if total > len(hits) {
		fmt.Fprintf(&sb, "\n[%d more matches not shown]\n", total-len(hits))
	}
	body, note := env.Clip("glob "+a.Pattern, sb.String())
	return &Result{
		Content: fmt.Sprintf("%d files matching %q:\n%s%s", total, a.Pattern, body, note),
		Display: fmt.Sprintf("glob %q -> %d files", a.Pattern, total),
	}, nil
}

// ---------- grep ----------

type Grep struct{}

func (Grep) Spec() Spec {
	return Spec{
		Name: "grep",
		Description: "Search file contents with a regular expression (Go/RE2 syntax). " +
			"This is the fastest way to locate code — prefer it over reading files speculatively.",
		Schema: obj(map[string]any{
			"pattern":          str("Regular expression to search for."),
			"path":             str("Directory to search under. Defaults to the workspace root."),
			"glob":             str("Only search files matching this glob, e.g. '**/*.go'."),
			"case_insensitive": boolean("Match case-insensitively."),
			"context":          integer("Lines of context around each match. Defaults to 0."),
			"limit":            integer("Maximum matching lines to return. Defaults to 100."),
			"files_only":       boolean("Return only the list of matching files, not the lines."),
		}, "pattern"),
	}
}

func (Grep) Mutates() bool { return false }

type grepHit struct {
	file  string
	line  int
	text  string
	above []string
	below []string
}

func (Grep) Run(ctx context.Context, raw json.RawMessage, env *Env) (*Result, error) {
	var a struct {
		Pattern         string `json:"pattern"`
		Path            string `json:"path"`
		Glob            string `json:"glob"`
		CaseInsensitive bool   `json:"case_insensitive"`
		Context         int    `json:"context"`
		Limit           int    `json:"limit"`
		FilesOnly       bool   `json:"files_only"`
	}
	if err := ParseArgs(raw, &a); err != nil {
		return Errorf("%v", err), nil
	}
	if a.Pattern == "" {
		return Errorf("pattern is required"), nil
	}
	if a.Path == "" {
		a.Path = "."
	}
	if a.Limit <= 0 {
		a.Limit = 100
	}
	if a.Context < 0 {
		a.Context = 0
	}
	if a.Context > 10 {
		a.Context = 10
	}

	expr := a.Pattern
	if a.CaseInsensitive {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return Errorf("invalid regular expression %q: %v", a.Pattern, err), nil
	}
	root, err := env.WS.Resolve(a.Path)
	if err != nil {
		return Errorf("%v", err), nil
	}

	// Collect candidate files first, then scan them in parallel. On a 4-core
	// laptop the scan is IO-bound enough that a small worker pool is a clear
	// win over a serial walk.
	var files []string
	_ = walkFiles(root, func(abs string, d fs.DirEntry) error {
		if a.Glob != "" {
			rel := env.WS.Rel(abs)
			if !MatchGlob(a.Glob, rel) && !MatchGlob(a.Glob, filepath.Base(abs)) {
				return nil
			}
		}
		if info, err := d.Info(); err == nil && info.Size() > 8<<20 {
			return nil // a >8 MB text file is generated data, not source
		}
		files = append(files, abs)
		return nil
	})

	workers := runtime.NumCPU()
	if workers > 8 {
		workers = 8
	}
	if workers < 1 {
		workers = 1
	}

	var (
		mu      sync.Mutex
		hits    []grepHit
		fileSet = map[string]int{}
		wg      sync.WaitGroup
	)
	jobs := make(chan string, len(files))
	for _, f := range files {
		jobs <- f
	}
	close(jobs)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for abs := range jobs {
				if ctx.Err() != nil {
					return
				}
				found := scanFile(abs, re, a.Context)
				if len(found) == 0 {
					continue
				}
				rel := env.WS.Rel(abs)
				mu.Lock()
				fileSet[rel] += len(found)
				for i := range found {
					found[i].file = rel
				}
				hits = append(hits, found...)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return Errorf("cancelled"), nil
	}

	if len(hits) == 0 {
		return Textf("No matches for %q under %s.", a.Pattern, a.Path), nil
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].file != hits[j].file {
			return hits[i].file < hits[j].file
		}
		return hits[i].line < hits[j].line
	})

	var sb strings.Builder
	if a.FilesOnly {
		names := make([]string, 0, len(fileSet))
		for f := range fileSet {
			names = append(names, f)
		}
		sort.Strings(names)
		for _, f := range names {
			fmt.Fprintf(&sb, "%s  (%d matches)\n", f, fileSet[f])
		}
		body, note := env.Clip("grep "+a.Pattern, sb.String())
		return &Result{
			Content: fmt.Sprintf("%d files match %q:\n%s%s", len(names), a.Pattern, body, note),
			Display: fmt.Sprintf("grep %q -> %d files", a.Pattern, len(names)),
		}, nil
	}

	total := len(hits)
	if len(hits) > a.Limit {
		hits = hits[:a.Limit]
	}
	lastFile := ""
	for _, h := range hits {
		if h.file != lastFile {
			if lastFile != "" {
				sb.WriteString("\n")
			}
			fmt.Fprintf(&sb, "%s\n", h.file)
			lastFile = h.file
		}
		for k, c := range h.above {
			fmt.Fprintf(&sb, "  %6d- %s\n", h.line-len(h.above)+k, c)
		}
		fmt.Fprintf(&sb, "  %6d: %s\n", h.line, h.text)
		for k, c := range h.below {
			fmt.Fprintf(&sb, "  %6d- %s\n", h.line+k+1, c)
		}
	}
	if total > len(hits) {
		fmt.Fprintf(&sb, "\n[%d more matches. Narrow the pattern or set files_only.]\n", total-len(hits))
	}
	body, note := env.Clip("grep "+a.Pattern, sb.String())
	return &Result{
		Content: fmt.Sprintf("%d matches in %d files for %q:\n\n%s%s", total, len(fileSet), a.Pattern, body, note),
		Display: fmt.Sprintf("grep %q -> %d matches in %d files", a.Pattern, total, len(fileSet)),
	}, nil
}

func scanFile(abs string, re *regexp.Regexp, ctxLines int) []grepHit {
	f, err := os.Open(abs)
	if err != nil {
		return nil
	}
	defer f.Close()

	head := make([]byte, 8000)
	n, _ := f.Read(head)
	if IsBinary(head[:n]) {
		return nil
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil
	}

	var (
		out    []grepHit
		window []string
		lineNo int
	)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)

	// pending maps a match's line number to its index in out. It stores an
	// index rather than a *grepHit deliberately: appending to out can
	// reallocate the backing array, which would leave a stored pointer aimed
	// at the old one and silently drop the trailing context of every match
	// that had a later match appended after it.
	pending := map[int]int{}
	for sc.Scan() {
		lineNo++
		line := strings.TrimRight(sc.Text(), "\r")

		// Fill in the trailing context of earlier matches.
		for at, idx := range pending {
			if lineNo-at <= ctxLines {
				out[idx].below = append(out[idx].below, line)
			}
			if lineNo-at >= ctxLines {
				delete(pending, at)
			}
		}

		if re.MatchString(line) {
			h := grepHit{line: lineNo, text: line}
			if ctxLines > 0 {
				start := len(window) - ctxLines
				if start < 0 {
					start = 0
				}
				h.above = append([]string(nil), window[start:]...)
			}
			out = append(out, h)
			if ctxLines > 0 {
				pending[lineNo] = len(out) - 1
			}
		}

		if ctxLines > 0 {
			window = append(window, line)
			if len(window) > ctxLines {
				window = window[1:]
			}
		}
		if len(out) > 5000 {
			break // a pattern this broad is a mistake; stop burning IO on it
		}
	}
	return out
}
