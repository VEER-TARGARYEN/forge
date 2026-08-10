// Package index provides code search: semantic chunking, BM25 keyword search,
// binary-quantized vector search, and rank fusion over both.
//
// It is written from scratch rather than wrapping a vector database because at
// repository scale brute force wins outright. See vector.go for the arithmetic.
package index

import (
	"path/filepath"
	"strings"

	"github.com/VEER-TARGARYEN/forge/internal/repomap"
)

// Chunk is one retrievable unit: ideally a single declaration, so a hit points
// at a function rather than at an arbitrary 40-line window straddling two.
//
// Text is not persisted. The source file is right there on disk and we track
// its hash, so re-reading the handful of chunks that actually make it into a
// result set is cheaper than carrying every chunk's body in the index.
type Chunk struct {
	ID     int    `json:"id"`
	File   string `json:"file"`
	Start  int    `json:"start"`
	End    int    `json:"end"`
	Symbol string `json:"sym,omitempty"`
	Kind   string `json:"kind,omitempty"`
}

// Header renders the chunk's identity for display and for embedding, so the
// model sees where a hit came from without a separate lookup.
func (c Chunk) Header() string {
	if c.Symbol != "" {
		return c.File + ":" + itoa(c.Start) + "  " + c.Kind + " " + c.Symbol
	}
	return c.File + ":" + itoa(c.Start) + "-" + itoa(c.End)
}

const (
	// maxChunkLines caps a chunk so one enormous function cannot dominate a
	// result set or an embedding.
	maxChunkLines = 120
	// windowLines is the fallback chunk size for prose and config files.
	windowLines = 60
	// windowOverlap keeps a match that straddles a boundary findable.
	windowOverlap = 10
	// minChunkChars drops chunks too small to carry meaning — a closing brace
	// or a one-line import is retrieval noise.
	minChunkChars = 24
)

// textExts are files worth indexing that have no declaration structure.
var textExts = map[string]bool{
	".md": true, ".markdown": true, ".txt": true, ".rst": true, ".adoc": true,
	".json": true, ".yaml": true, ".yml": true, ".toml": true, ".ini": true,
	".sql": true, ".sh": true, ".bash": true, ".zsh": true, ".ps1": true,
	".html": true, ".htm": true, ".css": true, ".scss": true, ".less": true,
	".proto": true, ".graphql": true, ".gql": true, ".tf": true, ".hcl": true,
	".env": true, ".cfg": true, ".conf": true, ".xml": true, ".gradle": true,
}

// textNames are extensionless files worth indexing.
var textNames = map[string]bool{
	"Makefile": true, "Dockerfile": true, "Jenkinsfile": true,
	"CMakeLists.txt": true, "go.mod": true, "Cargo.toml": true,
	"README": true, "LICENSE": true,
}

// Indexable reports whether a path is worth chunking at all.
func Indexable(rel string) bool {
	ext := strings.ToLower(filepath.Ext(rel))
	if repomap.Supported(ext) || textExts[ext] {
		return true
	}
	return textNames[filepath.Base(rel)]
}

// ChunkFile splits one file into retrievable units.
//
// Code is split at declaration boundaries using the same regex extractor the
// repo map uses, so a chunk is a function or a type rather than a window that
// happens to start mid-body. That boundary choice is most of what separates
// useful code retrieval from the sliding-window kind.
func ChunkFile(rel, content string) []Chunk {
	lines := splitLines(content)
	if len(lines) == 0 {
		return nil
	}
	ext := strings.ToLower(filepath.Ext(rel))

	if !repomap.Supported(ext) {
		return windowChunks(rel, lines)
	}
	syms := repomap.Extract(rel, ext, content)
	if len(syms.Defs) == 0 {
		return windowChunks(rel, lines)
	}

	var out []Chunk
	// Everything above the first declaration: package clause, imports, the
	// file-level comment. Often the best summary of what a file is for.
	if syms.Defs[0].Line > 1 {
		out = append(out, split(rel, lines, 1, syms.Defs[0].Line-1, "", "preamble")...)
	}
	for i, d := range syms.Defs {
		end := len(lines)
		if i+1 < len(syms.Defs) {
			end = syms.Defs[i+1].Line - 1
		}
		if end < d.Line {
			end = d.Line
		}
		out = append(out, split(rel, lines, d.Line, end, d.Name, d.Kind)...)
	}
	return renumber(out)
}

// split emits one chunk for a line range, subdividing when it exceeds the cap.
func split(rel string, lines []string, start, end int, sym, kind string) []Chunk {
	if start < 1 {
		start = 1
	}
	if end > len(lines) {
		end = len(lines)
	}
	if end < start {
		return nil
	}
	var out []Chunk
	for s := start; s <= end; s += maxChunkLines {
		e := s + maxChunkLines - 1
		if e > end {
			e = end
		}
		if !worthKeeping(lines[s-1 : e]) {
			continue
		}
		c := Chunk{File: rel, Start: s, End: e, Symbol: sym, Kind: kind}
		if s > start {
			// A continuation carries the symbol name but is not its head.
			c.Kind = kind + "-cont"
		}
		out = append(out, c)
	}
	return out
}

func windowChunks(rel string, lines []string) []Chunk {
	var out []Chunk
	step := windowLines - windowOverlap
	if step < 1 {
		step = windowLines
	}
	for s := 1; s <= len(lines); s += step {
		e := s + windowLines - 1
		if e > len(lines) {
			e = len(lines)
		}
		if worthKeeping(lines[s-1 : e]) {
			out = append(out, Chunk{File: rel, Start: s, End: e})
		}
		if e == len(lines) {
			break
		}
	}
	return renumber(out)
}

func worthKeeping(lines []string) bool {
	n := 0
	for _, l := range lines {
		n += len(strings.TrimSpace(l))
		if n >= minChunkChars {
			return true
		}
	}
	return false
}

func renumber(cs []Chunk) []Chunk {
	for i := range cs {
		cs[i].ID = i
	}
	return cs
}

// splitLines splits into real lines, dropping the empty element a trailing
// newline produces.
//
// Without this, a 200-line file that ends in a newline reports 201 lines, and
// every chunk range shown to the model is off by one past the end of the file.
func splitLines(content string) []string {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

// Slice extracts a chunk's text from the file content it came from.
func Slice(content string, c Chunk) string {
	lines := splitLines(content)
	s, e := c.Start-1, c.End
	if s < 0 {
		s = 0
	}
	if e > len(lines) {
		e = len(lines)
	}
	if s >= e {
		return ""
	}
	return strings.Join(lines[s:e], "\n")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
