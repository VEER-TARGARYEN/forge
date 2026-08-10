package selfcheck

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/VEER-TARGARYEN/forge/internal/compact"
	"github.com/VEER-TARGARYEN/forge/internal/config"
	"github.com/VEER-TARGARYEN/forge/internal/provider"
	"github.com/VEER-TARGARYEN/forge/internal/repomap"
	"github.com/VEER-TARGARYEN/forge/internal/router"
	"github.com/VEER-TARGARYEN/forge/internal/tools"
)

// contextCases cover Phase 3: the repo map, overflow handles, compaction, and
// cross-session notes — the machinery that decides how many tokens a run costs.
func contextCases() []namedCheck {
	return []namedCheck{
		{"repomap: extracts declarations across languages", checkRepoMapExtract},
		{"repomap: ranks a depended-upon file above a leaf", checkRepoMapRank},
		{"repomap: focus biases the ranking", checkRepoMapFocus},
		{"repomap: respects the token budget", checkRepoMapBudget},
		{"repomap: skips vendored and hidden directories", checkRepoMapSkips},
		{"overflow: clip parks the remainder and hands back an id", checkOverflowClip},
		{"overflow: expand retrieves the parked lines", checkOverflowExpand},
		{"overflow: expand rejects an unknown id", checkOverflowUnknown},
		{"compact: collapses the middle, keeps system + task + tail", checkCompactShape},
		{"compact: never leaves a dangling tool result", checkCompactDangling},
		{"compact: no-op when there is nothing worth collapsing", checkCompactNoop},
		{"notes: append, dedupe, and load newest-first", checkNotes},
	}
}

// ---------- repo map ----------

func checkRepoMapExtract() (string, error) {
	ws, cleanup, err := tempWS(map[string]string{
		"a.go": "package a\n\ntype Server struct{}\n\nfunc NewServer() *Server { return nil }\n\nfunc (s *Server) Serve(addr string) error { return nil }\n",
		"b.py": "class Handler:\n    def handle(self, req):\n        pass\n",
		"c.ts": "export interface Config { a: number }\nexport function build(c: Config) {}\n",
	})
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}

	m, err := repomap.Build(ws.Root(), repomap.Options{})
	if err != nil {
		return "", failf("build: %v", err)
	}
	if m.Scanned != 3 {
		return "", failf("scanned %d files, want 3", m.Scanned)
	}
	got := map[string][]string{}
	for _, f := range m.Files {
		for _, d := range f.Defs {
			got[f.Path] = append(got[f.Path], d.Name)
		}
	}
	want := map[string][]string{
		"a.go": {"Server", "NewServer", "Serve"},
		"b.py": {"Handler", "handle"},
		"c.ts": {"Config", "build"},
	}
	for path, names := range want {
		for _, n := range names {
			if !contains(got[path], n) {
				return "", failf("%s: missing declaration %q (got %v)", path, n, got[path])
			}
		}
	}
	return "go, python, typescript", nil
}

func checkRepoMapRank() (string, error) {
	// core is referenced by three files; leaf is referenced by none. PageRank
	// must put core first regardless of file size or name.
	ws, cleanup, err := tempWS(map[string]string{
		"core.go":  "package core\n\ntype Widget struct{}\n\nfunc Assemble() *Widget { return nil }\n",
		"one.go":   "package one\n\nfunc A() { Assemble(); Assemble() }\n",
		"two.go":   "package two\n\nfunc B() { w := Assemble(); _ = w }\n",
		"three.go": "package three\n\nfunc C() *Widget { return Assemble() }\n",
		"leaf.go":  "package leaf\n\nfunc Lonely() int { return 1 }\n",
	})
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}

	m, err := repomap.Build(ws.Root(), repomap.Options{})
	if err != nil {
		return "", failf("build: %v", err)
	}
	ranked := m.RankedFiles()
	if len(ranked) == 0 {
		return "", failf("no files ranked")
	}
	if ranked[0] != "core.go" {
		return "", failf("top file = %q, want core.go (ranking: %v)", ranked[0], ranked)
	}
	if m.Rank("core.go") <= m.Rank("leaf.go") {
		return "", failf("core rank %.5f not above leaf rank %.5f", m.Rank("core.go"), m.Rank("leaf.go"))
	}
	return fmt.Sprintf("core %.4f > leaf %.4f", m.Rank("core.go"), m.Rank("leaf.go")), nil
}

func checkRepoMapFocus() (string, error) {
	ws, cleanup, err := tempWS(map[string]string{
		"core.go": "package core\n\nfunc Assemble() int { return 1 }\n",
		"one.go":  "package one\n\nfunc A() { Assemble() }\n",
		"two.go":  "package two\n\nfunc B() { Assemble() }\n",
		"leaf.go": "package leaf\n\nfunc Lonely() int { return 1 }\n",
	})
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}

	plain, err := repomap.Build(ws.Root(), repomap.Options{})
	if err != nil {
		return "", failf("build: %v", err)
	}
	focused, err := repomap.Build(ws.Root(), repomap.Options{Focus: []string{"leaf.go"}})
	if err != nil {
		return "", failf("build focused: %v", err)
	}
	// Focus should lift the named file's share of the rank mass.
	if focused.Rank("leaf.go") <= plain.Rank("leaf.go") {
		return "", failf("focus did not raise leaf.go: %.5f -> %.5f",
			plain.Rank("leaf.go"), focused.Rank("leaf.go"))
	}
	return fmt.Sprintf("leaf %.4f -> %.4f", plain.Rank("leaf.go"), focused.Rank("leaf.go")), nil
}

func checkRepoMapBudget() (string, error) {
	files := map[string]string{}
	for i := 0; i < 40; i++ {
		var sb strings.Builder
		fmt.Fprintf(&sb, "package p%d\n\n", i)
		for j := 0; j < 20; j++ {
			fmt.Fprintf(&sb, "func Function%d_%d(argument string, another int) error { return nil }\n", i, j)
		}
		files[fmt.Sprintf("pkg%d/file.go", i)] = sb.String()
	}
	ws, cleanup, err := tempWS(files)
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}

	m, err := repomap.Build(ws.Root(), repomap.Options{})
	if err != nil {
		return "", failf("build: %v", err)
	}
	for _, budget := range []int{200, 500, 1500} {
		out := m.Render(budget)
		est := len(out) * 10 / 36
		if est > budget {
			return "", failf("budget %d produced ~%d tokens", budget, est)
		}
		if est == 0 {
			return "", failf("budget %d produced nothing", budget)
		}
		if !strings.Contains(out, "more files not shown") {
			return "", failf("budget %d did not report omitted files", budget)
		}
	}
	// A budget of zero must disable the map entirely, not emit a header.
	if m.Render(0) != "" {
		return "", failf("zero budget still produced output")
	}
	return "200/500/1500 all within budget", nil
}

func checkRepoMapSkips() (string, error) {
	ws, cleanup, err := tempWS(map[string]string{
		"real.go":                    "package real\n\nfunc Real() {}\n",
		"node_modules/dep/index.js":  "function Vendored() {}\n",
		"vendor/lib/lib.go":          "package lib\n\nfunc Vendored() {}\n",
		".git/hooks/x.go":            "package x\n\nfunc Hooked() {}\n",
		"internal/deep/nested/ok.go": "package nested\n\nfunc Nested() {}\n",
	})
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}

	m, err := repomap.Build(ws.Root(), repomap.Options{})
	if err != nil {
		return "", failf("build: %v", err)
	}
	for _, f := range m.Files {
		if strings.Contains(f.Path, "node_modules") || strings.Contains(f.Path, "vendor") ||
			strings.Contains(f.Path, ".git") {
			return "", failf("scanned a skipped directory: %s", f.Path)
		}
	}
	if m.Scanned != 2 {
		return "", failf("scanned %d files, want 2 (real.go and the nested one)", m.Scanned)
	}
	return "vendor/node_modules/.git excluded, nested kept", nil
}

// ---------- overflow ----------

func checkOverflowClip() (string, error) {
	ws, cleanup, err := tempWS(nil)
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}
	env, _ := envFor(ws, true)
	env.MaxBytes = 500
	env.Overflow = tools.NewOverflow("")

	var sb strings.Builder
	for i := 1; i <= 400; i++ {
		fmt.Fprintf(&sb, "line %d of the result\n", i)
	}
	full := sb.String()

	shown, note := env.Clip("test", full)
	if len(shown) > 500 {
		return "", failf("clip returned %d bytes, over the %d cap", len(shown), 500)
	}
	if !strings.HasSuffix(shown, "\n") {
		return "", failf("clip cut mid-line: %q", shown[len(shown)-40:])
	}
	if note == "" {
		return "", failf("no truncation notice emitted")
	}
	// The notice must carry a usable handle, not just say "truncated".
	if !strings.Contains(note, "expand(id=") || !strings.Contains(note, "of 400") {
		return "", failf("notice lacks a retrieval handle: %q", note)
	}
	// Content that fits must pass through untouched, with no notice.
	small, note2 := env.Clip("test", "short\n")
	if small != "short\n" || note2 != "" {
		return "", failf("small content was altered: %q / %q", small, note2)
	}
	return "clipped at a line boundary with a handle", nil
}

func checkOverflowExpand() (string, error) {
	ws, cleanup, err := tempWS(nil)
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}
	env, _ := envFor(ws, true)
	env.MaxBytes = 300
	env.Overflow = tools.NewOverflow("")

	var sb strings.Builder
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&sb, "row %d\n", i)
	}
	_, note := env.Clip("test", sb.String())

	// Pull the id out of the notice exactly as a model would.
	start := strings.Index(note, `id="`)
	if start < 0 {
		return "", failf("no id in notice: %q", note)
	}
	rest := note[start+4:]
	id := rest[:strings.IndexByte(rest, '"')]

	res, err := runTool(tools.Expand{}, map[string]any{"id": id, "offset": 150, "limit": 10}, env)
	if err != nil {
		return "", failf("expand: %v", err)
	}
	if res.IsError {
		return "", failf("expand errored: %s", res.Content)
	}
	if !strings.Contains(res.Content, "row 150") || !strings.Contains(res.Content, "row 159") {
		return "", failf("expand returned the wrong window:\n%s", res.Content)
	}
	if strings.Contains(res.Content, "row 160") {
		return "", failf("expand overran its limit:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "offset=160") {
		return "", failf("expand did not hint the next offset:\n%s", res.Content)
	}
	return "retrieved lines 150-159 of 200", nil
}

func checkOverflowUnknown() (string, error) {
	ws, cleanup, err := tempWS(nil)
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}
	env, _ := envFor(ws, true)
	env.Overflow = tools.NewOverflow("")

	res, err := runTool(tools.Expand{}, map[string]any{"id": "r999"}, env)
	if err != nil {
		return "", failf("expand: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "r999") {
		return "", failf("unknown id did not produce a clear error: %+v", res)
	}
	return "clear error", nil
}

// ---------- compaction ----------

// summarizerRouter wires a router whose only target is a stub that answers
// any summarisation request.
func summarizerRouter() (*router.Router, func(), error) {
	srv := stubServer(200, "", "", nil)
	rt, _, cleanup, err := testRouter(
		[]config.Target{{Provider: "s", Model: "m"}},
		[]config.Provider{{Name: "s", BaseURL: srv.URL + "/v1", APIKey: "k"}})
	return rt, func() { srv.Close(); cleanup() }, err
}

func buildConversation(middle int) []provider.Message {
	msgs := []provider.Message{
		{Role: provider.RoleSystem, Content: "SYSTEM PROMPT"},
		{Role: provider.RoleUser, Content: "THE ORIGINAL TASK"},
	}
	for i := 0; i < middle; i++ {
		msgs = append(msgs,
			provider.Message{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{
				ID: fmt.Sprintf("call%d", i), Type: "function",
				Function: provider.FunctionCall{Name: "read_file", Arguments: fmt.Sprintf(`{"path":"f%d.go"}`, i)},
			}}},
			provider.Message{Role: provider.RoleTool, ToolCallID: fmt.Sprintf("call%d", i),
				Name: "read_file", Content: fmt.Sprintf("contents of f%d.go", i)},
		)
	}
	msgs = append(msgs, provider.Message{Role: provider.RoleAssistant, Content: "MOST RECENT REPLY"})
	return msgs
}

// validatePairing checks the invariant every provider enforces: a tool result
// must follow an assistant message that requested it.
func validatePairing(msgs []provider.Message) error {
	open := map[string]bool{}
	for i, m := range msgs {
		switch m.Role {
		case provider.RoleAssistant:
			for _, tc := range m.ToolCalls {
				open[tc.ID] = true
			}
		case provider.RoleTool:
			if !open[m.ToolCallID] {
				return fmt.Errorf("message %d is a tool result for %q with no preceding tool_calls", i, m.ToolCallID)
			}
		}
	}
	return nil
}

func checkCompactShape() (string, error) {
	rt, cleanup, err := summarizerRouter()
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}

	msgs := buildConversation(15)
	res, err := compact.Run(context.Background(), rt, msgs, compact.Options{
		Class: "coder", KeepTail: 4,
	})
	if err != nil {
		return "", failf("compact: %v", err)
	}
	if res.Collapsed == 0 {
		return "", failf("nothing was collapsed from a %d-message conversation", len(msgs))
	}
	if len(res.Messages) >= len(msgs) {
		return "", failf("compaction did not shrink the conversation: %d -> %d", len(msgs), len(res.Messages))
	}
	// The two things that must never be lost.
	if res.Messages[0].Role != provider.RoleSystem || res.Messages[0].Content != "SYSTEM PROMPT" {
		return "", failf("system prompt was altered: %+v", res.Messages[0])
	}
	if res.Messages[1].Content != "THE ORIGINAL TASK" {
		return "", failf("original task was lost; message 1 = %q", res.Messages[1].Content)
	}
	last := res.Messages[len(res.Messages)-1]
	if last.Content != "MOST RECENT REPLY" {
		return "", failf("most recent message was lost; got %q", last.Content)
	}
	if err := validatePairing(res.Messages); err != nil {
		return "", failf("%v", err)
	}
	if res.AfterTokens >= res.BeforeTokens {
		return "", failf("tokens did not drop: %d -> %d", res.BeforeTokens, res.AfterTokens)
	}
	return fmt.Sprintf("%d msgs collapsed, ~%d → ~%d tok", res.Collapsed, res.BeforeTokens, res.AfterTokens), nil
}

func checkCompactDangling() (string, error) {
	rt, cleanup, err := summarizerRouter()
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}

	// KeepTail values that land the boundary squarely on a tool message are
	// the dangerous ones: keeping a tool result whose assistant tool_calls
	// message was collapsed makes the provider reject the whole request.
	for _, keep := range []int{1, 2, 3, 4, 5, 6, 7} {
		msgs := buildConversation(12)
		res, err := compact.Run(context.Background(), rt, msgs, compact.Options{
			Class: "coder", KeepTail: keep,
		})
		if err != nil {
			return "", failf("keepTail=%d: %v", keep, err)
		}
		if err := validatePairing(res.Messages); err != nil {
			return "", failf("keepTail=%d: %v", keep, err)
		}
	}
	return "7 tail boundaries, no dangling results", nil
}

func checkCompactNoop() (string, error) {
	rt, cleanup, err := summarizerRouter()
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}

	msgs := buildConversation(1) // system + task + 2 middle + 1 tail
	res, err := compact.Run(context.Background(), rt, msgs, compact.Options{
		Class: "coder", KeepTail: 6, MinCollapse: 4,
	})
	if err != nil {
		return "", failf("compact: %v", err)
	}
	if res.Collapsed != 0 {
		return "", failf("collapsed %d messages from a short conversation", res.Collapsed)
	}
	if len(res.Messages) != len(msgs) {
		return "", failf("no-op changed the conversation: %d -> %d", len(msgs), len(res.Messages))
	}
	return "short conversation left alone", nil
}

// ---------- notes ----------

func checkNotes() (string, error) {
	dir, err := os.MkdirTemp("", "forge-notes-*")
	if err != nil {
		return "", failf("tempdir: %v", err)
	}
	defer os.RemoveAll(dir)

	ws, cleanup, err := tempWS(nil)
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}

	path := tools.NotesPath(dir, ws.Root())
	if path == "" {
		return "", failf("NotesPath returned empty")
	}
	// Notes must live in forge's state dir, never inside the user's repo.
	if strings.HasPrefix(path, ws.Root()) {
		return "", failf("notes path %q is inside the workspace", path)
	}

	env, _ := envFor(ws, true)
	env.NotesFile = path

	if _, err := runTool(tools.Remember{}, map[string]any{"note": "build with: go build ./..."}, env); err != nil {
		return "", failf("remember: %v", err)
	}
	if _, err := runTool(tools.Remember{}, map[string]any{"note": "tests need CGO_ENABLED=0"}, env); err != nil {
		return "", failf("remember: %v", err)
	}
	// The same fact twice would slowly crowd out the system prompt.
	res, err := runTool(tools.Remember{}, map[string]any{"note": "build with: go build ./..."}, env)
	if err != nil {
		return "", failf("remember: %v", err)
	}
	if !strings.Contains(res.Content, "Already") {
		return "", failf("duplicate note was not detected: %q", res.Content)
	}

	loaded := tools.LoadNotes(path, 4000)
	if !strings.Contains(loaded, "go build ./...") || !strings.Contains(loaded, "CGO_ENABLED=0") {
		return "", failf("notes did not round-trip: %q", loaded)
	}
	if strings.Count(loaded, "go build ./...") != 1 {
		return "", failf("duplicate note was written anyway: %q", loaded)
	}
	// Over budget, the newest notes are the ones that survive.
	trimmedNotes := tools.LoadNotes(path, 40)
	if len(trimmedNotes) > 80 {
		return "", failf("budget ignored: %d chars", len(trimmedNotes))
	}
	if !strings.Contains(trimmedNotes, "CGO_ENABLED=0") {
		return "", failf("trimming dropped the newest note: %q", trimmedNotes)
	}
	return "appended, deduped, trimmed newest-first", nil
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
