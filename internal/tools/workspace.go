package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/VEER-TARGARYEN/forge/internal/fsx"
)

// Workspace confines every file operation to a single root directory.
//
// This is the agent's blast radius. A model that hallucinates "../../.ssh/id_rsa"
// must fail at the path layer, not at the approval prompt — an approver can be
// set to auto-approve, but the root check never can be.
type Workspace struct {
	root string
}

func NewWorkspace(root string) (*Workspace, error) {
	if root == "" {
		root = "."
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("workspace %s: %w", abs, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace %s is not a directory", abs)
	}
	// Resolve the root's own symlinks once, so comparisons downstream are
	// against a canonical path.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return &Workspace{root: filepath.Clean(abs)}, nil
}

func (w *Workspace) Root() string { return w.root }

// Resolve maps a caller-supplied path to an absolute path inside the
// workspace, or fails. It resolves symlinks on whatever prefix of the path
// already exists, which is what catches a symlink planted mid-path pointing
// outside the root.
func (w *Workspace) Resolve(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("empty path")
	}
	// Accept both separators regardless of host OS; models emit forward
	// slashes even on Windows.
	p = strings.ReplaceAll(p, "\\", string(filepath.Separator))
	p = strings.ReplaceAll(p, "/", string(filepath.Separator))

	var abs string
	if filepath.IsAbs(p) {
		abs = filepath.Clean(p)
	} else {
		abs = filepath.Clean(filepath.Join(w.root, p))
	}

	real := w.resolveExistingPrefix(abs)
	if !w.within(real) {
		return "", fmt.Errorf("path %q is outside the workspace (%s)", p, w.root)
	}
	return abs, nil
}

// resolveExistingPrefix walks up until it finds a path that exists, resolves
// its symlinks, then re-appends the non-existent tail. Without this, creating
// a new file under a symlinked directory would escape the root undetected.
func (w *Workspace) resolveExistingPrefix(abs string) string {
	tail := ""
	cur := abs
	for i := 0; i < 64; i++ {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			if tail == "" {
				return resolved
			}
			return filepath.Join(resolved, tail)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs
		}
		tail = filepath.Join(filepath.Base(cur), tail)
		cur = parent
	}
	return abs
}

func (w *Workspace) within(p string) bool {
	if pathEqual(p, w.root) {
		return true
	}
	prefix := w.root
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	if runtime.GOOS == "windows" {
		// NTFS is case-insensitive; a case-flipped prefix is the same directory.
		return strings.HasPrefix(strings.ToLower(p), strings.ToLower(prefix))
	}
	return strings.HasPrefix(p, prefix)
}

func pathEqual(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// Rel renders an absolute path relative to the root, for display. Falls back
// to the absolute path if it somehow sits outside.
func (w *Workspace) Rel(abs string) string {
	rel, err := filepath.Rel(w.root, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return abs
	}
	return filepath.ToSlash(rel)
}

// The walk conventions (which directories to skip, what counts as binary)
// live in fsx so repomap can share them without importing this package.
var (
	SkipDirs = fsx.SkipDirs
	IsBinary = fsx.IsBinary
)
