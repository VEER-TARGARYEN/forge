package index

import (
	"math"
	"math/bits"
	"sort"
)

// Vectors is a brute-force vector store with binary-quantized first-stage
// search.
//
// Why there is no vector database here:
//
//	50,000 chunks × 768 dims, float32   = 154 MB → a full scan reads 154 MB,
//	                                      ~4.4 ms at 35 GB/s. Memory-bound.
//	the same corpus, 1 bit per dim      = 4.8 MB → ~0.14 ms to scan, and the
//	                                      comparison is XOR + POPCNT, which is
//	                                      roughly one cycle per 8 bytes.
//
// So: rank everything by Hamming distance on the binary codes, keep a few
// hundred candidates, and re-score only those in float32. Sub-millisecond,
// exact-by-construction recall over the candidate set, no index build step, no
// server, no dependency. HNSW exists to solve the ten-million-vector problem.
// A repository is not that problem.
type Vectors struct {
	Dim   int
	codes [][]uint64
	vecs  [][]float32
}

func NewVectors(dim int) *Vectors {
	return &Vectors{Dim: dim}
}

func (v *Vectors) Len() int {
	if v == nil {
		return 0
	}
	return len(v.vecs)
}

// Add normalizes and stores one embedding.
//
// Normalizing matters twice over: it makes the dot product a cosine, and it
// makes sign-based binary quantization a sane approximation of direction,
// which is the only thing the first stage is asked to preserve.
func (v *Vectors) Add(vec []float32) {
	n := normalize(vec)
	v.vecs = append(v.vecs, n)
	v.codes = append(v.codes, quantize(n))
}

// quantize packs the sign of each dimension into one bit.
func quantize(vec []float32) []uint64 {
	words := (len(vec) + 63) / 64
	out := make([]uint64, words)
	for i, x := range vec {
		if x > 0 {
			out[i/64] |= 1 << uint(i%64)
		}
	}
	return out
}

func normalize(vec []float32) []float32 {
	var sum float64
	for _, x := range vec {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return append([]float32(nil), vec...)
	}
	inv := float32(1 / math.Sqrt(sum))
	out := make([]float32, len(vec))
	for i, x := range vec {
		out[i] = x * inv
	}
	return out
}

func hamming(a, b []uint64) int {
	d := 0
	for i := range a {
		d += bits.OnesCount64(a[i] ^ b[i])
	}
	return d
}

func dot(a, b []float32) float64 {
	var s float64
	// Four accumulators: independent dependency chains let the CPU keep
	// several FMAs in flight instead of serialising on one running sum.
	var s0, s1, s2, s3 float64
	n := len(a)
	i := 0
	for ; i+4 <= n; i += 4 {
		s0 += float64(a[i]) * float64(b[i])
		s1 += float64(a[i+1]) * float64(b[i+1])
		s2 += float64(a[i+2]) * float64(b[i+2])
		s3 += float64(a[i+3]) * float64(b[i+3])
	}
	for ; i < n; i++ {
		s += float64(a[i]) * float64(b[i])
	}
	return s + s0 + s1 + s2 + s3
}

// Search returns the best `limit` matches.
//
// rerank controls how many binary-stage candidates are re-scored in float32.
// Larger trades latency for recall; the default is generous because the whole
// operation is sub-millisecond anyway.
func (v *Vectors) Search(query []float32, limit, rerank int) []Hit {
	if v == nil || len(v.vecs) == 0 || len(query) != v.Dim {
		return nil
	}
	if limit <= 0 {
		limit = 10
	}
	if rerank <= 0 {
		// Scale the candidate pool with the corpus, not just the result count.
		// The second stage costs one float32 dot product per candidate — a few
		// thousand of those is well under a millisecond — so a tight pool buys
		// nothing and costs recall on corpora where the binary codes are close
		// together.
		rerank = limit * 20
		if byCorpus := len(v.vecs) / 4; byCorpus > rerank {
			rerank = byCorpus
		}
		if rerank > 8000 {
			rerank = 8000
		}
	}
	if rerank < 200 {
		rerank = 200
	}
	if rerank > len(v.vecs) {
		rerank = len(v.vecs)
	}

	q := normalize(query)
	qc := quantize(q)

	// Hamming distance is a small integer in [0, Dim], so the candidate cut is
	// a counting sort rather than a comparison sort: O(n) instead of
	// O(n log n), and the histogram is 769 ints.
	counts := make([]int32, v.Dim+2)
	dists := make([]int32, len(v.codes))
	for i, c := range v.codes {
		d := hamming(qc, c)
		dists[i] = int32(d)
		counts[d]++
	}
	cutoff, running := v.Dim, 0
	for d := 0; d <= v.Dim; d++ {
		running += int(counts[d])
		if running >= rerank {
			cutoff = d
			break
		}
	}

	// Stage two: exact cosine, but only over the survivors.
	hits := make([]Hit, 0, rerank+int(counts[cutoff]))
	for i, d := range dists {
		if int(d) <= cutoff {
			hits = append(hits, Hit{Doc: i, Score: dot(q, v.vecs[i])})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Doc < hits[j].Doc
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

// Bytes reports the in-memory footprint, split by stage, so the trade-off
// above is inspectable rather than asserted.
func (v *Vectors) Bytes() (binary, full int) {
	if v == nil {
		return 0, 0
	}
	for _, c := range v.codes {
		binary += len(c) * 8
	}
	for _, f := range v.vecs {
		full += len(f) * 4
	}
	return binary, full
}

// RRF fuses ranked lists by Reciprocal Rank Fusion.
//
// Fusing on rank rather than score is the point: BM25 scores and cosine
// similarities live on incompatible scales, and any attempt to normalise them
// into a weighted sum needs per-corpus tuning that then silently rots. Rank
// fusion needs none, and reliably beats either list alone.
func RRF(lists [][]Hit, k float64, limit int) []Hit {
	if k <= 0 {
		k = 60 // the value from the original RRF paper; robust in practice
	}
	scores := map[int]float64{}
	for _, list := range lists {
		for rank, h := range list {
			scores[h.Doc] += 1 / (k + float64(rank+1))
		}
	}
	out := make([]Hit, 0, len(scores))
	for doc, s := range scores {
		out = append(out, Hit{Doc: doc, Score: s})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Doc < out[j].Doc
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}
