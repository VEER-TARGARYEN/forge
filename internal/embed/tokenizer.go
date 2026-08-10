package embed

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"unicode"
)

// Tokenizer is BERT WordPiece.
//
// Two stages, and the split between them matters: basic tokenization decides
// what a "word" is (whitespace, punctuation, CJK, accents, case), and
// WordPiece then breaks each word into known subwords. Getting stage one wrong
// produces subtly different token ids than the reference implementation, and
// the resulting embeddings are quietly worse rather than obviously broken.
type Tokenizer struct {
	vocab    map[string]int32
	ids      []string
	lower    bool
	unkID    int32
	clsID    int32
	sepID    int32
	padID    int32
	maxChars int
}

// LoadVocab reads a BERT vocab.txt: one token per line, index is line number.
func LoadVocab(path string, lowercase bool) (*Tokenizer, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	t := &Tokenizer{vocab: map[string]int32{}, lower: lowercase, maxChars: 100}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		// Only the trailing newline is stripped: some vocabularies contain a
		// token that is literally a space.
		tok := strings.TrimRight(sc.Text(), "\r\n")
		t.vocab[tok] = int32(len(t.ids))
		t.ids = append(t.ids, tok)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(t.ids) == 0 {
		return nil, fmt.Errorf("%s: empty vocab", path)
	}
	for _, spec := range []struct {
		tok string
		dst *int32
	}{
		{"[UNK]", &t.unkID}, {"[CLS]", &t.clsID}, {"[SEP]", &t.sepID}, {"[PAD]", &t.padID},
	} {
		id, ok := t.vocab[spec.tok]
		if !ok {
			return nil, fmt.Errorf("%s: vocab is missing %s", path, spec.tok)
		}
		*spec.dst = id
	}
	return t, nil
}

// NewTokenizer builds one from an in-memory vocabulary, for tests.
func NewTokenizer(tokens []string, lowercase bool) (*Tokenizer, error) {
	t := &Tokenizer{vocab: map[string]int32{}, lower: lowercase, maxChars: 100}
	for _, tok := range tokens {
		t.vocab[tok] = int32(len(t.ids))
		t.ids = append(t.ids, tok)
	}
	for _, spec := range []struct {
		tok string
		dst *int32
	}{
		{"[UNK]", &t.unkID}, {"[CLS]", &t.clsID}, {"[SEP]", &t.sepID}, {"[PAD]", &t.padID},
	} {
		id, ok := t.vocab[spec.tok]
		if !ok {
			return nil, fmt.Errorf("vocab is missing %s", spec.tok)
		}
		*spec.dst = id
	}
	return t, nil
}

func (t *Tokenizer) VocabSize() int { return len(t.ids) }
func (t *Tokenizer) PadID() int32   { return t.padID }

// Encode produces input ids with [CLS] and [SEP], truncated to maxLen.
func (t *Tokenizer) Encode(text string, maxLen int) []int32 {
	if maxLen < 2 {
		maxLen = 2
	}
	pieces := t.wordpiece(t.basicTokenize(text))

	// Reserve two slots for the special tokens rather than truncating them
	// away: a sequence missing [SEP] embeds differently.
	if len(pieces) > maxLen-2 {
		pieces = pieces[:maxLen-2]
	}
	out := make([]int32, 0, len(pieces)+2)
	out = append(out, t.clsID)
	out = append(out, pieces...)
	out = append(out, t.sepID)
	return out
}

// Decode is for diagnostics: it turns ids back into readable text.
func (t *Tokenizer) Decode(ids []int32) string {
	var b strings.Builder
	for _, id := range ids {
		if int(id) >= len(t.ids) {
			continue
		}
		tok := t.ids[id]
		if strings.HasPrefix(tok, "##") {
			b.WriteString(tok[2:])
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(tok)
	}
	return b.String()
}

// basicTokenize splits into words: whitespace, punctuation as its own token,
// and every CJK ideograph standing alone.
func (t *Tokenizer) basicTokenize(text string) []string {
	var out []string
	var cur strings.Builder

	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range text {
		if t.lower {
			r = unicode.ToLower(r)
			// BERT's do_lower_case also strips accents, so "café" and "cafe"
			// reach the same token. Reference BERT does this by running NFD
			// and dropping combining marks; both forms are handled here —
			// precomposed letters through the fold table, already-decomposed
			// input through the Mn check below.
			if base, ok := latinFold[r]; ok {
				r = base
			}
			if isAccent(r) {
				continue
			}
		}
		switch {
		case r == 0 || r == 0xfffd:
			continue
		case unicode.IsSpace(r):
			flush()
		case isPunct(r):
			flush()
			out = append(out, string(r))
		case isCJK(r):
			flush()
			out = append(out, string(r))
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// wordpiece applies greedy longest-match-first within each word.
func (t *Tokenizer) wordpiece(words []string) []int32 {
	var out []int32
	for _, word := range words {
		runes := []rune(word)
		if len(runes) > t.maxChars {
			out = append(out, t.unkID)
			continue
		}

		start := 0
		var pieces []int32
		bad := false
		for start < len(runes) {
			end := len(runes)
			matched := int32(-1)
			for end > start {
				sub := string(runes[start:end])
				if start > 0 {
					sub = "##" + sub
				}
				if id, ok := t.vocab[sub]; ok {
					matched = id
					break
				}
				end--
			}
			if matched < 0 {
				// No prefix of the remainder is in the vocabulary, so the
				// whole word becomes [UNK] — partial coverage would produce
				// tokens the model never saw in training.
				bad = true
				break
			}
			pieces = append(pieces, matched)
			start = end
		}
		if bad {
			out = append(out, t.unkID)
			continue
		}
		out = append(out, pieces...)
	}
	return out
}

func isPunct(r rune) bool {
	// BERT treats every ASCII non-alphanumeric as punctuation, which is wider
	// than Unicode's category, plus the Unicode punctuation categories.
	if (r >= '!' && r <= '/') || (r >= ':' && r <= '@') ||
		(r >= '[' && r <= '`') || (r >= '{' && r <= '~') {
		return true
	}
	return unicode.IsPunct(r)
}

func isCJK(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) ||
		(r >= 0x3400 && r <= 0x4DBF) ||
		(r >= 0x20000 && r <= 0x2A6DF) ||
		(r >= 0x2A700 && r <= 0x2B73F) ||
		(r >= 0x2B740 && r <= 0x2B81F) ||
		(r >= 0x2B820 && r <= 0x2CEAF) ||
		(r >= 0xF900 && r <= 0xFAFF) ||
		(r >= 0x2F800 && r <= 0x2FA1F)
}

func isAccent(r rune) bool { return unicode.Is(unicode.Mn, r) }

// latinFold maps precomposed accented Latin letters to their base form.
//
// This is a deliberate substitute for full Unicode NFD normalisation. Shipping
// the decomposition tables would be a large dependency for a case that, in
// practice, is Latin-1 and Latin Extended-A: the accented characters that
// appear in identifiers, comments, and English-adjacent prose. Other scripts
// pass through untouched, which is also what a monolingual model's vocabulary
// expects.
var latinFold = func() map[rune]rune {
	m := map[rune]rune{}
	add := func(base rune, accented string) {
		for _, r := range accented {
			m[r] = base
		}
	}
	add('a', "àáâãäåāăą")
	add('c', "çćĉċč")
	add('d', "ďđ")
	add('e', "èéêëēĕėęě")
	add('g', "ĝğġģ")
	add('h', "ĥħ")
	add('i', "ìíîïĩīĭįı")
	add('j', "ĵ")
	add('k', "ķ")
	add('l', "ĺļľŀł")
	add('n', "ñńņňŉ")
	add('o', "òóôõöøōŏő")
	add('r', "ŕŗř")
	add('s', "śŝşš")
	add('t', "ţťŧ")
	add('u', "ùúûüũūŭůűų")
	add('w', "ŵ")
	add('y', "ýÿŷ")
	add('z', "źżž")
	// Uppercase too, for the do_lower_case=false path where ToLower does not
	// run first.
	add('A', "ÀÁÂÃÄÅĀĂĄ")
	add('C', "ÇĆĈĊČ")
	add('D', "ĎĐ")
	add('E', "ÈÉÊËĒĔĖĘĚ")
	add('G', "ĜĞĠĢ")
	add('H', "ĤĦ")
	add('I', "ÌÍÎÏĨĪĬĮİ")
	add('J', "Ĵ")
	add('K', "Ķ")
	add('L', "ĹĻĽĿŁ")
	add('N', "ÑŃŅŇ")
	add('O', "ÒÓÔÕÖØŌŎŐ")
	add('R', "ŔŖŘ")
	add('S', "ŚŜŞŠ")
	add('T', "ŢŤŦ")
	add('U', "ÙÚÛÜŨŪŬŮŰŲ")
	add('W', "Ŵ")
	add('Y', "ÝŶŸ")
	add('Z', "ŹŻŽ")
	return m
}()
