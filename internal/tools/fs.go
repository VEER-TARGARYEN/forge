package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/VEER-TARGARYEN/forge/internal/diff"
)

// ---------- read_file ----------

type ReadFile struct{}

func (ReadFile) Spec() Spec {
	return Spec{
		Name: "read_file",
		Description: "Read a file from the workspace. Returns lines prefixed with line numbers. " +
			"Use offset and limit to read a slice of a large file instead of the whole thing.",
		Schema: obj(map[string]any{
			"path":   str("Path relative to the workspace root."),
			"offset": integer("1-based line to start at. Omit to start at the beginning."),
			"limit":  integer("Maximum lines to return. Defaults to 2000."),
		}, "path"),
	}
}

func (ReadFile) Mutates() bool { return false }

func (ReadFile) Run(ctx context.Context, raw json.RawMessage, env *Env) (*Result, error) {
	var a struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := ParseArgs(raw, &a); err != nil {
		return Errorf("%v", err), nil
	}
	abs, err := env.WS.Resolve(a.Path)
	if err != nil {
		return Errorf("%v", err), nil
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return Errorf("read %s: %v", a.Path, err), nil
	}
	if IsBinary(data) {
		return Errorf("%s looks like a binary file (%d bytes); not reading it", a.Path, len(data)), nil
	}

	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	total := len(lines)

	start := 0
	if a.Offset > 0 {
		start = a.Offset - 1
	}
	if start >= total {
		return Textf("%s has %d lines; offset %d is past the end.", a.Path, total, a.Offset), nil
	}
	limit := a.Limit
	if limit <= 0 {
		limit = 2000
	}
	end := start + limit
	if end > total {
		end = total
	}

	var sb strings.Builder
	for i := start; i < end; i++ {
		fmt.Fprintf(&sb, "%6d\t%s\n", i+1, lines[i])
	}
	body, note := env.Clip("read_file "+a.Path, sb.String())

	header := fmt.Sprintf("%s (lines %d-%d of %d)\n", a.Path, start+1, end, total)
	if note == "" && end < total {
		body += fmt.Sprintf("\n[%d more lines. Call read_file again with offset=%d.]\n", total-end, end+1)
	}
	return &Result{
		Content: header + body + note,
		Display: fmt.Sprintf("read %s (%d-%d of %d lines)", a.Path, start+1, end, total),
	}, nil
}

// ---------- write_file ----------

type WriteFile struct{}

func (WriteFile) Spec() Spec {
	return Spec{
		Name: "write_file",
		Description: "Create a file, or replace its entire contents. " +
			"For changing part of an existing file, prefer a SEARCH/REPLACE block or edit_file — " +
			"rewriting a whole file to change three lines wastes output tokens and risks losing code.",
		Schema: obj(map[string]any{
			"path":    str("Path relative to the workspace root."),
			"content": str("Full file contents to write."),
		}, "path", "content"),
	}
}

func (WriteFile) Mutates() bool { return true }

func (WriteFile) Run(ctx context.Context, raw json.RawMessage, env *Env) (*Result, error) {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := ParseArgs(raw, &a); err != nil {
		return Errorf("%v", err), nil
	}
	abs, err := env.WS.Resolve(a.Path)
	if err != nil {
		return Errorf("%v", err), nil
	}

	old := ""
	existed := false
	if b, err := os.ReadFile(abs); err == nil {
		old = string(b)
		existed = true
	}

	kind, summary := "write", fmt.Sprintf("create %s (%d bytes)", a.Path, len(a.Content))
	if existed {
		added, removed := diff.Summary(old, a.Content)
		summary = fmt.Sprintf("overwrite %s (+%d -%d)", a.Path, added, removed)
	}
	preview := diff.Unified(a.Path, old, a.Content, 3)

	if err := env.Approver.Approve(ApprovalRequest{
		Tool: "write_file", Kind: kind, Summary: summary, Detail: preview, Path: a.Path,
		Risky: existed,
	}); err != nil {
		return Errorf("declined: %v", err), nil
	}

	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return Errorf("mkdir for %s: %v", a.Path, err), nil
	}
	// Recorded after approval, before the write: a declined edit must leave
	// nothing in the undo journal.
	env.noteSnapshot(env.WS.Rel(abs), []byte(old), existed)
	if err := os.WriteFile(abs, []byte(a.Content), 0o644); err != nil {
		return Errorf("write %s: %v", a.Path, err), nil
	}
	env.noteChange(env.WS.Rel(abs))
	added, removed := diff.Summary(old, a.Content)
	return &Result{
		Content: fmt.Sprintf("Wrote %s (+%d -%d lines).", a.Path, added, removed),
		Display: summary,
	}, nil
}

// ---------- list_dir ----------

type ListDir struct{}

func (ListDir) Spec() Spec {
	return Spec{
		Name:        "list_dir",
		Description: "List files and directories. Skips build output, dependency caches, and VCS internals.",
		Schema: obj(map[string]any{
			"path":  str("Directory relative to the workspace root. Defaults to the root."),
			"depth": integer("How many levels to descend. Defaults to 2."),
		}),
	}
}

func (ListDir) Mutates() bool { return false }

func (ListDir) Run(ctx context.Context, raw json.RawMessage, env *Env) (*Result, error) {
	var a struct {
		Path  string `json:"path"`
		Depth int    `json:"depth"`
	}
	if err := ParseArgs(raw, &a); err != nil {
		return Errorf("%v", err), nil
	}
	if a.Path == "" {
		a.Path = "."
	}
	if a.Depth <= 0 {
		a.Depth = 2
	}
	root, err := env.WS.Resolve(a.Path)
	if err != nil {
		return Errorf("%v", err), nil
	}

	var sb strings.Builder
	count := 0
	var walk func(dir string, depth int, prefix string) error
	walk = func(dir string, depth int, prefix string) error {
		if depth > a.Depth {
			return nil
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].IsDir() != entries[j].IsDir() {
				return entries[i].IsDir()
			}
			return entries[i].Name() < entries[j].Name()
		})
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") && e.Name() != ".github" {
				continue
			}
			if e.IsDir() && SkipDirs[e.Name()] {
				fmt.Fprintf(&sb, "%s%s/  (skipped)\n", prefix, e.Name())
				continue
			}
			count++
			if e.IsDir() {
				fmt.Fprintf(&sb, "%s%s/\n", prefix, e.Name())
				if err := walk(filepath.Join(dir, e.Name()), depth+1, prefix+"  "); err != nil {
					return err
				}
			} else {
				size := int64(0)
				if info, err := e.Info(); err == nil {
					size = info.Size()
				}
				fmt.Fprintf(&sb, "%s%s  (%s)\n", prefix, e.Name(), humanSize(size))
			}
		}
		return nil
	}
	if err := walk(root, 1, ""); err != nil {
		return Errorf("list %s: %v", a.Path, err), nil
	}
	if count == 0 {
		return Textf("%s is empty.", a.Path), nil
	}
	body, note := env.Clip("list_dir "+a.Path, sb.String())
	return &Result{
		Content: fmt.Sprintf("%s (depth %d)\n%s%s", a.Path, a.Depth, body, note),
		Display: fmt.Sprintf("list %s (%d entries)", a.Path, count),
	}, nil
}

func humanSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fK", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/(1024*1024))
	}
}
