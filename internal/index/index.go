package index

import (
	"context"
	"encoding/binary"
	"encoding/gob"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/VEER-TARGARYEN/forge/internal/fsx"
)

// FileMeta tracks a file's identity so an update can tell what changed.
type FileMeta struct {
	Hash uint64
	Size int64
}

// Index is the searchable form of a repository.
type Index struct {
	Root       string
	Chunks     []Chunk
	ChunkHash  []uint64
	Files      map[string]FileMeta
	EmbedModel string
	EmbedDim   int

	bm25 *BM25
	vecs *Vectors
	// vecByHash keys embeddings by the hash of the chunk text, not by chunk
	// position. A function that moves down a file, or into another file, keeps
	// its embedding — which is what makes re-indexing after an edit nearly
	// free instead of a full re-embed.
	vecByHash map[uint64][]float32
}

type Options struct {
	MaxFileBytes int64
	// MaxChunks bounds the corpus. Zero means unbounded.
	MaxChunks int
}

// Result is one search hit with its source text loaded.
type Result struct {
	Chunk Chunk
	Score float64
	Text  string
}

type Mode string

const (
	ModeKeyword  Mode = "keyword"
	ModeSemantic Mode = "semantic"
	ModeHybrid   Mode = "hybrid"
)

// Build scans and chunks a tree, then builds the keyword index.
//
// It deliberately does no embedding: BM25 alone is useful, costs no network
// call, and takes well under a second on a normal repository. Vectors are an
// upgrade applied on top, not a precondition for searching at all.
func Build(root string, opts Options) (*Index, error) {
	if opts.MaxFileBytes <= 0 {
		opts.MaxFileBytes = 1 << 20
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	ix := &Index{Root: abs, Files: map[string]FileMeta{}, vecByHash: map[uint64][]float32{}}

	err = fsx.WalkFiles(abs, func(p string, d fs.DirEntry) error {
		rel, err := filepath.Rel(abs, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if !Indexable(rel) {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > opts.MaxFileBytes {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil || fsx.IsBinary(data) {
			return nil
		}
		content := string(data)
		ix.Files[rel] = FileMeta{Hash: fnv64(content), Size: info.Size()}

		for _, c := range ChunkFile(rel, content) {
			if opts.MaxChunks > 0 && len(ix.Chunks) >= opts.MaxChunks {
				return filepath.SkipAll
			}
			c.ID = len(ix.Chunks)
			ix.Chunks = append(ix.Chunks, c)
			ix.ChunkHash = append(ix.ChunkHash, fnv64(Slice(content, c)))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	ix.buildBM25()
	return ix, nil
}

// buildBM25 indexes each chunk as "header + body", so a query matching a
// symbol name or a path scores even when the body does not mention it.
func (ix *Index) buildBM25() {
	docs := make([]string, len(ix.Chunks))
	byFile := map[string]string{}
	for i, c := range ix.Chunks {
		content, ok := byFile[c.File]
		if !ok {
			data, err := os.ReadFile(filepath.Join(ix.Root, filepath.FromSlash(c.File)))
			if err == nil {
				content = string(data)
			}
			byFile[c.File] = content
		}
		docs[i] = c.File + " " + c.Symbol + " " + c.Kind + "\n" + Slice(content, c)
	}
	ix.bm25 = BuildBM25(docs)
}

// NeedsEmbedding returns the chunk texts with no cached vector, plus their
// indices. An unchanged repository returns nothing.
func (ix *Index) NeedsEmbedding() ([]int, []string) {
	var idx []int
	var texts []string
	byFile := map[string]string{}
	for i, c := range ix.Chunks {
		if _, ok := ix.vecByHash[ix.ChunkHash[i]]; ok {
			continue
		}
		content, ok := byFile[c.File]
		if !ok {
			data, err := os.ReadFile(filepath.Join(ix.Root, filepath.FromSlash(c.File)))
			if err == nil {
				content = string(data)
			}
			byFile[c.File] = content
		}
		idx = append(idx, i)
		// The header gives the embedding the file path and symbol name, which
		// carry a lot of the meaning for a short or generic body.
		texts = append(texts, c.Header()+"\n"+Slice(content, c))
	}
	return idx, texts
}

// EmbedFunc vectorises a batch of texts.
type EmbedFunc func(ctx context.Context, texts []string) ([][]float32, error)

// Vectorize fills in missing embeddings and assembles the vector store.
// progress, if non-nil, is called with (done, total) as batches complete.
func (ix *Index) Vectorize(ctx context.Context, model string, embed EmbedFunc, progress func(done, total int)) error {
	idx, texts := ix.NeedsEmbedding()
	if len(texts) > 0 {
		const batch = 64
		for i := 0; i < len(texts); i += batch {
			end := i + batch
			if end > len(texts) {
				end = len(texts)
			}
			vecs, err := embed(ctx, texts[i:end])
			if err != nil {
				return err
			}
			if len(vecs) != end-i {
				return fmt.Errorf("embedder returned %d vectors for %d inputs", len(vecs), end-i)
			}
			for j, v := range vecs {
				ix.vecByHash[ix.ChunkHash[idx[i+j]]] = v
			}
			if progress != nil {
				progress(end, len(texts))
			}
		}
	}
	ix.EmbedModel = model
	return ix.assembleVectors()
}

// assembleVectors builds the searchable store in chunk order. A chunk whose
// embedding is missing gets a zero vector, which scores at cosine 0 rather
// than shifting every later chunk's index.
func (ix *Index) assembleVectors() error {
	dim := 0
	for _, v := range ix.vecByHash {
		dim = len(v)
		break
	}
	if dim == 0 {
		ix.vecs = nil
		ix.EmbedDim = 0
		return nil
	}
	ix.EmbedDim = dim
	vs := NewVectors(dim)
	zero := make([]float32, dim)
	for i := range ix.Chunks {
		if v, ok := ix.vecByHash[ix.ChunkHash[i]]; ok && len(v) == dim {
			vs.Add(v)
		} else {
			vs.Add(zero)
		}
	}
	ix.vecs = vs
	return nil
}

// HasVectors reports whether semantic search is available.
func (ix *Index) HasVectors() bool { return ix.vecs != nil && ix.vecs.Len() == len(ix.Chunks) }

func (ix *Index) Vectors() *Vectors { return ix.vecs }

// Search runs the requested mode and returns results with text loaded.
func (ix *Index) Search(ctx context.Context, query string, limit int, mode Mode, embed EmbedFunc) ([]Result, error) {
	if limit <= 0 {
		limit = 10
	}
	if len(ix.Chunks) == 0 {
		return nil, nil
	}

	var lists [][]Hit
	// Over-fetch each list: fusion needs depth to work with, and truncating
	// each arm to the final limit throws away exactly the mid-ranked items
	// that agreement between arms is supposed to promote.
	deep := limit * 5

	if mode != ModeSemantic {
		if kw := ix.bm25.Search(query, deep); len(kw) > 0 {
			lists = append(lists, kw)
		}
	}
	if mode != ModeKeyword && ix.HasVectors() && embed != nil {
		vecs, err := embed(ctx, []string{query})
		if err != nil {
			// Semantic-only cannot degrade; hybrid can, and should.
			if mode == ModeSemantic {
				return nil, err
			}
		} else if len(vecs) == 1 {
			if sem := ix.vecs.Search(vecs[0], deep, 0); len(sem) > 0 {
				lists = append(lists, sem)
			}
		}
	}
	if len(lists) == 0 {
		return nil, nil
	}

	var fused []Hit
	if len(lists) == 1 {
		fused = lists[0]
		if len(fused) > limit {
			fused = fused[:limit]
		}
	} else {
		fused = RRF(lists, 60, limit)
	}
	return ix.load(fused), nil
}

// load reads the source text for a result set. Only the handful of chunks
// being returned are read, which is why chunk bodies are not stored.
func (ix *Index) load(hits []Hit) []Result {
	byFile := map[string]string{}
	out := make([]Result, 0, len(hits))
	for _, h := range hits {
		if h.Doc < 0 || h.Doc >= len(ix.Chunks) {
			continue
		}
		c := ix.Chunks[h.Doc]
		content, ok := byFile[c.File]
		if !ok {
			data, err := os.ReadFile(filepath.Join(ix.Root, filepath.FromSlash(c.File)))
			if err == nil {
				content = string(data)
			}
			byFile[c.File] = content
		}
		out = append(out, Result{Chunk: c, Score: h.Score, Text: Slice(content, c)})
	}
	return out
}

// OpenOrBuild loads a persisted index, rebuilding it when the tree has moved
// on. Embeddings are carried across the rebuild by content hash, so a normal
// edit costs a re-chunk and a handful of re-embeds rather than a full pass.
func OpenOrBuild(dir, root string, opts Options) (*Index, error) {
	old, err := Load(dir)
	if err != nil {
		return Build(root, opts)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	changed, removed, _ := old.Stale(abs, opts)
	if len(changed) == 0 && len(removed) == 0 {
		old.Root = abs
		return old, nil
	}
	fresh, err := Build(abs, opts)
	if err != nil {
		return nil, err
	}
	fresh.vecByHash = old.vecByHash
	fresh.EmbedModel = old.EmbedModel
	_ = fresh.assembleVectors()
	return fresh, nil
}

// Stale reports files that changed, were added, or were deleted since the
// index was built.
func (ix *Index) Stale(root string, opts Options) (changed, removed []string, err error) {
	if opts.MaxFileBytes <= 0 {
		opts.MaxFileBytes = 1 << 20
	}
	seen := map[string]bool{}
	err = fsx.WalkFiles(root, func(p string, d fs.DirEntry) error {
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if !Indexable(rel) {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > opts.MaxFileBytes {
			return nil
		}
		seen[rel] = true
		prev, ok := ix.Files[rel]
		if !ok || prev.Size != info.Size() {
			changed = append(changed, rel)
			return nil
		}
		data, err := os.ReadFile(p)
		if err == nil && fnv64(string(data)) != prev.Hash {
			changed = append(changed, rel)
		}
		return nil
	})
	for rel := range ix.Files {
		if !seen[rel] {
			removed = append(removed, rel)
		}
	}
	sort.Strings(changed)
	sort.Strings(removed)
	return changed, removed, err
}

// Stats summarizes an index for display.
type Stats struct {
	Files       int
	Chunks      int
	Terms       int
	Embedded    int
	Dim         int
	EmbedModel  string
	BinaryBytes int
	FullBytes   int
}

func (ix *Index) Stats() Stats {
	s := Stats{
		Files: len(ix.Files), Chunks: len(ix.Chunks),
		Dim: ix.EmbedDim, EmbedModel: ix.EmbedModel,
	}
	if ix.bm25 != nil {
		s.Terms = len(ix.bm25.Postings)
	}
	s.Embedded = len(ix.vecByHash)
	if ix.vecs != nil {
		s.BinaryBytes, s.FullBytes = ix.vecs.Bytes()
	}
	return s
}

// ---------- persistence ----------

// The index is written as two files: metadata and the keyword index through
// gob, and the embeddings as raw little-endian float32.
//
// Vectors get their own format because gob on a few million floats is both
// slow and several times larger than the data. Raw float32 is exactly 4 bytes
// per dimension and loads at memory speed.

type persisted struct {
	Root       string
	Chunks     []Chunk
	ChunkHash  []uint64
	Files      map[string]FileMeta
	EmbedModel string
	EmbedDim   int
	BM25       *BM25
}

const vecMagic = "FRGVEC01"

func (ix *Index) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	metaPath := filepath.Join(dir, "index.gob")
	f, err := os.Create(metaPath + ".tmp")
	if err != nil {
		return err
	}
	err = gob.NewEncoder(f).Encode(persisted{
		Root: ix.Root, Chunks: ix.Chunks, ChunkHash: ix.ChunkHash, Files: ix.Files,
		EmbedModel: ix.EmbedModel, EmbedDim: ix.EmbedDim, BM25: ix.bm25,
	})
	cerr := f.Close()
	if err != nil {
		return err
	}
	if cerr != nil {
		return cerr
	}
	if err := os.Rename(metaPath+".tmp", metaPath); err != nil {
		return err
	}

	return ix.saveVectors(filepath.Join(dir, "index.vec"))
}

func (ix *Index) saveVectors(path string) error {
	if len(ix.vecByHash) == 0 {
		os.Remove(path)
		return nil
	}
	f, err := os.Create(path + ".tmp")
	if err != nil {
		return err
	}
	defer f.Close()

	dim := ix.EmbedDim
	if dim == 0 {
		for _, v := range ix.vecByHash {
			dim = len(v)
			break
		}
	}
	if _, err := f.WriteString(vecMagic); err != nil {
		return err
	}
	hdr := make([]byte, 8)
	binary.LittleEndian.PutUint32(hdr[0:4], uint32(len(ix.vecByHash)))
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(dim))
	if _, err := f.Write(hdr); err != nil {
		return err
	}

	rec := make([]byte, 8+dim*4)
	for h, v := range ix.vecByHash {
		if len(v) != dim {
			continue
		}
		binary.LittleEndian.PutUint64(rec[0:8], h)
		for i, x := range v {
			binary.LittleEndian.PutUint32(rec[8+i*4:], math.Float32bits(x))
		}
		if _, err := f.Write(rec); err != nil {
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// Load reads an index from disk. A missing or unreadable vector file is not an
// error: the index degrades to keyword-only rather than failing to open.
func Load(dir string) (*Index, error) {
	f, err := os.Open(filepath.Join(dir, "index.gob"))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var p persisted
	if err := gob.NewDecoder(f).Decode(&p); err != nil {
		return nil, fmt.Errorf("decode index: %w", err)
	}
	ix := &Index{
		Root: p.Root, Chunks: p.Chunks, ChunkHash: p.ChunkHash, Files: p.Files,
		EmbedModel: p.EmbedModel, EmbedDim: p.EmbedDim, bm25: p.BM25,
		vecByHash: map[uint64][]float32{},
	}
	if ix.Files == nil {
		ix.Files = map[string]FileMeta{}
	}
	if err := ix.loadVectors(filepath.Join(dir, "index.vec")); err == nil {
		_ = ix.assembleVectors()
	}
	return ix, nil
}

func (ix *Index) loadVectors(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(raw) < len(vecMagic)+8 || string(raw[:len(vecMagic)]) != vecMagic {
		return fmt.Errorf("bad vector file header")
	}
	off := len(vecMagic)
	count := int(binary.LittleEndian.Uint32(raw[off : off+4]))
	dim := int(binary.LittleEndian.Uint32(raw[off+4 : off+8]))
	off += 8
	if dim <= 0 || dim > 16384 {
		return fmt.Errorf("implausible embedding dimension %d", dim)
	}
	recLen := 8 + dim*4
	if len(raw)-off < count*recLen {
		return fmt.Errorf("vector file truncated")
	}
	for i := 0; i < count; i++ {
		base := off + i*recLen
		h := binary.LittleEndian.Uint64(raw[base : base+8])
		v := make([]float32, dim)
		for j := 0; j < dim; j++ {
			v[j] = math.Float32frombits(binary.LittleEndian.Uint32(raw[base+8+j*4:]))
		}
		ix.vecByHash[h] = v
	}
	ix.EmbedDim = dim
	return nil
}

// Dir returns the on-disk location for a workspace's index inside stateDir.
func Dir(stateDir, root string) string {
	h := fnv64(strings.ToLower(filepath.Clean(root)))
	return filepath.Join(stateDir, "index", fmt.Sprintf("%x", h))
}

func fnv64(s string) uint64 {
	var h uint64 = 1469598103934665603
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}
