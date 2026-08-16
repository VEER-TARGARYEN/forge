package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/VEER-TARGARYEN/forge/internal/diff"
)

// Block is one SEARCH/REPLACE edit parsed out of the model's message content.
//
// Why this exists alongside the edit_file tool: a JSON tool call has to carry
// code as an escaped string. Small models mangle that constantly — a stray
// backslash or an unescaped newline loses the whole call. Emitting code as
// plain text between markers removes the escaping problem entirely, which is
// the single biggest reliability win available on a 7B-class model.
type Block struct {
	Path    string
	Search  string
	Replace string
	// Line is where the block started in the message, for error reporting.
	Line int
}

const (
	markSearch  = "<<<<<<<"
	markDivider = "======="
	markReplace = ">>>>>>>"
)

// ParseBlocks extracts every SEARCH/REPLACE block from a message.
//
// It is deliberately tolerant about the surrounding shape: the path may sit
// directly above the markers or above a code fence, and the marker lines may
// or may not carry the SEARCH/REPLACE words. Rejecting a well-formed edit over
// a missing fence would cost a full round trip at 7 tok/s.
func ParseBlocks(text string) ([]Block, []string) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var blocks []Block
	var problems []string

	for i := 0; i < len(lines); i++ {
		if !strings.HasPrefix(strings.TrimSpace(lines[i]), markSearch) {
			continue
		}
		start := i
		path := pathAbove(lines, i)

		// Collect the SEARCH side.
		i++
		var search []string
		foundDivider := false
		for ; i < len(lines); i++ {
			t := strings.TrimSpace(lines[i])
			if strings.HasPrefix(t, markDivider) && t == strings.Repeat("=", len(t)) {
				foundDivider = true
				break
			}
			if strings.HasPrefix(t, markSearch) || strings.HasPrefix(t, markReplace) {
				break
			}
			search = append(search, lines[i])
		}
		if !foundDivider {
			problems = append(problems, fmt.Sprintf("block at line %d: no %s divider", start+1, markDivider))
			continue
		}

		// Collect the REPLACE side.
		i++
		var replace []string
		foundEnd := false
		for ; i < len(lines); i++ {
			t := strings.TrimSpace(lines[i])
			if strings.HasPrefix(t, markReplace) {
				foundEnd = true
				break
			}
			if strings.HasPrefix(t, markSearch) {
				break
			}
			replace = append(replace, lines[i])
		}
		if !foundEnd {
			problems = append(problems, fmt.Sprintf("block at line %d: no %s terminator", start+1, markReplace))
			continue
		}
		if path == "" {
			problems = append(problems, fmt.Sprintf("block at line %d: no file path above the %s marker", start+1, markSearch))
			continue
		}

		blocks = append(blocks, Block{
			Path:    path,
			Search:  joinBlock(search),
			Replace: joinBlock(replace),
			Line:    start + 1,
		})
	}
	return blocks, problems
}

// joinBlock rebuilds a block body. A non-empty body always ends with a
// newline so it splices cleanly into a line-oriented file.
func joinBlock(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// pathAbove walks back from a marker to find the file path, stepping over
// blank lines and code fences.
func pathAbove(lines []string, markerIdx int) string {
	for i := markerIdx - 1; i >= 0 && i >= markerIdx-4; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" || strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			continue
		}
		return cleanPath(t)
	}
	return ""
}

func cleanPath(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "`'\"")
	s = strings.TrimPrefix(s, "File: ")
	s = strings.TrimPrefix(s, "file: ")
	s = strings.TrimSuffix(s, ":")
	s = strings.TrimSpace(s)
	// A path never contains spaces in practice here; a sentence is not a path.
	if s == "" || strings.ContainsAny(s, " \t") || len(s) > 300 {
		return ""
	}
	return s
}

// ---------- matching ----------

type matchKind int

const (
	matchNone matchKind = iota
	matchExact
	matchTrailingWS
	matchIndent
)

func (m matchKind) String() string {
	switch m {
	case matchExact:
		return "exact"
	case matchTrailingWS:
		return "ignoring trailing whitespace"
	case matchIndent:
		return "after normalising indentation"
	default:
		return "no match"
	}
}

// locate finds where search occurs in fileLines, escalating through
// progressively looser strategies. Each fallback exists because it is a
// mistake models actually make: trailing whitespace they cannot see, and
// indentation they reconstruct by eye rather than by copying.
func locate(fileLines, searchLines []string) (start int, kind matchKind, count int) {
	if len(searchLines) == 0 {
		return -1, matchNone, 0
	}
	try := func(eq func(a, b string) bool) (int, int) {
		first, n := -1, 0
		for i := 0; i+len(searchLines) <= len(fileLines); i++ {
			ok := true
			for j := range searchLines {
				if !eq(fileLines[i+j], searchLines[j]) {
					ok = false
					break
				}
			}
			if ok {
				if first < 0 {
					first = i
				}
				n++
			}
		}
		return first, n
	}

	if i, n := try(func(a, b string) bool { return a == b }); i >= 0 {
		return i, matchExact, n
	}
	if i, n := try(func(a, b string) bool {
		return strings.TrimRight(a, " \t") == strings.TrimRight(b, " \t")
	}); i >= 0 {
		return i, matchTrailingWS, n
	}
	if i, n := try(func(a, b string) bool {
		return strings.TrimSpace(a) == strings.TrimSpace(b)
	}); i >= 0 {
		return i, matchIndent, n
	}
	return -1, matchNone, 0
}

// reindent shifts replacement lines by the indentation difference between what
// the model wrote and what the file actually uses, so an indent-normalised
// match does not flatten the replacement.
func reindent(replaceLines, searchLines, fileWindow []string) []string {
	modelIndent, fileIndent := "", ""
	for i, l := range searchLines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		modelIndent = leadingWS(l)
		if i < len(fileWindow) {
			fileIndent = leadingWS(fileWindow[i])
		}
		break
	}
	if modelIndent == fileIndent {
		return replaceLines
	}
	out := make([]string, len(replaceLines))
	for i, l := range replaceLines {
		if strings.TrimSpace(l) == "" {
			out[i] = l
			continue
		}
		if modelIndent != "" && strings.HasPrefix(l, modelIndent) {
			out[i] = fileIndent + strings.TrimPrefix(l, modelIndent)
		} else {
			out[i] = fileIndent + strings.TrimLeft(l, " \t")
		}
	}
	return out
}

func leadingWS(s string) string {
	return s[:len(s)-len(strings.TrimLeft(s, " \t"))]
}

// ---------- applying ----------

type BlockResult struct {
	Block   Block
	OK      bool
	Message string
	Added   int
	Removed int
	// NoOp marks a block that was valid but changed nothing — a SEARCH and
	// REPLACE that are already identical. It is separate from OK because the
	// block is not *wrong*; it simply is not progress, and a caller counting
	// turns needs to tell those apart or a model that re-sends the same
	// satisfied edit forever looks productive every time.
	NoOp bool
}

// ApplyBlocks applies each block in order and reports per-block outcomes.
// One bad block does not abort the rest: partial progress plus precise
// feedback lets the model repair just the block that failed.
func ApplyBlocks(blocks []Block, env *Env) []BlockResult {
	out := make([]BlockResult, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, applyOne(b, env))
	}
	return out
}

func applyOne(b Block, env *Env) BlockResult {
	res := BlockResult{Block: b}

	abs, err := env.WS.Resolve(b.Path)
	if err != nil {
		res.Message = err.Error()
		return res
	}

	raw, readErr := os.ReadFile(abs)
	exists := readErr == nil
	crlf := exists && strings.Contains(string(raw), "\r\n")
	old := strings.ReplaceAll(string(raw), "\r\n", "\n")

	var updated string
	switch {
	case strings.TrimSpace(b.Search) == "":
		// Empty SEARCH means "create this file".
		if exists && strings.TrimSpace(old) != "" {
			res.Message = fmt.Sprintf("%s already exists and is not empty; use a non-empty SEARCH section to modify it", b.Path)
			return res
		}
		updated = b.Replace

	case !exists:
		// A model that copied the placeholder path out of the prompt will
		// repeat the mistake for as many steps as it is given, because
		// "use an empty SEARCH section" reads as permission to create the
		// file it just invented. Naming the file it almost certainly meant
		// ends the loop in one turn.
		if near := nearestByBase(env, b.Path); near != "" {
			res.Message = fmt.Sprintf(
				"%s does not exist. Did you mean %s? Use the real path relative to the workspace root.",
				b.Path, near)
			return res
		}
		// Naming real files is what actually breaks the loop. Told only that a
		// path is missing, a model re-sends the same invented path every turn;
		// given the candidates, it picks one immediately.
		if real := someFiles(env, 10); len(real) > 0 {
			res.Message = fmt.Sprintf(
				"%s does not exist. Existing files: %s. Use one of these paths, "+
					"or an empty SEARCH section if you truly mean to create a new file.",
				b.Path, strings.Join(real, ", "))
			return res
		}
		res.Message = fmt.Sprintf("%s does not exist. To create it, use a block with an empty SEARCH section.", b.Path)
		return res

	default:
		fileLines := strings.Split(old, "\n")
		searchLines := strings.Split(strings.TrimSuffix(b.Search, "\n"), "\n")
		start, kind, count := locate(fileLines, searchLines)
		if kind == matchNone {
			res.Message = fmt.Sprintf("SEARCH text not found in %s. Read the file again and copy the exact lines.", b.Path)
			return res
		}
		if count > 1 {
			res.Message = fmt.Sprintf("SEARCH text matches %d places in %s. Add surrounding lines to make it unique.", count, b.Path)
			return res
		}
		replaceLines := strings.Split(strings.TrimSuffix(b.Replace, "\n"), "\n")
		if b.Replace == "" {
			replaceLines = nil
		}
		if kind == matchIndent {
			replaceLines = reindent(replaceLines, searchLines, fileLines[start:start+len(searchLines)])
		}
		merged := append([]string{}, fileLines[:start]...)
		merged = append(merged, replaceLines...)
		merged = append(merged, fileLines[start+len(searchLines):]...)
		updated = strings.Join(merged, "\n")
		if kind != matchExact {
			res.Message = "matched " + kind.String()
		}
	}

	if updated == old && exists {
		res.OK, res.NoOp = true, true
		res.Message = "no change — " + b.Path + " already contains exactly that text"
		return res
	}

	added, removed := diff.Summary(old, updated)
	verb := "edit"
	if !exists {
		verb = "create"
	}
	if err := env.Approver.Approve(ApprovalRequest{
		Tool:    "search_replace",
		Kind:    "edit",
		Summary: fmt.Sprintf("%s %s (+%d -%d)", verb, b.Path, added, removed),
		Detail:  diff.Unified(b.Path, old, updated, 3),
		Path:    b.Path,
		Risky:   exists,
	}); err != nil {
		res.Message = "declined: " + err.Error()
		return res
	}

	env.noteSnapshot(env.WS.Rel(abs), raw, exists)
	if err := writeBack(abs, updated, crlf); err != nil {
		res.Message = err.Error()
		return res
	}
	env.noteChange(env.WS.Rel(abs))
	res.OK, res.Added, res.Removed = true, added, removed
	if res.Message != "" {
		res.Message = fmt.Sprintf("+%d -%d (%s)", added, removed, res.Message)
	} else {
		res.Message = fmt.Sprintf("+%d -%d", added, removed)
	}
	return res
}

// writeBack restores CRLF endings if the file used them. Silently converting a
// CRLF file to LF would show up as a whole-file diff in the user's VCS.
func writeBack(abs, content string, crlf bool) error {
	if crlf {
		content = strings.ReplaceAll(content, "\n", "\r\n")
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// FormatBlockResults renders outcomes for the model, so it can see exactly
// which blocks landed and why the others did not.
func FormatBlockResults(rs []BlockResult) string {
	var sb strings.Builder
	okCount := 0
	for _, r := range rs {
		if r.OK {
			okCount++
			fmt.Fprintf(&sb, "APPLIED  %s  %s\n", r.Block.Path, r.Message)
		} else {
			fmt.Fprintf(&sb, "FAILED   %s  %s\n", r.Block.Path, r.Message)
		}
	}
	fmt.Fprintf(&sb, "\n%d of %d blocks applied.", okCount, len(rs))
	if okCount < len(rs) {
		sb.WriteString(" Fix the failed blocks and send them again; do not resend the ones that applied.")
	}
	return sb.String()
}

// ---------- edit_file tool ----------

// EditFile is the JSON-tool path to the same capability. Strong hosted models
// handle it fine; it stays available so the agent is not forced into the text
// protocol when the model is good enough not to need it.
type EditFile struct{}

func (EditFile) Spec() Spec {
	return Spec{
		Name: "edit_file",
		Description: "Replace an exact string in a file. old_string must match the file byte-for-byte " +
			"and must be unique unless replace_all is true. Prefer a SEARCH/REPLACE block in your " +
			"message for anything longer than a line or two.",
		Schema: obj(map[string]any{
			"path": str("Path relative to the workspace root."),
			"old_string": str("Exact text to find, including indentation. " +
				"Leave it empty to append new_string to the end of the file instead."),
			"new_string":  str("Text to replace it with, or to append when old_string is empty."),
			"replace_all": boolean("Replace every occurrence instead of requiring uniqueness."),
		}, "path", "new_string"),
	}
}

// nearestByBase finds the one file in the workspace whose name matches the
// base of a path that does not exist. It returns "" when there is no match or
// more than one, so the caller never suggests a guess it cannot stand behind.
func nearestByBase(env *Env, p string) string {
	base := path.Base(filepath.ToSlash(p))
	if base == "" || base == "." || base == "/" {
		return ""
	}
	found := ""
	_ = filepath.WalkDir(env.WS.Root(), func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if SkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != base {
			return nil
		}
		if found != "" {
			found = "" // ambiguous
			return filepath.SkipAll
		}
		found = env.WS.Rel(abs)
		return nil
	})
	return found
}

// someFiles lists up to n workspace-relative file paths, for grounding an
// error message in what actually exists.
func someFiles(env *Env, n int) []string {
	var out []string
	_ = filepath.WalkDir(env.WS.Root(), func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if SkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		out = append(out, env.WS.Rel(abs))
		if len(out) >= n {
			return filepath.SkipAll
		}
		return nil
	})
	return out
}

func (EditFile) Mutates() bool { return true }

func (EditFile) Run(ctx context.Context, raw json.RawMessage, env *Env) (*Result, error) {
	var a struct {
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`

		// Models reach for the SEARCH/REPLACE vocabulary they were taught
		// elsewhere in the prompt. The spelling differs; the meaning does not.
		Search  string `json:"search"`
		Replace string `json:"replace"`
	}
	if err := ParseArgs(raw, &a); err != nil {
		return Errorf("%v", err), nil
	}
	if a.OldString == "" {
		a.OldString = a.Search
	}
	if a.NewString == "" {
		a.NewString = a.Replace
	}

	if a.OldString == "" && a.NewString == "" {
		return Errorf(
			"edit_file needs old_string (the exact existing text) and new_string. "+
				"Got neither for %s. Required arguments: path, old_string, new_string.",
			a.Path), nil
	}
	if a.OldString != "" && a.OldString == a.NewString {
		return Errorf("old_string and new_string are identical; nothing to do"), nil
	}
	abs, err := env.WS.Resolve(a.Path)
	if err != nil {
		return Errorf("%v", err), nil
	}
	raw2, err := os.ReadFile(abs)
	if err != nil {
		return Errorf("read %s: %v", a.Path, err), nil
	}
	crlf := strings.Contains(string(raw2), "\r\n")
	old := strings.ReplaceAll(string(raw2), "\r\n", "\n")
	needle := strings.ReplaceAll(a.OldString, "\r\n", "\n")
	insert := strings.ReplaceAll(a.NewString, "\r\n", "\n")

	var updated string
	occurrences := 0
	switch {
	// Empty old_string with real new_string means append. Every model tries
	// this — adding a function to a file is the single most common edit, and
	// there was no other way to express it: write_file forces the whole file
	// back through JSON escaping, which small models cannot do reliably.
	// Appending cannot destroy anything, and the undo journal covers it.
	case a.OldString == "":
		sep := "\n"
		if old == "" || strings.HasSuffix(old, "\n") {
			sep = ""
		}
		updated = old + sep + insert

	default:
		n := strings.Count(old, needle)
		occurrences = n
		switch {
		case n == 0:
			return Errorf("old_string not found in %s. Read the file and copy the exact text.", a.Path), nil
		case n > 1 && !a.ReplaceAll:
			return Errorf("old_string matches %d places in %s. Add surrounding context, or set replace_all.", n, a.Path), nil
		}
		if a.ReplaceAll {
			updated = strings.ReplaceAll(old, needle, insert)
		} else {
			updated = strings.Replace(old, needle, insert, 1)
		}
	}

	added, removed := diff.Summary(old, updated)
	if err := env.Approver.Approve(ApprovalRequest{
		Tool:    "edit_file",
		Kind:    "edit",
		Summary: fmt.Sprintf("edit %s (+%d -%d)", a.Path, added, removed),
		Detail:  diff.Unified(a.Path, old, updated, 3),
		Path:    a.Path,
		Risky:   true,
	}); err != nil {
		return Errorf("declined: %v", err), nil
	}
	env.noteSnapshot(env.WS.Rel(abs), raw2, true)
	if err := writeBack(abs, updated, crlf); err != nil {
		return Errorf("%v", err), nil
	}
	env.noteChange(env.WS.Rel(abs))
	occ := ""
	if a.ReplaceAll && occurrences > 1 {
		occ = fmt.Sprintf(" (%d occurrences)", occurrences)
	}
	if a.OldString == "" {
		occ = " (appended)"
	}
	return &Result{
		Content: fmt.Sprintf("Edited %s: +%d -%d lines%s.", a.Path, added, removed, occ),
		Display: fmt.Sprintf("edit %s (+%d -%d)%s", a.Path, added, removed, occ),
	}, nil
}
