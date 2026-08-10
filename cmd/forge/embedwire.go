package main

import (
	"fmt"
	"io"

	"github.com/VEER-TARGARYEN/forge/internal/embed"
)

// resolveEmbedder decides which embedding model to use, if any.
//
// Precedence: an explicit -embed-model flag, then a model discovered under the
// state directory. A locally loaded model always beats the router — no
// network, no key, no quota — so discovery is checked before falling through
// to a provider.
//
// Returns nil with no error when nothing is available: semantic search is an
// upgrade, not a requirement, and the caller degrades to keyword-only.
func resolveEmbedder(flagPath, stateDir string, out io.Writer) (*embed.Embedder, error) {
	path := flagPath
	discovered := false
	if path == "" {
		var ok bool
		path, ok = embed.Discover(stateDir)
		if !ok {
			return nil, nil
		}
		discovered = true
	}
	if err := embed.LooksLikeModelDir(path); err != nil {
		if discovered {
			// A half-downloaded directory should not break the run.
			return nil, nil
		}
		return nil, fmt.Errorf("%v\n\n%s", err, embed.ModelHint)
	}

	em, err := embed.Load(path, embed.Options{})
	if err != nil {
		return nil, fmt.Errorf("load embedding model: %w", err)
	}
	if out != nil {
		how := "configured"
		if discovered {
			how = "auto-detected"
		}
		fmt.Fprintf(out, "embedder:  %s, %s\n", how, em.Describe())
	}
	return em, nil
}
