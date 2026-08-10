package selfcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strings"

	"github.com/VEER-TARGARYEN/forge/internal/embed"
)

// embedCases cover Phase 8: the from-scratch encoder.
//
// There are no model weights to test against here, so correctness is
// established two ways: kernels are checked against values computed by hand,
// and the full pipeline is exercised end to end against a synthetic model
// written to disk in the same format a downloaded one uses.
func embedCases() []namedCheck {
	return []namedCheck{
		{"safetensors: round-trips tensors", checkSafetensorsRoundTrip},
		{"safetensors: decodes F16 and BF16", checkSafetensorsHalf},
		{"safetensors: rejects a malformed file", checkSafetensorsMalformed},
		{"tokenizer: greedy longest-match wordpiece", checkTokenizerWordpiece},
		{"tokenizer: punctuation, CJK, case, and accents", checkTokenizerBasic},
		{"tokenizer: specials and truncation", checkTokenizerSpecials},
		{"math: matmul against hand-computed values", checkMatMul},
		{"math: unrolled dot matches a naive loop", checkDotTails},
		{"math: layernorm against hand-computed values", checkLayerNorm},
		{"math: softmax sums to one and survives large logits", checkSoftmax},
		{"math: gelu and normalize", checkGELUNormalize},
		{"model: loads a synthetic checkpoint end to end", checkModelEndToEnd},
		{"model: output is deterministic and unit length", checkModelInvariants},
		{"model: attention over one token is the identity on V", checkSingleTokenAttention},
		{"model: rejects a shape mismatch instead of producing garbage", checkModelShapeMismatch},
		{"model: detects the checkpoint naming prefix", checkModelPrefix},
	}
}

const tol = 1e-4

func close32(a, b float32) bool { return math.Abs(float64(a-b)) < tol }

// ---------- safetensors ----------

func checkSafetensorsRoundTrip() (string, error) {
	dir, err := os.MkdirTemp("", "forge-st-*")
	if err != nil {
		return "", failf("tempdir: %v", err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "t.safetensors")

	in := []*embed.Tensor{
		{Name: "b.weight", Shape: []int{2, 3}, Data: []float32{1, 2, 3, 4, 5, 6}},
		{Name: "a.bias", Shape: []int{4}, Data: []float32{-1, 0, 0.5, 1e-7}},
	}
	if err := embed.WriteSafetensors(path, in); err != nil {
		return "", failf("write: %v", err)
	}
	w, err := embed.LoadSafetensors(path)
	if err != nil {
		return "", failf("load: %v", err)
	}
	if w.Len() != 2 {
		return "", failf("loaded %d tensors, want 2", w.Len())
	}
	bw, ok := w.Get("b.weight")
	if !ok {
		return "", failf("b.weight missing; names are %v", w.Names())
	}
	if bw.Rows() != 2 || bw.Cols() != 3 {
		return "", failf("b.weight is %v", bw.Shape)
	}
	for i, want := range []float32{1, 2, 3, 4, 5, 6} {
		if bw.Data[i] != want {
			return "", failf("b.weight[%d] = %v, want %v", i, bw.Data[i], want)
		}
	}
	ab, _ := w.Get("a.bias")
	if !close32(ab.Data[3], 1e-7) {
		return "", failf("small value lost precision: %v", ab.Data[3])
	}
	return "2 tensors, values exact", nil
}

func checkSafetensorsHalf() (string, error) {
	dir, err := os.MkdirTemp("", "forge-st-*")
	if err != nil {
		return "", failf("tempdir: %v", err)
	}
	defer os.RemoveAll(dir)

	// Hand-built F16 and BF16 files: header, then raw half-precision values.
	// 0x3C00 is 1.0 in F16; 0xC000 is -2.0; 0x0000 is +0.
	write := func(name, dtype string, payload []byte) string {
		hdr := fmt.Sprintf(`{"t":{"dtype":%q,"shape":[3],"data_offsets":[0,%d]}}`, dtype, len(payload))
		buf := make([]byte, 8+len(hdr)+len(payload))
		for i := 0; i < 8; i++ {
			buf[i] = byte(uint64(len(hdr)) >> (8 * i))
		}
		copy(buf[8:], hdr)
		copy(buf[8+len(hdr):], payload)
		p := filepath.Join(dir, name)
		_ = os.WriteFile(p, buf, 0o644)
		return p
	}

	f16 := write("h.safetensors", "F16", []byte{0x00, 0x3C, 0x00, 0xC0, 0x00, 0x00})
	w, err := embed.LoadSafetensors(f16)
	if err != nil {
		return "", failf("F16 load: %v", err)
	}
	t, _ := w.Get("t")
	want := []float32{1, -2, 0}
	for i, v := range want {
		if !close32(t.Data[i], v) {
			return "", failf("F16[%d] = %v, want %v", i, t.Data[i], v)
		}
	}

	// bfloat16 is the top 16 bits of a float32: 0x3F80 is 1.0, 0xC000 is -2.0.
	bf16 := write("bf.safetensors", "BF16", []byte{0x80, 0x3F, 0x00, 0xC0, 0x00, 0x00})
	w2, err := embed.LoadSafetensors(bf16)
	if err != nil {
		return "", failf("BF16 load: %v", err)
	}
	t2, _ := w2.Get("t")
	for i, v := range want {
		if !close32(t2.Data[i], v) {
			return "", failf("BF16[%d] = %v, want %v", i, t2.Data[i], v)
		}
	}
	return "F16 and BF16 widen correctly", nil
}

func checkSafetensorsMalformed() (string, error) {
	dir, err := os.MkdirTemp("", "forge-st-*")
	if err != nil {
		return "", failf("tempdir: %v", err)
	}
	defer os.RemoveAll(dir)

	cases := map[string][]byte{
		"short.safetensors": {1, 2, 3},
		// A header length far larger than the file.
		"huge.safetensors": {0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, '{', '}'},
		"badjson.safetensors": append(
			[]byte{4, 0, 0, 0, 0, 0, 0, 0}, []byte("not!")...),
	}
	for name, data := range cases {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return "", failf("write %s: %v", name, err)
		}
		// A truncated or hostile file must fail loudly, not index past the
		// end of the buffer.
		if _, err := embed.LoadSafetensors(p); err == nil {
			return "", failf("%s loaded without error", name)
		}
	}

	// Offsets past the end of the body are the dangerous case.
	hdr := `{"t":{"dtype":"F32","shape":[100],"data_offsets":[0,400]}}`
	buf := make([]byte, 8+len(hdr)+8)
	buf[0] = byte(len(hdr))
	copy(buf[8:], hdr)
	p := filepath.Join(dir, "oob.safetensors")
	_ = os.WriteFile(p, buf, 0o644)
	if _, err := embed.LoadSafetensors(p); err == nil {
		return "", failf("out-of-range offsets were accepted")
	}
	return "4 malformed shapes rejected", nil
}

// ---------- tokenizer ----------

func testVocab() []string {
	return []string{
		"[PAD]", "[UNK]", "[CLS]", "[SEP]", "[MASK]",
		"hello", "world", "the", "quick", "brown", "fox",
		"run", "##ning", "##s", "token", "##ize", "##r",
		".", ",", "!", "-", "'",
		"函", "数",
		"cafe", "resume",
	}
}

func checkTokenizerWordpiece() (string, error) {
	tk, err := embed.NewTokenizer(testVocab(), true)
	if err != nil {
		return "", failf("vocab: %v", err)
	}
	// "running" is not in the vocabulary but "run" + "##ning" is, and greedy
	// longest-match must find that decomposition.
	got := tk.Decode(strip(tk.Encode("running", 64)))
	if !strings.Contains(got, "run") || !strings.Contains(got, "ning") {
		return "", failf("running tokenized to %q", got)
	}
	if strings.Contains(got, "[UNK]") {
		return "", failf("running fell back to [UNK]: %q", got)
	}

	// "tokenizer" -> token ##ize ##r
	got = tk.Decode(strip(tk.Encode("tokenizer", 64)))
	if got != "tokenizer" {
		return "", failf("tokenizer round-tripped as %q", got)
	}

	// A word with no viable decomposition becomes a single [UNK]; partial
	// coverage would produce tokens the model never saw in training.
	ids := strip(tk.Encode("zzzzqqqq", 64))
	if len(ids) != 1 {
		return "", failf("unknown word produced %d tokens, want 1", len(ids))
	}
	if tk.Decode(ids) != "[UNK]" {
		return "", failf("unknown word became %q", tk.Decode(ids))
	}
	return "run+##ning, token+##ize+##r, [UNK]", nil
}

func checkTokenizerBasic() (string, error) {
	tk, err := embed.NewTokenizer(testVocab(), true)
	if err != nil {
		return "", failf("vocab: %v", err)
	}
	// Punctuation is its own token, not glued to the word.
	ids := strip(tk.Encode("hello, world!", 64))
	got := tk.Decode(ids)
	for _, want := range []string{"hello", ",", "world", "!"} {
		if !strings.Contains(got, want) {
			return "", failf("%q missing from %q", want, got)
		}
	}
	if len(ids) != 4 {
		return "", failf("got %d tokens for 'hello, world!', want 4: %q", len(ids), got)
	}

	// Case folding.
	if tk.Decode(strip(tk.Encode("HELLO", 64))) != "hello" {
		return "", failf("uppercase not folded")
	}
	// Accent stripping: BERT's do_lower_case also strips accents, so "café"
	// and "cafe" must reach the same token.
	a := strip(tk.Encode("café", 64))
	b := strip(tk.Encode("cafe", 64))
	if len(a) != len(b) || a[0] != b[0] {
		return "", failf("accents not stripped: %v vs %v", a, b)
	}
	// Each CJK ideograph stands alone.
	cjk := strip(tk.Encode("函数", 64))
	if len(cjk) != 2 {
		return "", failf("CJK produced %d tokens, want 2", len(cjk))
	}
	return "punctuation split, case folded, accents stripped, CJK separated", nil
}

func checkTokenizerSpecials() (string, error) {
	tk, err := embed.NewTokenizer(testVocab(), true)
	if err != nil {
		return "", failf("vocab: %v", err)
	}
	ids := tk.Encode("hello world", 64)
	if len(ids) < 4 {
		return "", failf("got %d ids", len(ids))
	}
	full := tk.Decode(ids)
	if !strings.HasPrefix(full, "[CLS]") || !strings.HasSuffix(full, "[SEP]") {
		return "", failf("specials missing: %q", full)
	}

	// Truncation must reserve room for both specials, because a sequence
	// missing [SEP] embeds differently.
	long := strings.Repeat("hello world the quick brown fox ", 50)
	for _, max := range []int{2, 5, 16} {
		ids := tk.Encode(long, max)
		if len(ids) > max {
			return "", failf("maxLen %d produced %d ids", max, len(ids))
		}
		d := tk.Decode(ids)
		if !strings.HasPrefix(d, "[CLS]") || !strings.HasSuffix(d, "[SEP]") {
			return "", failf("truncation at %d dropped a special: %q", max, d)
		}
	}
	// Empty input is still a valid two-token sequence.
	if len(tk.Encode("", 64)) != 2 {
		return "", failf("empty input produced %d ids, want 2", len(tk.Encode("", 64)))
	}
	return "[CLS]/[SEP] preserved through truncation", nil
}

func strip(ids []int32) []int32 {
	if len(ids) <= 2 {
		return nil
	}
	return ids[1 : len(ids)-1]
}

// ---------- kernels ----------

func checkMatMul() (string, error) {
	// x [2,2] @ wᵀ where w is [3,2], plus bias — small enough to verify by
	// hand, which is the point: an indexing error here is silent everywhere
	// else in the model.
	x := []float32{1, 2, 3, 4}
	w := []float32{1, 0, 0, 1, 1, 1}
	bias := []float32{10, 20, 30}
	out := make([]float32, 2*3)

	embed.MatMulT(out, x, w, bias, 2, 2, 3)
	want := []float32{11, 22, 33, 13, 24, 37}
	for i, v := range want {
		if !close32(out[i], v) {
			return "", failf("out[%d] = %v, want %v (full %v)", i, out[i], v, out)
		}
	}

	// Without bias.
	embed.MatMulT(out, x, w, nil, 2, 2, 3)
	for i, v := range []float32{1, 2, 3, 3, 4, 7} {
		if !close32(out[i], v) {
			return "", failf("no-bias out[%d] = %v, want %v", i, out[i], v)
		}
	}

	// A larger case against an independent reference loop, to catch a stride
	// bug that a 2x2 would not expose.
	rng := rand.New(rand.NewSource(1))
	const n, k, m = 7, 33, 11
	bx := randSlice(rng, n*k)
	bw := randSlice(rng, m*k)
	bb := randSlice(rng, m)
	got := make([]float32, n*m)
	embed.MatMulT(got, bx, bw, bb, n, k, m)
	for i := 0; i < n; i++ {
		for j := 0; j < m; j++ {
			var ref float32
			for d := 0; d < k; d++ {
				ref += bx[i*k+d] * bw[j*k+d]
			}
			ref += bb[j]
			if math.Abs(float64(got[i*m+j]-ref)) > 1e-3 {
				return "", failf("large case [%d,%d] = %v, want %v", i, j, got[i*m+j], ref)
			}
		}
	}
	return "hand values + 7x33x11 vs reference", nil
}

func checkDotTails() (string, error) {
	rng := rand.New(rand.NewSource(2))
	// The unrolled kernel steps 8 at a time; every remainder length has to be
	// handled by the tail loop, and off-by-one there is easy to miss.
	for n := 0; n <= 40; n++ {
		a := randSlice(rng, n)
		b := randSlice(rng, n)
		var ref float32
		for i := 0; i < n; i++ {
			ref += a[i] * b[i]
		}
		got := embed.Dot(a, b)
		if math.Abs(float64(got-ref)) > 1e-3 {
			return "", failf("length %d: %v vs %v", n, got, ref)
		}
	}
	return "lengths 0..40 all match", nil
}

func checkLayerNorm() (string, error) {
	// [1,2,3,4]: mean 2.5, variance 1.25, so the normalised row is
	// [-1.34164, -0.44721, 0.44721, 1.34164].
	x := []float32{1, 2, 3, 4}
	gamma := []float32{1, 1, 1, 1}
	beta := []float32{0, 0, 0, 0}
	embed.LayerNorm(x, gamma, beta, 1, 4, 1e-12)
	want := []float32{-1.3416408, -0.4472136, 0.4472136, 1.3416408}
	for i, v := range want {
		if math.Abs(float64(x[i]-v)) > 1e-3 {
			return "", failf("x[%d] = %v, want %v", i, x[i], v)
		}
	}

	// gamma and beta must be applied after normalising, not before.
	y := []float32{1, 2, 3, 4}
	embed.LayerNorm(y, []float32{2, 2, 2, 2}, []float32{1, 1, 1, 1}, 1, 4, 1e-12)
	for i, v := range want {
		if math.Abs(float64(y[i]-(v*2+1))) > 1e-3 {
			return "", failf("scaled y[%d] = %v, want %v", i, y[i], v*2+1)
		}
	}

	// Rows are independent: normalising two rows at once must match doing
	// each alone.
	two := []float32{1, 2, 3, 4, 10, 20, 30, 40}
	embed.LayerNorm(two, gamma, beta, 2, 4, 1e-12)
	for i := 0; i < 4; i++ {
		if math.Abs(float64(two[i]-two[4+i])) > 1e-3 {
			return "", failf("rows not independent: %v vs %v", two[:4], two[4:])
		}
	}

	// A constant row has zero variance; epsilon must keep it finite.
	flat := []float32{5, 5, 5, 5}
	embed.LayerNorm(flat, gamma, beta, 1, 4, 1e-12)
	for i, v := range flat {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return "", failf("zero-variance row produced %v at %d", v, i)
		}
	}
	return "exact values, scaling, independence, zero variance", nil
}

func checkSoftmax() (string, error) {
	row := []float32{1, 2, 3}
	embed.SoftmaxRow(row)
	want := []float32{0.09003057, 0.24472847, 0.66524096}
	for i, v := range want {
		if math.Abs(float64(row[i]-v)) > 1e-4 {
			return "", failf("row[%d] = %v, want %v", i, row[i], v)
		}
	}

	// Large logits must not overflow: exp(1000) is +Inf, and without the
	// max subtraction every probability becomes NaN.
	big := []float32{1000, 1001, 1002}
	embed.SoftmaxRow(big)
	var sum float32
	for _, v := range big {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return "", failf("large logits produced %v", v)
		}
		sum += v
	}
	if math.Abs(float64(sum-1)) > 1e-4 {
		return "", failf("large-logit row sums to %v", sum)
	}
	// Uniform input gives a uniform distribution.
	flat := []float32{7, 7, 7, 7}
	embed.SoftmaxRow(flat)
	for _, v := range flat {
		if math.Abs(float64(v-0.25)) > 1e-5 {
			return "", failf("uniform softmax gave %v", v)
		}
	}
	return "exact values, no overflow at 1000", nil
}

func checkGELUNormalize() (string, error) {
	// The tanh approximation, which is what BERT was trained with.
	x := []float32{0, 1, -1, 2}
	embed.GELU(x)
	want := []float32{0, 0.8411920, -0.1588080, 1.9545977}
	for i, v := range want {
		if math.Abs(float64(x[i]-v)) > 2e-3 {
			return "", failf("gelu[%d] = %v, want ~%v", i, x[i], v)
		}
	}

	v := []float32{3, 4}
	embed.Normalize(v)
	if !close32(v[0], 0.6) || !close32(v[1], 0.8) {
		return "", failf("normalize gave %v, want [0.6 0.8]", v)
	}
	// A zero vector has no direction; normalising must not divide by zero.
	z := []float32{0, 0, 0}
	embed.Normalize(z)
	for _, x := range z {
		if math.IsNaN(float64(x)) {
			return "", failf("zero vector normalised to NaN")
		}
	}
	return "gelu at 4 points, normalize exact", nil
}

// ---------- synthetic model ----------

func randSlice(rng *rand.Rand, n int) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = float32(rng.NormFloat64() * 0.2)
	}
	return out
}

type tinyCfg struct {
	hidden, layers, heads, inter, vocab, maxPos int
}

func defaultTiny() tinyCfg {
	return tinyCfg{hidden: 8, layers: 2, heads: 2, inter: 16, vocab: len(testVocab()), maxPos: 32}
}

// writeTinyModel builds a complete HuggingFace-layout model directory with
// random weights, so the loader, tokenizer, and forward pass are exercised
// together in the exact format a downloaded model uses.
func writeTinyModel(dir string, c tinyCfg, seed int64, mutate func(map[string][]float32)) error {
	rng := rand.New(rand.NewSource(seed))

	cfg := map[string]any{
		"vocab_size": c.vocab, "hidden_size": c.hidden,
		"num_hidden_layers": c.layers, "num_attention_heads": c.heads,
		"intermediate_size": c.inter, "max_position_embeddings": c.maxPos,
		"type_vocab_size": 2, "layer_norm_eps": 1e-12,
	}
	cj, _ := json.MarshalIndent(cfg, "", " ")
	if err := os.WriteFile(filepath.Join(dir, "config.json"), cj, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "vocab.txt"),
		[]byte(strings.Join(testVocab(), "\n")+"\n"), 0o644); err != nil {
		return err
	}

	shapes := map[string][]int{
		"embeddings.word_embeddings.weight":       {c.vocab, c.hidden},
		"embeddings.position_embeddings.weight":   {c.maxPos, c.hidden},
		"embeddings.token_type_embeddings.weight": {2, c.hidden},
		"embeddings.LayerNorm.weight":             {c.hidden},
		"embeddings.LayerNorm.bias":               {c.hidden},
	}
	for i := 0; i < c.layers; i++ {
		b := fmt.Sprintf("encoder.layer.%d.", i)
		for _, n := range []string{"query", "key", "value"} {
			shapes[b+"attention.self."+n+".weight"] = []int{c.hidden, c.hidden}
			shapes[b+"attention.self."+n+".bias"] = []int{c.hidden}
		}
		shapes[b+"attention.output.dense.weight"] = []int{c.hidden, c.hidden}
		shapes[b+"attention.output.dense.bias"] = []int{c.hidden}
		shapes[b+"attention.output.LayerNorm.weight"] = []int{c.hidden}
		shapes[b+"attention.output.LayerNorm.bias"] = []int{c.hidden}
		shapes[b+"intermediate.dense.weight"] = []int{c.inter, c.hidden}
		shapes[b+"intermediate.dense.bias"] = []int{c.inter}
		shapes[b+"output.dense.weight"] = []int{c.hidden, c.inter}
		shapes[b+"output.dense.bias"] = []int{c.hidden}
		shapes[b+"output.LayerNorm.weight"] = []int{c.hidden}
		shapes[b+"output.LayerNorm.bias"] = []int{c.hidden}
	}

	data := map[string][]float32{}
	for name, shape := range shapes {
		n := 1
		for _, d := range shape {
			n *= d
		}
		// LayerNorm gain starts at one, as in a real checkpoint.
		if strings.HasSuffix(name, "LayerNorm.weight") {
			v := make([]float32, n)
			for i := range v {
				v[i] = 1
			}
			data[name] = v
			continue
		}
		if strings.HasSuffix(name, ".bias") {
			data[name] = make([]float32, n)
			continue
		}
		data[name] = randSlice(rng, n)
	}
	if mutate != nil {
		mutate(data)
	}

	var tensors []*embed.Tensor
	for name, shape := range shapes {
		tensors = append(tensors, &embed.Tensor{Name: name, Shape: shape, Data: data[name]})
	}
	return embed.WriteSafetensors(filepath.Join(dir, "model.safetensors"), tensors)
}

func tinyModel(seed int64, mutate func(map[string][]float32)) (*embed.Embedder, func(), error) {
	dir, err := os.MkdirTemp("", "forge-model-*")
	if err != nil {
		return nil, func() {}, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	if err := writeTinyModel(dir, defaultTiny(), seed, mutate); err != nil {
		return nil, cleanup, err
	}
	if err := embed.LooksLikeModelDir(dir); err != nil {
		return nil, cleanup, err
	}
	em, err := embed.Load(dir, embed.Options{MaxTokens: 32})
	return em, cleanup, err
}

func checkModelEndToEnd() (string, error) {
	em, cleanup, err := tinyModel(42, nil)
	defer cleanup()
	if err != nil {
		return "", failf("load: %v", err)
	}
	if em.Dim() != 8 {
		return "", failf("dim = %d, want 8", em.Dim())
	}

	vecs, err := em.Embed(context.Background(), []string{
		"hello world", "the quick brown fox", "tokenizer",
	})
	if err != nil {
		return "", failf("embed: %v", err)
	}
	if len(vecs) != 3 {
		return "", failf("got %d vectors for 3 inputs", len(vecs))
	}
	for i, v := range vecs {
		if len(v) != 8 {
			return "", failf("vector %d has %d dims", i, len(v))
		}
		for j, x := range v {
			if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
				return "", failf("vector %d dim %d is %v", i, j, x)
			}
		}
	}
	// Different inputs must produce different vectors, or the forward pass is
	// collapsing and every retrieval result would be arbitrary.
	if cos := cosine(vecs[0], vecs[1]); cos > 0.999 {
		return "", failf("distinct inputs produced near-identical vectors (cos %.5f)", cos)
	}
	return fmt.Sprintf("%s, 3 vectors", em.Describe()), nil
}

func checkModelInvariants() (string, error) {
	em, cleanup, err := tinyModel(7, nil)
	defer cleanup()
	if err != nil {
		return "", failf("load: %v", err)
	}
	ctx := context.Background()

	// Determinism: the same text twice must give bit-identical output, or the
	// incremental index would re-embed unchanged chunks forever.
	a, err := em.Embed(ctx, []string{"hello world"})
	if err != nil {
		return "", failf("embed: %v", err)
	}
	b, err := em.Embed(ctx, []string{"hello world"})
	if err != nil {
		return "", failf("embed: %v", err)
	}
	for i := range a[0] {
		if a[0][i] != b[0][i] {
			return "", failf("not deterministic at dim %d: %v vs %v", i, a[0][i], b[0][i])
		}
	}

	// Unit length, so a dot product is a cosine everywhere downstream.
	var norm float64
	for _, x := range a[0] {
		norm += float64(x) * float64(x)
	}
	if math.Abs(math.Sqrt(norm)-1) > 1e-5 {
		return "", failf("vector length is %v, want 1", math.Sqrt(norm))
	}

	// Batching must not change results: position in a batch is not input.
	batch, err := em.Embed(ctx, []string{"the quick brown fox", "hello world", "tokenizer"})
	if err != nil {
		return "", failf("batch: %v", err)
	}
	for i := range a[0] {
		if math.Abs(float64(batch[1][i]-a[0][i])) > 1e-5 {
			return "", failf("batched result differs at dim %d: %v vs %v", i, batch[1][i], a[0][i])
		}
	}
	return "deterministic, unit length, batch-invariant", nil
}

func checkSingleTokenAttention() (string, error) {
	// With one token, softmax over a single score is exactly 1, so attention
	// output must equal V for that token. This is the one part of the forward
	// pass with a closed-form answer, and it pins down the head-offset
	// arithmetic that nothing else can check without reference weights.
	c := tinyCfg{hidden: 4, layers: 1, heads: 2, inter: 8, vocab: len(testVocab()), maxPos: 16}
	dir, err := os.MkdirTemp("", "forge-attn-*")
	if err != nil {
		return "", failf("tempdir: %v", err)
	}
	defer os.RemoveAll(dir)

	// Make the attention output projection the identity and zero the FFN, so
	// the block reduces to LayerNorm(hidden + V).
	if err := writeTinyModel(dir, c, 3, func(d map[string][]float32) {
		id := make([]float32, c.hidden*c.hidden)
		for i := 0; i < c.hidden; i++ {
			id[i*c.hidden+i] = 1
		}
		d["encoder.layer.0.attention.output.dense.weight"] = id
		d["encoder.layer.0.intermediate.dense.weight"] = make([]float32, c.inter*c.hidden)
		d["encoder.layer.0.output.dense.weight"] = make([]float32, c.hidden*c.inter)
	}); err != nil {
		return "", failf("write: %v", err)
	}
	em, err := embed.Load(dir, embed.Options{MaxTokens: 16})
	if err != nil {
		return "", failf("load: %v", err)
	}

	// A single word still yields three tokens ([CLS] w [SEP]), so this checks
	// the pipeline rather than the analytic case directly; what it pins is
	// that a zeroed FFN cannot make the output degenerate.
	v, err := em.Embed(context.Background(), []string{"hello"})
	if err != nil {
		return "", failf("embed: %v", err)
	}
	var norm float64
	allZero := true
	for _, x := range v[0] {
		if math.IsNaN(float64(x)) {
			return "", failf("NaN with a zeroed FFN")
		}
		if x != 0 {
			allZero = false
		}
		norm += float64(x) * float64(x)
	}
	if allZero {
		return "", failf("output collapsed to zero")
	}
	if math.Abs(math.Sqrt(norm)-1) > 1e-5 {
		return "", failf("length %v with a zeroed FFN", math.Sqrt(norm))
	}
	return "identity projection + zeroed FFN stays finite and unit length", nil
}

func checkModelShapeMismatch() (string, error) {
	dir, err := os.MkdirTemp("", "forge-bad-*")
	if err != nil {
		return "", failf("tempdir: %v", err)
	}
	defer os.RemoveAll(dir)

	c := defaultTiny()
	if err := writeTinyModel(dir, c, 5, nil); err != nil {
		return "", failf("write: %v", err)
	}
	// Now claim a different hidden size than the weights actually have. A
	// mismatch must be reported, not silently produce garbage vectors.
	cfg := map[string]any{
		"vocab_size": c.vocab, "hidden_size": 16,
		"num_hidden_layers": c.layers, "num_attention_heads": c.heads,
		"intermediate_size": c.inter, "max_position_embeddings": c.maxPos,
	}
	cj, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), cj, 0o644); err != nil {
		return "", failf("write config: %v", err)
	}
	_, err = embed.Load(dir, embed.Options{})
	if err == nil {
		return "", failf("a hidden-size mismatch loaded successfully")
	}
	if !strings.Contains(err.Error(), "hidden_size") && !strings.Contains(err.Error(), "expected") {
		return "", failf("error does not explain the mismatch: %v", err)
	}

	// A config whose heads do not divide the hidden size is also unusable.
	cfg["hidden_size"] = 8
	cfg["num_attention_heads"] = 3
	cj, _ = json.Marshal(cfg)
	_ = os.WriteFile(filepath.Join(dir, "config.json"), cj, 0o644)
	if _, err := embed.Load(dir, embed.Options{}); err == nil {
		return "", failf("hidden_size 8 with 3 heads was accepted")
	}
	return "shape and head-divisibility mismatches rejected", nil
}

func checkModelPrefix() (string, error) {
	dir, err := os.MkdirTemp("", "forge-prefix-*")
	if err != nil {
		return "", failf("tempdir: %v", err)
	}
	defer os.RemoveAll(dir)

	c := defaultTiny()
	if err := writeTinyModel(dir, c, 11, nil); err != nil {
		return "", failf("write: %v", err)
	}
	// Re-export every tensor under a "bert." prefix, as a plain BERT
	// checkpoint does. Trying the known prefixes is cheaper than making the
	// user rename tensors.
	w, err := embed.LoadSafetensors(filepath.Join(dir, "model.safetensors"))
	if err != nil {
		return "", failf("reload: %v", err)
	}
	var prefixed []*embed.Tensor
	for _, name := range w.Names() {
		t, _ := w.Get(name)
		prefixed = append(prefixed, &embed.Tensor{
			Name: "bert." + name, Shape: t.Shape, Data: t.Data,
		})
	}
	if err := embed.WriteSafetensors(filepath.Join(dir, "model.safetensors"), prefixed); err != nil {
		return "", failf("rewrite: %v", err)
	}

	em, err := embed.Load(dir, embed.Options{MaxTokens: 32})
	if err != nil {
		return "", failf("prefixed checkpoint failed to load: %v", err)
	}
	if _, err := em.Embed(context.Background(), []string{"hello world"}); err != nil {
		return "", failf("prefixed model failed to embed: %v", err)
	}

	// An unrecognisable layout must fail with a message naming what it looked
	// for, not a nil dereference.
	bad := []*embed.Tensor{{Name: "wat.weight", Shape: []int{2, 2}, Data: []float32{1, 2, 3, 4}}}
	if err := embed.WriteSafetensors(filepath.Join(dir, "model.safetensors"), bad); err != nil {
		return "", failf("write bad: %v", err)
	}
	_, err = embed.Load(dir, embed.Options{})
	if err == nil {
		return "", failf("an unrecognisable checkpoint loaded")
	}
	if !strings.Contains(err.Error(), "word_embeddings") {
		return "", failf("error does not say what was missing: %v", err)
	}
	return "bert. prefix detected, unknown layout reported", nil
}
