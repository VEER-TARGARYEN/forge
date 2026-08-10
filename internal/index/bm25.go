package index

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// Tokenize turns source text into search terms.
//
// The identifier splitting is what makes BM25 work on code at all. A query for
// "parse arguments" must match a function named parseArgs, and a query for
// "http server" must match HTTPServer. Indexing raw whitespace-delimited words
// gets neither, which is why naive full-text search feels useless on code.
func Tokenize(s string) []string {
	var out []string
	var cur strings.Builder

	flush := func() {
		if cur.Len() == 0 {
			return
		}
		word := cur.String()
		cur.Reset()
		lower := strings.ToLower(word)
		out = append(out, lower)
		// Also index the parts, so a query using any one of them hits.
		if parts := splitIdentifier(word); len(parts) > 1 {
			for _, p := range parts {
				if len(p) >= 2 {
					out = append(out, strings.ToLower(p))
				}
			}
		}
	}

	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

// splitIdentifier breaks camelCase, PascalCase, snake_case, and SCREAMING_CASE
// into their parts. HTTPServer yields [HTTP, Server], not [H, T, T, P, Server].
func splitIdentifier(s string) []string {
	var parts []string
	var cur []rune
	runes := []rune(s)

	for i, r := range runes {
		switch {
		case r == '_' || r == '-':
			if len(cur) > 0 {
				parts = append(parts, string(cur))
				cur = nil
			}
			continue
		case i > 0 && unicode.IsUpper(r):
			prev := runes[i-1]
			// Boundary at lower→upper (parseArgs) and at the end of an
			// acronym run (HTTPServer → HTTP | Server).
			nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
				if len(cur) > 0 {
					parts = append(parts, string(cur))
					cur = nil
				}
			}
		}
		cur = append(cur, r)
	}
	if len(cur) > 0 {
		parts = append(parts, string(cur))
	}
	return parts
}

type posting struct {
	Doc int `json:"d"`
	TF  int `json:"f"`
}

// BM25 is a standard Okapi BM25 inverted index.
//
// Roughly 200 lines replaces a full-text search engine here. The corpus is one
// repository — tens of thousands of chunks — where an in-memory postings list
// is both faster and simpler than anything with a server attached.
type BM25 struct {
	Postings map[string][]posting `json:"postings"`
	DocLen   []int                `json:"doclen"`
	AvgLen   float64              `json:"avglen"`
	N        int                  `json:"n"`
}

const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

// BuildBM25 indexes a corpus of documents, one per chunk.
func BuildBM25(docs []string) *BM25 {
	b := &BM25{Postings: map[string][]posting{}, DocLen: make([]int, len(docs)), N: len(docs)}
	total := 0
	for i, d := range docs {
		terms := Tokenize(d)
		b.DocLen[i] = len(terms)
		total += len(terms)

		tf := map[string]int{}
		for _, t := range terms {
			tf[t]++
		}
		for t, f := range tf {
			b.Postings[t] = append(b.Postings[t], posting{Doc: i, TF: f})
		}
	}
	if len(docs) > 0 {
		b.AvgLen = float64(total) / float64(len(docs))
	}
	if b.AvgLen == 0 {
		b.AvgLen = 1
	}
	return b
}

// Hit is one scored result.
type Hit struct {
	Doc   int
	Score float64
}

// Search scores documents against a query and returns the best `limit`.
func (b *BM25) Search(query string, limit int) []Hit {
	if b == nil || b.N == 0 {
		return nil
	}
	terms := Tokenize(query)
	if len(terms) == 0 {
		return nil
	}
	// A term repeated in the query should not be counted twice.
	seen := map[string]bool{}
	scores := map[int]float64{}

	for _, t := range terms {
		if seen[t] {
			continue
		}
		seen[t] = true
		posts := b.Postings[t]
		if len(posts) == 0 {
			continue
		}
		df := float64(len(posts))
		// Probabilistic IDF with the +1 guard, so a term appearing in more
		// than half the corpus scores near zero instead of going negative.
		idf := math.Log(1 + (float64(b.N)-df+0.5)/(df+0.5))
		for _, p := range posts {
			dl := float64(b.DocLen[p.Doc])
			tf := float64(p.TF)
			norm := tf + bm25K1*(1-bm25B+bm25B*dl/b.AvgLen)
			scores[p.Doc] += idf * tf * (bm25K1 + 1) / norm
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
