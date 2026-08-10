// Package checkpoint records the original contents of every file the agent
// modifies, so a run can be undone.
//
// This is a file-level undo journal rather than automatic git commits. Writing
// commits into someone's repository is a side effect they did not ask for: it
// pollutes their history and their reflog, it interacts badly with an
// in-progress rebase or a dirty index, and it does not work at all outside a
// git repo. The journal has none of those problems, is exact about which files
// were touched, and costs nothing to capture — the editing tools already read
// the original bytes to build their diff.
package checkpoint

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

type entry struct {
	Content []byte
	Existed bool
}

type Journal struct {
	mu   sync.Mutex
	root string
	orig map[string]entry
}

func New(root string) *Journal {
	return &Journal{root: root, orig: map[string]entry{}}
}

// Record captures a file's pre-modification state. Only the first call for a
// given path takes effect: reverting must restore the state at the start of
// the run, not the state before the most recent of several edits.
func (j *Journal) Record(rel string, original []byte, existed bool) {
	if j == nil || rel == "" {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, seen := j.orig[rel]; seen {
		return
	}
	cp := make([]byte, len(original))
	copy(cp, original)
	j.orig[rel] = entry{Content: cp, Existed: existed}
}

// Touched lists every path recorded, in stable order.
func (j *Journal) Touched() []string {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]string, 0, len(j.orig))
	for p := range j.orig {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func (j *Journal) Len() int {
	if j == nil {
		return 0
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.orig)
}

// Revert restores every recorded file to its original state, deleting the ones
// that did not exist before. It returns the paths restored.
//
// It keeps going past individual failures and reports them together: a partial
// revert that stops at the first read-only file would leave the tree in a
// worse state than either endpoint.
func (j *Journal) Revert() ([]string, error) {
	if j == nil {
		return nil, nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()

	var restored []string
	var errs []string
	for _, rel := range sortedKeys(j.orig) {
		e := j.orig[rel]
		abs := filepath.Join(j.root, filepath.FromSlash(rel))
		if !e.Existed {
			if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
				errs = append(errs, fmt.Sprintf("%s: %v", rel, err))
				continue
			}
			restored = append(restored, rel)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", rel, err))
			continue
		}
		if err := os.WriteFile(abs, e.Content, 0o644); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", rel, err))
			continue
		}
		restored = append(restored, rel)
	}
	if len(errs) > 0 {
		return restored, fmt.Errorf("restored %d file(s); %d failed: %v", len(restored), len(errs), errs)
	}
	return restored, nil
}

// Save writes the originals to disk so a run can be undone after the process
// exits, and so a bad edit is recoverable even if the agent crashed.
func (j *Journal) Save(dir string) error {
	if j == nil || len(j.orig) == 0 {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	manifest := map[string]any{"root": j.root, "files": map[string]bool{}}
	files := manifest["files"].(map[string]bool)

	for i, rel := range sortedKeys(j.orig) {
		e := j.orig[rel]
		files[rel] = e.Existed
		if !e.Existed {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%03d.orig", i)), e.Content, 0o600); err != nil {
			return err
		}
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0o600)
}

func sortedKeys(m map[string]entry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
