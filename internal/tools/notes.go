package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Notes are durable facts carried between sessions for one workspace.
//
// They live in forge's own state directory rather than the repository. The
// agent writing a NOTES.md into someone's project is a side effect they did
// not ask for, and it would show up in their next git status.
//
// This is the cheapest possible long-term memory: a handful of lines loaded
// into the system prompt, so the next run starts knowing what the last one
// worked out instead of rediscovering it.

// NotesPath returns the notes file for a workspace inside the state dir.
func NotesPath(stateDir, wsRoot string) string {
	if stateDir == "" {
		return ""
	}
	h := uint64(1469598103934665603)
	for _, c := range strings.ToLower(filepath.Clean(wsRoot)) {
		h ^= uint64(c)
		h *= 1099511628211
	}
	return filepath.Join(stateDir, "notes", fmt.Sprintf("%x.md", h))
}

// LoadNotes reads the notes file, keeping the most recent entries when it
// exceeds the budget. Recency wins because notes are append-only and the
// latest state of a project supersedes earlier observations.
func LoadNotes(path string, maxChars int) string {
	if path == "" || maxChars <= 0 {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(raw))
	if len(s) <= maxChars {
		return s
	}
	s = s[len(s)-maxChars:]
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return "[older notes omitted]\n" + s
}

type Remember struct{}

func (Remember) Spec() Spec {
	return Spec{
		Name: "remember",
		Description: "Save a durable fact about this project for future sessions — a build command " +
			"that works, a non-obvious constraint, where something lives. Do not save what the code " +
			"already says, or anything only relevant to the current task.",
		Schema: obj(map[string]any{
			"note": str("One fact, in a single sentence."),
		}, "note"),
	}
}

func (Remember) Mutates() bool { return false }

func (Remember) Run(ctx context.Context, raw json.RawMessage, env *Env) (*Result, error) {
	var a struct {
		Note string `json:"note"`
	}
	if err := ParseArgs(raw, &a); err != nil {
		return Errorf("%v", err), nil
	}
	a.Note = strings.TrimSpace(strings.ReplaceAll(a.Note, "\n", " "))
	if a.Note == "" {
		return Errorf("note is required"), nil
	}
	if env.NotesFile == "" {
		return Errorf("notes are not enabled for this run"), nil
	}
	if err := os.MkdirAll(filepath.Dir(env.NotesFile), 0o755); err != nil {
		return Errorf("notes dir: %v", err), nil
	}

	// Appending the same fact every run would slowly crowd out the system
	// prompt, so an existing note is left alone.
	if existing, err := os.ReadFile(env.NotesFile); err == nil {
		if strings.Contains(string(existing), a.Note) {
			return Textf("Already noted."), nil
		}
	}

	f, err := os.OpenFile(env.NotesFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return Errorf("open notes: %v", err), nil
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "- %s\n", a.Note); err != nil {
		return Errorf("write note: %v", err), nil
	}
	return &Result{Content: "Noted.", Display: "remember: " + a.Note}, nil
}
