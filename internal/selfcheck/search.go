package selfcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"

	"github.com/VEER-TARGARYEN/forge/internal/index"
	"github.com/VEER-TARGARYEN/forge/internal/provider"
	"github.com/VEER-TARGARYEN/forge/internal/tools"
)

// searchCases cover Phase 4: chunking, BM25, the binary-quantized vector
// store, rank fusion, and the index lifecycle.
func searchCases() []namedCheck {
	return []namedCheck{
		{"tokenize: splits camelCase, snake_case, acronyms", checkTokenize},
		{"bm25: ranks the document containing the terms", checkBM25Rank},
		{"bm25: IDF discounts a ubiquitous term", checkBM25IDF},
		{"chunk: splits code at declaration boundaries", checkChunkCode},
		{"chunk: caps an oversized declaration", checkChunkCap},
		{"chunk: windows prose with overlap", checkChunkProse},
		{"vector: binary first stage finds a planted neighbour", checkVectorPlanted},
		{"vector: two-stage recall against exact brute force", checkVectorRecall},
		{"vector: binary stage is ~32x smaller than float32", checkVectorMemory},
		{"rrf: agreement between lists beats a single strong rank", checkRRF},
		{"index: build then find the right file", checkIndexSearch},
		{"index: save/load round-trips search results", checkIndexRoundTrip},
		{"index: reuses embeddings by content hash after an edit", checkIndexIncremental},
		{"index: detects changed and removed files", checkIndexStale},
		{"index: hybrid degrades to keyword without an embedder", checkIndexDegrade},
		{"embed: batches over 64 and preserves input order", checkEmbedOrder},
		{"search_code: reports unavailable instead of failing", checkSearchToolUnavailable},
	}
}

// ---------- tokenizer ----------

func checkTokenize() (string, error) {
	cases := []struct {
		in   string
		want []string
	}{
		{"parseArgs", []string{"parseargs", "parse", "args"}},
		{"HTTPServer", []string{"httpserver", "http", "server"}},
		{"MAX_RETRY_COUNT", []string{"max_retry_count", "max", "retry", "count"}},
		{"snake_case_name", []string{"snake_case_name", "snake", "case", "name"}},
		{"XMLHttpRequest", []string{"xmlhttprequest", "xml", "http", "request"}},
	}
	for _, c := range cases {
		got := index.Tokenize(c.in)
		for _, w := range c.want {
			if !containsStr(got, w) {
				return "", failf("Tokenize(%q) = %v, missing %q", c.in, got, w)
			}
		}
	}
	return fmt.Sprintf("%d identifiers", len(cases)), nil
}

func checkBM25Rank() (string, error) {
	docs := []string{
		"func handleRequest(w http.ResponseWriter, r *http.Request) { }",
		"type Config struct { Timeout time.Duration }",
		"func retryWithBackoff(attempts int) error { }",
		"// documentation about configuration and timeouts",
	}
	b := index.BuildBM25(docs)

	hits := b.Search("retry backoff", 4)
	if len(hits) == 0 || hits[0].Doc != 2 {
		return "", failf("'retry backoff' ranked %v, want doc 2 first", hits)
	}
	hits = b.Search("handleRequest", 4)
	if len(hits) == 0 || hits[0].Doc != 0 {
		return "", failf("'handleRequest' ranked %v, want doc 0 first", hits)
	}
	// Split parts must match too: querying "handle request" as two words has
	// to reach the camelCase identifier.
	hits = b.Search("handle request", 4)
	if len(hits) == 0 || hits[0].Doc != 0 {
		return "", failf("'handle request' ranked %v, want doc 0 first", hits)
	}
	if len(b.Search("nonexistentterm", 4)) != 0 {
		return "", failf("a term absent from the corpus returned hits")
	}
	return "3 queries ranked correctly", nil
}

func checkBM25IDF() (string, error) {
	// "func" is in every document and carries no signal; "quantize" is in one.
	docs := []string{
		"func alpha() {}", "func beta() {}", "func gamma() {}",
		"func quantize() {}", "func delta() {}", "func epsilon() {}",
	}
	b := index.BuildBM25(docs)

	rare := b.Search("quantize", 6)
	common := b.Search("func", 6)
	if len(rare) == 0 {
		return "", failf("rare term found nothing")
	}
	if rare[0].Doc != 3 {
		return "", failf("rare term ranked doc %d, want 3", rare[0].Doc)
	}
	if len(common) > 0 && common[0].Score >= rare[0].Score {
		return "", failf("ubiquitous term scored %.3f, not below the rare term's %.3f",
			common[0].Score, rare[0].Score)
	}
	return fmt.Sprintf("rare %.2f > common %.2f", rare[0].Score, common[0].Score), nil
}

// ---------- chunking ----------

func checkChunkCode() (string, error) {
	src := "package main\n\nimport \"fmt\"\n\nfunc Alpha() {\n\tfmt.Println(1)\n}\n\n" +
		"type Beta struct {\n\tX int\n}\n\nfunc Gamma() {\n\tfmt.Println(2)\n}\n"
	chunks := index.ChunkFile("main.go", src)
	if len(chunks) < 4 {
		return "", failf("got %d chunks, want at least 4 (preamble + 3 declarations)", len(chunks))
	}
	// The preamble carries the package clause and imports, which is often the
	// most informative thing about a file.
	if chunks[0].Kind != "preamble" || chunks[0].Start != 1 {
		return "", failf("first chunk = %+v, want a preamble starting at line 1", chunks[0])
	}
	syms := map[string]index.Chunk{}
	for _, c := range chunks {
		if c.Symbol != "" {
			syms[c.Symbol] = c
		}
	}
	for _, want := range []string{"Alpha", "Beta", "Gamma"} {
		c, ok := syms[want]
		if !ok {
			return "", failf("no chunk for %q", want)
		}
		if !strings.Contains(index.Slice(src, c), want) {
			return "", failf("chunk for %q does not contain it: %q", want, index.Slice(src, c))
		}
	}
	// Boundaries must not overlap: Alpha's chunk must end before Beta starts.
	if syms["Alpha"].End >= syms["Beta"].Start {
		return "", failf("chunks overlap: Alpha ends %d, Beta starts %d", syms["Alpha"].End, syms["Beta"].Start)
	}
	return fmt.Sprintf("%d chunks, boundaries clean", len(chunks)), nil
}

func checkChunkCap() (string, error) {
	var sb strings.Builder
	sb.WriteString("package main\n\nfunc Enormous() {\n")
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&sb, "\tstep%d()\n", i)
	}
	sb.WriteString("}\n")

	chunks := index.ChunkFile("big.go", sb.String())
	var forFn int
	for _, c := range chunks {
		if strings.HasPrefix(c.Kind, "func") {
			forFn++
			if c.End-c.Start+1 > 120 {
				return "", failf("chunk spans %d lines, over the 120 cap", c.End-c.Start+1)
			}
		}
	}
	if forFn < 3 {
		return "", failf("a 400-line function produced only %d chunks", forFn)
	}
	return fmt.Sprintf("split into %d capped chunks", forFn), nil
}

func checkChunkProse() (string, error) {
	var sb strings.Builder
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&sb, "This is line %d of the document with enough text to matter.\n", i)
	}
	chunks := index.ChunkFile("doc.md", sb.String())
	if len(chunks) < 3 {
		return "", failf("200 lines produced %d chunks", len(chunks))
	}
	// Overlap keeps a match spanning a boundary findable from either side.
	if chunks[1].Start > chunks[0].End {
		return "", failf("no overlap: chunk0 ends %d, chunk1 starts %d", chunks[0].End, chunks[1].Start)
	}
	last := chunks[len(chunks)-1]
	if last.End != 200 {
		return "", failf("last chunk ends at %d, want 200 (tail dropped)", last.End)
	}
	return fmt.Sprintf("%d windows with overlap", len(chunks)), nil
}

// ---------- vectors ----------

func randVec(rng *rand.Rand, dim int) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = float32(rng.NormFloat64())
	}
	return v
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func exactTopK(raw [][]float32, q []float32, k int) []int {
	type sc struct {
		i int
		s float64
	}
	all := make([]sc, len(raw))
	for i, v := range raw {
		all[i] = sc{i, cosine(q, v)}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].s > all[j].s })
	out := make([]int, 0, k)
	for i := 0; i < k && i < len(all); i++ {
		out = append(out, all[i].i)
	}
	return out
}

func checkVectorPlanted() (string, error) {
	rng := rand.New(rand.NewSource(7))
	const n, dim = 2000, 256

	vs := index.NewVectors(dim)
	for i := 0; i < n; i++ {
		vs.Add(randVec(rng, dim))
	}
	// A near-duplicate of the query, planted last. If the binary first stage
	// discards it, the whole two-stage design is unsound.
	query := randVec(rng, dim)
	planted := make([]float32, dim)
	for i := range query {
		planted[i] = query[i] + float32(rng.NormFloat64())*0.05
	}
	vs.Add(planted)

	hits := vs.Search(query, 5, 0)
	if len(hits) == 0 {
		return "", failf("no hits")
	}
	if hits[0].Doc != n {
		return "", failf("top hit is doc %d, want the planted neighbour at %d", hits[0].Doc, n)
	}
	if hits[0].Score < 0.9 {
		return "", failf("planted neighbour scored only %.3f", hits[0].Score)
	}
	return fmt.Sprintf("found at rank 1, cosine %.3f", hits[0].Score), nil
}

// measureRecall reports recall@k of the two-stage search against exact
// brute-force cosine over the same corpus.
func measureRecall(vs *index.Vectors, raw [][]float32, queries [][]float32, k int) float64 {
	found, wanted := 0, 0
	for _, q := range queries {
		want := exactTopK(raw, q, k)
		got := vs.Search(q, k, 0)
		gotSet := map[int]bool{}
		for _, h := range got {
			gotSet[h.Doc] = true
		}
		for _, w := range want {
			if gotSet[w] {
				found++
			}
		}
		wanted += len(want)
	}
	if wanted == 0 {
		return 0
	}
	return float64(found) / float64(wanted)
}

func checkVectorRecall() (string, error) {
	const dim, k, trials = 256, 10, 25

	// Clustered corpus: 60 topics with 50 members each, members drawn around a
	// topic centre. This is what real embeddings look like — semantically
	// related chunks sit at cosine 0.7-0.9 while everything else sits near
	// zero, which is exactly the angular structure sign-based quantization
	// preserves well.
	rng := rand.New(rand.NewSource(11))
	const topics, perTopic = 60, 50
	clustered := make([][]float32, 0, topics*perTopic)
	for t := 0; t < topics; t++ {
		centre := randVec(rng, dim)
		for m := 0; m < perTopic; m++ {
			v := make([]float32, dim)
			for i := range centre {
				v[i] = centre[i] + float32(rng.NormFloat64())*0.45
			}
			clustered = append(clustered, v)
		}
	}
	cvs := index.NewVectors(dim)
	for _, v := range clustered {
		cvs.Add(v)
	}
	cq := make([][]float32, trials)
	for i := range cq {
		// Query near a random topic centre, as a real search would be.
		base := clustered[rng.Intn(len(clustered))]
		q := make([]float32, dim)
		for j := range base {
			q[j] = base[j] + float32(rng.NormFloat64())*0.35
		}
		cq[i] = q
	}
	clusteredRecall := measureRecall(cvs, clustered, cq, k)

	// Uniform random vectors: the pathological floor. In 256 dimensions every
	// pair is near-orthogonal, so the gap between the true 10th neighbour and
	// the 500th is a cosine difference of ~0.01 — below what one bit per
	// dimension can resolve. Reported, not asserted at a high bar, because a
	// corpus with no structure has no meaningful nearest neighbours to find.
	rng2 := rand.New(rand.NewSource(11))
	const n = 3000
	uniform := make([][]float32, n)
	uvs := index.NewVectors(dim)
	for i := 0; i < n; i++ {
		uniform[i] = randVec(rng2, dim)
		uvs.Add(uniform[i])
	}
	uq := make([][]float32, trials)
	for i := range uq {
		uq[i] = randVec(rng2, dim)
	}
	uniformRecall := measureRecall(uvs, uniform, uq, k)

	if clusteredRecall < 0.95 {
		return "", failf("recall@%d on clustered data = %.3f, want >= 0.95", k, clusteredRecall)
	}
	if uniformRecall < 0.50 {
		return "", failf("recall@%d on uniform data = %.3f, below the 0.50 sanity floor", k, uniformRecall)
	}
	return fmt.Sprintf("clustered %.3f, uniform floor %.3f (recall@%d, %d queries each)",
		clusteredRecall, uniformRecall, k, trials), nil
}

func checkVectorMemory() (string, error) {
	rng := rand.New(rand.NewSource(3))
	const n, dim = 5000, 768

	vs := index.NewVectors(dim)
	for i := 0; i < n; i++ {
		vs.Add(randVec(rng, dim))
	}
	binary, full := vs.Bytes()
	if binary == 0 || full == 0 {
		return "", failf("Bytes() reported %d / %d", binary, full)
	}
	ratio := float64(full) / float64(binary)
	// float32 is 32 bits per dimension, the binary code is 1. Anything far
	// off 32x means the packing is wrong.
	if ratio < 30 || ratio > 34 {
		return "", failf("ratio = %.1fx, want ~32x (binary %d B, full %d B)", ratio, binary, full)
	}
	return fmt.Sprintf("%.1f MB float32 vs %.2f MB binary (%.0fx)",
		float64(full)/(1<<20), float64(binary)/(1<<20), ratio), nil
}

func checkRRF() (string, error) {
	// Doc 5 is second in both lists; doc 1 is first in one and absent from the
	// other. Fusion should prefer the one both arms agree on.
	a := []index.Hit{{Doc: 1, Score: 99}, {Doc: 5, Score: 9}, {Doc: 7, Score: 8}}
	b := []index.Hit{{Doc: 9, Score: 0.9}, {Doc: 5, Score: 0.8}, {Doc: 3, Score: 0.7}}

	fused := index.RRF([][]index.Hit{a, b}, 60, 5)
	if len(fused) == 0 {
		return "", failf("no fused results")
	}
	if fused[0].Doc != 5 {
		return "", failf("fused top = doc %d, want 5 (present in both lists)", fused[0].Doc)
	}
	// Incompatible score scales must not leak through: doc 1's score of 99 is
	// BM25-shaped, doc 9's 0.9 is cosine-shaped, and RRF must ignore both.
	var d1, d9 float64
	for _, h := range fused {
		switch h.Doc {
		case 1:
			d1 = h.Score
		case 9:
			d9 = h.Score
		}
	}
	if d1 != d9 {
		return "", failf("rank-1-in-one-list docs scored differently: %.6f vs %.6f", d1, d9)
	}
	return "agreement wins, scales ignored", nil
}

// ---------- index lifecycle ----------

func indexWS() (*tools.Workspace, func(), error) {
	return tempWS(map[string]string{
		"internal/auth/token.go": "package auth\n\n// ValidateToken checks a bearer token against the store.\nfunc ValidateToken(tok string) error {\n\treturn nil\n}\n",
		"internal/http/serve.go": "package http\n\nfunc StartServer(addr string) error {\n\treturn nil\n}\n",
		"README.md":              "# Project\n\nThis project does things with widgets and gizmos.\n",
	})
}

func checkIndexSearch() (string, error) {
	ws, cleanup, err := indexWS()
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}
	ix, err := index.Build(ws.Root(), index.Options{})
	if err != nil {
		return "", failf("build: %v", err)
	}
	if len(ix.Chunks) == 0 {
		return "", failf("no chunks produced")
	}

	res, err := ix.Search(context.Background(), "validate token", 3, index.ModeKeyword, nil)
	if err != nil {
		return "", failf("search: %v", err)
	}
	if len(res) == 0 {
		return "", failf("no results")
	}
	if res[0].Chunk.File != "internal/auth/token.go" {
		return "", failf("top hit is %s, want internal/auth/token.go", res[0].Chunk.File)
	}
	if !strings.Contains(res[0].Text, "ValidateToken") {
		return "", failf("result text was not loaded from disk: %q", res[0].Text)
	}
	return fmt.Sprintf("%d chunks, correct file first", len(ix.Chunks)), nil
}

func checkIndexRoundTrip() (string, error) {
	ws, cleanup, err := indexWS()
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}
	dir, err := os.MkdirTemp("", "forge-idx-*")
	if err != nil {
		return "", failf("tempdir: %v", err)
	}
	defer os.RemoveAll(dir)

	ix, err := index.Build(ws.Root(), index.Options{})
	if err != nil {
		return "", failf("build: %v", err)
	}
	// Synthetic embeddings, so the vector file is exercised without a network.
	rng := rand.New(rand.NewSource(5))
	err = ix.Vectorize(context.Background(), "stub-embed",
		func(ctx context.Context, texts []string) ([][]float32, error) {
			out := make([][]float32, len(texts))
			for i := range texts {
				out[i] = randVec(rng, 64)
			}
			return out, nil
		}, nil)
	if err != nil {
		return "", failf("vectorize: %v", err)
	}
	if !ix.HasVectors() {
		return "", failf("vectors were not assembled")
	}
	if err := ix.Save(dir); err != nil {
		return "", failf("save: %v", err)
	}

	loaded, err := index.Load(dir)
	if err != nil {
		return "", failf("load: %v", err)
	}
	if len(loaded.Chunks) != len(ix.Chunks) {
		return "", failf("chunk count changed: %d -> %d", len(ix.Chunks), len(loaded.Chunks))
	}
	if !loaded.HasVectors() {
		return "", failf("vectors did not survive the round trip")
	}
	if loaded.EmbedDim != 64 || loaded.EmbedModel != "stub-embed" {
		return "", failf("embedding metadata lost: dim %d model %q", loaded.EmbedDim, loaded.EmbedModel)
	}

	before, _ := ix.Search(context.Background(), "validate token", 3, index.ModeKeyword, nil)
	after, err := loaded.Search(context.Background(), "validate token", 3, index.ModeKeyword, nil)
	if err != nil {
		return "", failf("search after load: %v", err)
	}
	if len(before) != len(after) || len(after) == 0 || before[0].Chunk.File != after[0].Chunk.File {
		return "", failf("search results differ after reload")
	}
	return fmt.Sprintf("%d chunks + %d-dim vectors round-tripped", len(loaded.Chunks), loaded.EmbedDim), nil
}

func checkIndexIncremental() (string, error) {
	ws, cleanup, err := indexWS()
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}
	dir, err := os.MkdirTemp("", "forge-idx-*")
	if err != nil {
		return "", failf("tempdir: %v", err)
	}
	defer os.RemoveAll(dir)

	rng := rand.New(rand.NewSource(9))
	embedded := 0
	embed := func(ctx context.Context, texts []string) ([][]float32, error) {
		embedded += len(texts)
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = randVec(rng, 32)
		}
		return out, nil
	}

	ix, err := index.Build(ws.Root(), index.Options{})
	if err != nil {
		return "", failf("build: %v", err)
	}
	if err := ix.Vectorize(context.Background(), "stub", embed, nil); err != nil {
		return "", failf("vectorize: %v", err)
	}
	first := embedded
	if first == 0 {
		return "", failf("nothing was embedded on the first pass")
	}
	if err := ix.Save(dir); err != nil {
		return "", failf("save: %v", err)
	}

	// Touch one file. Only its chunks should need re-embedding; everything
	// else is looked up by content hash.
	target := ws.Root() + string(os.PathSeparator) + "README.md"
	if err := os.WriteFile(target, []byte("# Project\n\nCompletely rewritten prose about sprockets.\n"), 0o644); err != nil {
		return "", failf("edit: %v", err)
	}

	embedded = 0
	reopened, err := index.OpenOrBuild(dir, ws.Root(), index.Options{})
	if err != nil {
		return "", failf("reopen: %v", err)
	}
	if err := reopened.Vectorize(context.Background(), "stub", embed, nil); err != nil {
		return "", failf("re-vectorize: %v", err)
	}
	second := embedded
	if second == 0 {
		return "", failf("the edited file was not re-embedded")
	}
	if second >= first {
		return "", failf("re-embedded %d of %d chunks; hash reuse did not happen", second, first)
	}
	if !reopened.HasVectors() {
		return "", failf("vectors missing after incremental update")
	}
	return fmt.Sprintf("%d of %d chunks re-embedded after one file changed", second, first), nil
}

func checkIndexStale() (string, error) {
	ws, cleanup, err := indexWS()
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}
	ix, err := index.Build(ws.Root(), index.Options{})
	if err != nil {
		return "", failf("build: %v", err)
	}
	changed, removed, err := ix.Stale(ws.Root(), index.Options{})
	if err != nil {
		return "", failf("stale: %v", err)
	}
	if len(changed) != 0 || len(removed) != 0 {
		return "", failf("fresh index reported %d changed, %d removed", len(changed), len(removed))
	}

	sep := string(os.PathSeparator)
	if err := os.WriteFile(ws.Root()+sep+"README.md", []byte("# Different\n\nNew content entirely here.\n"), 0o644); err != nil {
		return "", failf("edit: %v", err)
	}
	if err := os.Remove(ws.Root() + sep + "internal" + sep + "http" + sep + "serve.go"); err != nil {
		return "", failf("remove: %v", err)
	}
	if err := os.WriteFile(ws.Root()+sep+"NEW.md", []byte("# New file\n\nWith some content in it.\n"), 0o644); err != nil {
		return "", failf("add: %v", err)
	}

	changed, removed, err = ix.Stale(ws.Root(), index.Options{})
	if err != nil {
		return "", failf("stale: %v", err)
	}
	if !containsStr(changed, "README.md") || !containsStr(changed, "NEW.md") {
		return "", failf("changed = %v, want README.md and NEW.md", changed)
	}
	if !containsStr(removed, "internal/http/serve.go") {
		return "", failf("removed = %v, want internal/http/serve.go", removed)
	}
	return "edit, add, and delete all detected", nil
}

func checkIndexDegrade() (string, error) {
	ws, cleanup, err := indexWS()
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}
	ix, err := index.Build(ws.Root(), index.Options{})
	if err != nil {
		return "", failf("build: %v", err)
	}
	// Hybrid with no vectors and no embedder must still answer, not error.
	res, err := ix.Search(context.Background(), "start server", 3, index.ModeHybrid, nil)
	if err != nil {
		return "", failf("hybrid without an embedder errored: %v", err)
	}
	if len(res) == 0 {
		return "", failf("hybrid degraded to nothing instead of keyword results")
	}
	if res[0].Chunk.File != "internal/http/serve.go" {
		return "", failf("top hit is %s, want internal/http/serve.go", res[0].Chunk.File)
	}
	if ix.HasVectors() {
		return "", failf("HasVectors is true on an index that was never vectorised")
	}
	return "answered from keyword arm alone", nil
}

// ---------- embeddings ----------

func checkEmbedOrder() (string, error) {
	const dim = 8
	// The server echoes the input index into the vector's first slot and
	// returns the batch in reverse order, so any reliance on arrival order
	// shows up immediately.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)

		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		items := make([]item, len(req.Input))
		for i, s := range req.Input {
			v := make([]float32, dim)
			// Recover the caller's ordinal from the input text itself.
			var n int
			fmt.Sscanf(s, "input-%d", &n)
			v[0] = float32(n)
			items[len(req.Input)-1-i] = item{Index: i, Embedding: v}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": items, "model": "m", "usage": map[string]int{"prompt_tokens": 10, "total_tokens": 10},
		})
	}))
	defer srv.Close()

	c := provider.NewOpenAICompat(provider.OpenAIOpts{
		Name: "e", BaseURL: srv.URL + "/v1", APIKey: "k", RequiresKey: true,
	})

	// 150 inputs forces three batches at the 64 cap.
	inputs := make([]string, 150)
	for i := range inputs {
		inputs[i] = fmt.Sprintf("input-%d", i)
	}
	resp, err := c.Embed(context.Background(), "m", inputs)
	if err != nil {
		return "", failf("embed: %v", err)
	}
	if len(resp.Vectors) != len(inputs) {
		return "", failf("got %d vectors for %d inputs", len(resp.Vectors), len(inputs))
	}
	for i, v := range resp.Vectors {
		if int(v[0]) != i {
			return "", failf("vector %d carries ordinal %d; order was not preserved", i, int(v[0]))
		}
	}
	if resp.Usage.TotalTokens != 30 {
		return "", failf("usage = %d, want 30 accumulated across 3 batches", resp.Usage.TotalTokens)
	}
	return "150 inputs, 3 batches, order intact", nil
}

func checkSearchToolUnavailable() (string, error) {
	ws, cleanup, err := tempWS(nil)
	defer cleanup()
	if err != nil {
		return "", failf("setup: %v", err)
	}
	env, _ := envFor(ws, true)
	env.Overflow = tools.NewOverflow("")
	env.SearchCode = nil

	res, err := runTool(tools.SearchCode{}, map[string]any{"query": "anything"}, env)
	if err != nil {
		return "", failf("run: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "grep") {
		return "", failf("unavailable search did not point at an alternative: %+v", res)
	}

	// With a backend wired, the tool must render hits and report the mode.
	env.SearchCode = func(ctx context.Context, q string, limit int, mode string) ([]tools.SearchHit, string, error) {
		return []tools.SearchHit{{
			File: "a.go", Start: 10, End: 12, Symbol: "Foo", Kind: "func",
			Score: 1.5, Text: "func Foo() {}",
		}}, "keyword", nil
	}
	res2, err := runTool(tools.SearchCode{}, map[string]any{"query": "foo"}, env)
	if err != nil {
		return "", failf("run: %v", err)
	}
	if res2.IsError {
		return "", failf("search errored: %s", res2.Content)
	}
	for _, want := range []string{"a.go:10-12", "func Foo", "keyword"} {
		if !strings.Contains(res2.Content, want) {
			return "", failf("result missing %q:\n%s", want, res2.Content)
		}
	}
	return "degrades cleanly, renders hits", nil
}

func containsStr(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
