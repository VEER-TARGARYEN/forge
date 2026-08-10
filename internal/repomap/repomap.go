// Package repomap builds a ranked, token-budgeted summary of a repository.
//
// This is the single biggest context saver in the agent. Without it, a model
// orients itself by reading files, which costs thousands of tokens and several
// minutes of CPU prefill before any work starts. A repo map puts the shape of
// the whole codebase — every file that matters and what it declares — into
// roughly a thousand tokens, computed once per run.
//
// The ranking is PageRank over a file-to-file reference graph, so "important"
// means "much of the rest of the code depends on this", not "large" or
// "recently edited". That is the property that makes a short map useful.
package repomap

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/VEER-TARGARYEN/forge/internal/fsx"
)

type Options struct {
	// Focus files get a personalisation boost, so the map leans toward the
	// area being worked on rather than the global centre of the codebase.
	Focus []string
	// MaxFileBytes skips generated or vendored blobs.
	MaxFileBytes int64
	// CacheDir persists extraction results between runs. Empty disables it.
	CacheDir string
}

type Map struct {
	Root    string
	Files   []*FileSyms
	rank    map[string]float64
	symRank map[string]map[string]float64
	Scanned int
	Skipped int
}

// Build scans the tree, extracts declarations and references, and ranks files.
func Build(root string, opts Options) (*Map, error) {
	if opts.MaxFileBytes <= 0 {
		opts.MaxFileBytes = 1 << 20
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}

	cache := loadCache(opts.CacheDir, abs)
	m := &Map{Root: abs}

	type job struct {
		path string
		rel  string
		size int64
		mod  int64
	}
	var jobs []job
	err = fsx.WalkFiles(abs, func(p string, d fs.DirEntry) error {
		ext := strings.ToLower(filepath.Ext(p))
		if !Supported(ext) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Size() > opts.MaxFileBytes {
			m.Skipped++
			return nil
		}
		rel, err := filepath.Rel(abs, p)
		if err != nil {
			return nil
		}
		jobs = append(jobs, job{p, filepath.ToSlash(rel), info.Size(), info.ModTime().UnixNano()})
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Extraction is CPU-bound regex work over many small files; a worker pool
	// is a straightforward win even on four cores.
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		results = make([]*FileSyms, len(jobs))
		fresh   = map[string]cacheEntry{}
	)
	sem := make(chan struct{}, 8)
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			key := j.rel
			if e, ok := cache[key]; ok && e.Size == j.size && e.Mod == j.mod {
				results[i] = e.Syms
				mu.Lock()
				fresh[key] = e
				mu.Unlock()
				return
			}
			data, err := os.ReadFile(j.path)
			if err != nil || fsx.IsBinary(data) {
				return
			}
			syms := Extract(j.rel, filepath.Ext(j.path), string(data))
			results[i] = syms
			mu.Lock()
			fresh[key] = cacheEntry{Size: j.size, Mod: j.mod, Syms: syms}
			mu.Unlock()
		}(i, j)
	}
	wg.Wait()

	for _, r := range results {
		if r != nil {
			m.Files = append(m.Files, r)
		}
	}
	m.Scanned = len(m.Files)
	saveCache(opts.CacheDir, abs, fresh)

	m.rankFiles(opts.Focus)
	return m, nil
}

// rankFiles runs PageRank over the graph "file A references a symbol that file
// B declares", then distributes each file's rank across its own declarations
// in proportion to how often they are referenced elsewhere.
func (m *Map) rankFiles(focus []string) {
	m.rank = map[string]float64{}
	m.symRank = map[string]map[string]float64{}
	if len(m.Files) == 0 {
		return
	}

	definedIn := map[string][]string{} // symbol -> files declaring it
	for _, f := range m.Files {
		for _, d := range f.Defs {
			definedIn[d.Name] = append(definedIn[d.Name], f.Path)
		}
	}

	idx := map[string]int{}
	for i, f := range m.Files {
		idx[f.Path] = i
	}
	n := len(m.Files)

	// out[i] holds weighted edges i -> j, meaning file i depends on file j.
	out := make([]map[int]float64, n)
	for i := range out {
		out[i] = map[int]float64{}
	}
	// symWeight[file][symbol] accumulates external interest in a declaration.
	symWeight := make([]map[string]float64, n)
	for i := range symWeight {
		symWeight[i] = map[string]float64{}
	}

	for i, f := range m.Files {
		for name, count := range f.Refs {
			defs := definedIn[name]
			if len(defs) == 0 || len(defs) > 6 {
				// Declared nowhere, or everywhere (a common method name like
				// String or Run). Neither carries dependency information.
				continue
			}
			// Diminishing returns: 100 mentions is not 100x the signal of one.
			w := math.Sqrt(float64(count)) / float64(len(defs))
			for _, dp := range defs {
				j := idx[dp]
				if i == j {
					continue
				}
				out[i][j] += w
				symWeight[j][name] += w
			}
		}
	}

	// Personalisation: focus files, and their immediate neighbourhood via the
	// normal PageRank flow, pull the map toward what is being worked on.
	pers := make([]float64, n)
	focusSet := map[string]bool{}
	for _, f := range focus {
		focusSet[filepath.ToSlash(f)] = true
	}
	total := 0.0
	for i, f := range m.Files {
		pers[i] = 1
		if focusSet[f.Path] {
			pers[i] = 25
		}
		total += pers[i]
	}
	for i := range pers {
		pers[i] /= total
	}

	const damping = 0.85
	pr := make([]float64, n)
	next := make([]float64, n)
	copy(pr, pers)

	for iter := 0; iter < 24; iter++ {
		dangling := 0.0
		for i := range next {
			next[i] = 0
		}
		for i := 0; i < n; i++ {
			sum := 0.0
			for _, w := range out[i] {
				sum += w
			}
			if sum == 0 {
				dangling += pr[i]
				continue
			}
			for j, w := range out[i] {
				next[j] += pr[i] * w / sum
			}
		}
		diff := 0.0
		for i := 0; i < n; i++ {
			v := (1-damping)*pers[i] + damping*(next[i]+dangling*pers[i])
			diff += math.Abs(v - pr[i])
			next[i] = v
		}
		copy(pr, next)
		if diff < 1e-9 {
			break
		}
	}

	for i, f := range m.Files {
		m.rank[f.Path] = pr[i]
		sr := map[string]float64{}
		for _, d := range f.Defs {
			// A declaration nothing references still deserves a floor: it may
			// be an entry point, which is exactly what a reader wants to see.
			sr[d.Name] = symWeight[i][d.Name] + 0.01
		}
		m.symRank[f.Path] = sr
	}
}

// Rank exposes a file's PageRank score.
func (m *Map) Rank(path string) float64 { return m.rank[filepath.ToSlash(path)] }

// RankedFiles returns file paths from most to least depended-upon.
func (m *Map) RankedFiles() []string {
	out := make([]string, 0, len(m.Files))
	for _, f := range m.Files {
		out = append(out, f.Path)
	}
	sort.Slice(out, func(i, j int) bool {
		ri, rj := m.rank[out[i]], m.rank[out[j]]
		if ri != rj {
			return ri > rj
		}
		return out[i] < out[j]
	})
	return out
}

// Render produces the map, stopping once the token budget is spent. Budgeting
// happens per file so the output is always a prefix of the ranking — the
// highest-value files are never the ones cut.
func (m *Map) Render(maxTokens int) string {
	if maxTokens <= 0 || len(m.Files) == 0 {
		return ""
	}
	byPath := map[string]*FileSyms{}
	for _, f := range m.Files {
		byPath[f.Path] = f
	}

	var sb strings.Builder
	sb.WriteString("REPOSITORY MAP — files ranked by how much the rest of the code depends on them.\n")
	sb.WriteString("Signatures only. Read a file when you need its body.\n\n")

	budget := maxTokens
	spent := estTokens(sb.String())
	shown, omitted := 0, 0

	for _, path := range m.RankedFiles() {
		f := byPath[path]
		if len(f.Defs) == 0 {
			continue
		}
		block := m.renderFile(f)
		cost := estTokens(block)
		if spent+cost > budget {
			// Keep counting so the footer can report what was left out.
			omitted++
			continue
		}
		sb.WriteString(block)
		spent += cost
		shown++
	}

	if omitted > 0 {
		fmt.Fprintf(&sb, "\n[%d more files not shown. Use glob or grep to find them.]\n", omitted)
	}
	_ = shown
	return sb.String()
}

func (m *Map) renderFile(f *FileSyms) string {
	sr := m.symRank[f.Path]
	defs := append([]Def(nil), f.Defs...)
	sort.Slice(defs, func(i, j int) bool {
		si, sj := sr[defs[i].Name], sr[defs[j].Name]
		if si != sj {
			return si > sj
		}
		return defs[i].Line < defs[j].Line
	})
	// A file with 200 declarations would swamp the map; the top slice by
	// external interest is what carries the information.
	const maxPerFile = 25
	trimmed := len(defs)
	if len(defs) > maxPerFile {
		defs = defs[:maxPerFile]
	}
	// Within the shown set, source order reads far better than score order.
	sort.Slice(defs, func(i, j int) bool { return defs[i].Line < defs[j].Line })

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s\n", f.Path)
	for _, d := range defs {
		fmt.Fprintf(&sb, "  %s\n", d.Sig)
	}
	if trimmed > len(defs) {
		fmt.Fprintf(&sb, "  … %d more declarations\n", trimmed-len(defs))
	}
	sb.WriteString("\n")
	return sb.String()
}

// estTokens approximates token count from characters. Code sits near 3.6
// chars/token across the tokenizers we target; being within ~15% is enough for
// budgeting, and it avoids shipping a tokenizer per model.
func estTokens(s string) int { return len(s) * 10 / 36 }

// ---------- cache ----------

type cacheEntry struct {
	Size int64     `json:"size"`
	Mod  int64     `json:"mod"`
	Syms *FileSyms `json:"syms"`
}

func cachePath(dir, root string) string {
	if dir == "" {
		return ""
	}
	// Key on the workspace path so several repos can share one state dir.
	h := 1469598103934665603
	for _, c := range strings.ToLower(root) {
		h ^= int(c)
		h *= 1099511628211
	}
	return filepath.Join(dir, fmt.Sprintf("repomap-%x.json", uint64(h)))
}

func loadCache(dir, root string) map[string]cacheEntry {
	out := map[string]cacheEntry{}
	p := cachePath(dir, root)
	if p == "" {
		return out
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	return out
}

func saveCache(dir, root string, entries map[string]cacheEntry) {
	p := cachePath(dir, root)
	if p == "" || len(entries) == 0 {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		return
	}
	tmp := p + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) == nil {
		_ = os.Rename(tmp, p)
	}
}
