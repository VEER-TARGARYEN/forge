// Package fsx holds the filesystem conventions shared by the tools and the
// repo map: which directories are never worth walking, and what counts as a
// binary file.
//
// It exists as its own package so repomap can reuse them without importing
// tools, which would create a cycle once a repo_map tool is added.
package fsx

import (
	"io/fs"
	"path/filepath"
	"strings"
)

// SkipDirs are directories never worth walking: build output, dependency
// caches, and VCS internals. Walking node_modules once will fill a context
// window with nothing useful.
var SkipDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true,
	"node_modules": true, "vendor": true, "bower_components": true,
	"dist": true, "build": true, "out": true, "target": true,
	".next": true, ".nuxt": true, ".svelte-kit": true, ".turbo": true,
	"__pycache__": true, ".venv": true, "venv": true, ".mypy_cache": true,
	".pytest_cache": true, ".tox": true, ".gradle": true, ".idea": true,
	".vscode": true, ".cache": true, "coverage": true, ".gotmp": true,
	"obj": true, "Debug": true, "Release": true, ".forge": true,
}

// SkipDir reports whether a directory should be skipped during a walk.
// Hidden directories are skipped too, with .github excepted because workflow
// files are often exactly what you are looking for.
func SkipDir(name string) bool {
	if SkipDirs[name] {
		return true
	}
	return strings.HasPrefix(name, ".") && name != ".github"
}

// IsBinary reports whether a byte slice looks like binary content. A NUL in
// the first block is the same heuristic grep uses, and it is right often
// enough to keep object files out of the model's context.
func IsBinary(b []byte) bool {
	n := len(b)
	if n > 8000 {
		n = 8000
	}
	for i := 0; i < n; i++ {
		if b[i] == 0 {
			return true
		}
	}
	return false
}

// WalkFiles visits every file under root, skipping the directories above.
// Unreadable directories are stepped over rather than aborting the walk: a
// permission error deep in a tree should not fail the whole search.
func WalkFiles(root string, fn func(abs string, d fs.DirEntry) error) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p != root && SkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		return fn(p, d)
	})
}
