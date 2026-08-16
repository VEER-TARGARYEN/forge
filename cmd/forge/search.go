package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/VEER-TARGARYEN/forge/internal/embed"
	"github.com/VEER-TARGARYEN/forge/internal/index"
	"github.com/VEER-TARGARYEN/forge/internal/router"
	"github.com/VEER-TARGARYEN/forge/internal/tools"
)

// lazyIndex defers building the search index until the agent actually calls
// search_code.
//
// Building costs a walk, a chunk pass, and a tokenise pass over the repository.
// Most tasks never search — they grep, or the repo map was enough — so paying
// that on every run would be a tax on the common case.
type lazyIndex struct {
	once sync.Once
	ix   *index.Index
	err  error

	root       string
	dir        string
	rt         *router.Router
	embedClass string
	// local, when set, embeds in-process instead of calling a provider.
	local *embed.Embedder
	opts  index.Options
	// out is an io.Writer rather than *os.File so a non-terminal caller — the
	// browser interface — can discard progress chatter instead of leaking it
	// onto the server's stderr.
	out io.Writer
}

func newLazyIndex(root, dir string, rt *router.Router, embedClass string, opts index.Options) *lazyIndex {
	return &lazyIndex{root: root, dir: dir, rt: rt, embedClass: embedClass, opts: opts, out: os.Stderr}
}

func (l *lazyIndex) useLocal(e *embed.Embedder) { l.local = e }

func (l *lazyIndex) get() (*index.Index, error) {
	l.once.Do(func() {
		l.ix, l.err = index.OpenOrBuild(l.dir, l.root, l.opts)
		if l.err == nil {
			s := l.ix.Stats()
			mode := "keyword only"
			if l.ix.HasVectors() {
				mode = fmt.Sprintf("hybrid, %d-dim vectors via %s", s.Dim, s.EmbedModel)
			}
			fmt.Fprintf(l.out, "  ⋯ index: %d chunks over %d files (%s)\n", s.Chunks, s.Files, mode)
		}
	})
	return l.ix, l.err
}

// embedFn returns an embedding callback, or nil when nothing can embed. Nil is
// the signal to run keyword-only rather than to fail.
//
// A locally loaded model takes precedence over the router: it needs no network,
// no key, and no quota, so if one is present it is strictly the better choice.
func (l *lazyIndex) embedFn() index.EmbedFunc {
	if l.local != nil {
		return l.local.Embed
	}
	if l.rt == nil || !l.rt.CanEmbed(l.embedClass) {
		return nil
	}
	return func(ctx context.Context, texts []string) ([][]float32, error) {
		resp, err := l.rt.Embed(ctx, l.embedClass, texts)
		if err != nil {
			return nil, err
		}
		return resp.Vectors, nil
	}
}

// search adapts the index to the tool-facing signature.
func (l *lazyIndex) search(ctx context.Context, query string, limit int, mode string) ([]tools.SearchHit, string, error) {
	ix, err := l.get()
	if err != nil {
		return nil, "", err
	}
	m := index.Mode(mode)
	switch m {
	case index.ModeKeyword, index.ModeSemantic, index.ModeHybrid:
	default:
		m = index.ModeHybrid
	}
	embed := l.embedFn()
	// Degrade rather than fail: an index with no vectors, or no configured
	// embedding backend, still answers keyword queries perfectly well.
	if (m == index.ModeHybrid || m == index.ModeSemantic) && (embed == nil || !ix.HasVectors()) {
		m = index.ModeKeyword
	}

	results, err := ix.Search(ctx, query, limit, m, embed)
	if err != nil {
		return nil, string(m), err
	}
	hits := make([]tools.SearchHit, 0, len(results))
	for _, r := range results {
		hits = append(hits, tools.SearchHit{
			File: r.Chunk.File, Start: r.Chunk.Start, End: r.Chunk.End,
			Symbol: r.Chunk.Symbol, Kind: r.Chunk.Kind,
			Score: r.Score, Text: r.Text,
		})
	}
	return hits, string(m), nil
}
