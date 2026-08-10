package embed

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Embedder is the public surface: text in, unit vectors out.
type Embedder struct {
	model  *Model
	tok    *Tokenizer
	maxLen int
	source string
	// concurrency bounds simultaneous sequences. The matmuls are already
	// parallel internally, so running many sequences at once mostly adds
	// cache pressure; a small number keeps every core busy through the
	// serial parts without thrashing.
	concurrency int
}

type Options struct {
	// MaxTokens truncates input. 256 covers a code chunk comfortably and
	// keeps attention, which is quadratic in sequence length, affordable.
	MaxTokens   int
	Concurrency int
	// Lowercase overrides the tokenizer_config setting when non-nil.
	Lowercase *bool
}

// Load reads a model directory: config.json, vocab.txt, and a safetensors
// file. That is the layout HuggingFace publishes, so a downloaded model works
// as-is with no conversion step.
func Load(dir string, opts Options) (*Embedder, error) {
	cfgPath := filepath.Join(dir, "config.json")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config.json: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config.json: %w", err)
	}

	lower := true
	if tc, err := os.ReadFile(filepath.Join(dir, "tokenizer_config.json")); err == nil {
		var t struct {
			DoLowerCase *bool `json:"do_lower_case"`
		}
		if json.Unmarshal(tc, &t) == nil && t.DoLowerCase != nil {
			lower = *t.DoLowerCase
		}
	}
	if opts.Lowercase != nil {
		lower = *opts.Lowercase
	}

	tok, err := LoadVocab(filepath.Join(dir, "vocab.txt"), lower)
	if err != nil {
		return nil, fmt.Errorf("load vocab: %w", err)
	}
	if tok.VocabSize() != cfg.VocabSize {
		// Not fatal — some checkpoints pad the embedding matrix past the
		// vocabulary — but a large mismatch means the wrong files were paired.
		if tok.VocabSize() > cfg.VocabSize {
			return nil, fmt.Errorf("vocab.txt has %d tokens but config says %d",
				tok.VocabSize(), cfg.VocabSize)
		}
	}

	stPath, err := findSafetensors(dir)
	if err != nil {
		return nil, err
	}
	w, err := LoadSafetensors(stPath)
	if err != nil {
		return nil, err
	}
	model, err := buildModel(cfg, w)
	if err != nil {
		return nil, err
	}

	maxLen := opts.MaxTokens
	if maxLen <= 0 {
		maxLen = 256
	}
	if maxLen > cfg.MaxPosition {
		maxLen = cfg.MaxPosition
	}
	conc := opts.Concurrency
	if conc <= 0 {
		conc = 2
	}
	return &Embedder{model: model, tok: tok, maxLen: maxLen, concurrency: conc, source: dir}, nil
}

// Source is the directory this model was loaded from, used to label the index
// so a later run can tell which model produced its vectors.
func (e *Embedder) Source() string { return e.source }

func findSafetensors(dir string) (string, error) {
	for _, name := range []string{"model.safetensors", "pytorch_model.safetensors"} {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*.safetensors"))
	if len(matches) > 0 {
		return matches[0], nil
	}
	return "", fmt.Errorf("no .safetensors file in %s\n"+
		"a HuggingFace model directory is expected: config.json, vocab.txt, model.safetensors", dir)
}

func (e *Embedder) Dim() int       { return e.model.Cfg.HiddenSize }
func (e *Embedder) MaxTokens() int { return e.maxLen }

// Describe summarises the loaded model for the CLI.
func (e *Embedder) Describe() string {
	c := e.model.Cfg
	return fmt.Sprintf("%d layers, %d dims, %d heads, vocab %d, max %d tokens",
		c.NumLayers, c.HiddenSize, c.NumHeads, c.VocabSize, e.maxLen)
}

// Embed vectorises a batch. Order is preserved, and an error anywhere fails
// the whole batch rather than returning a short slice the caller would silently
// misalign against its inputs.
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	errs := make([]error, len(texts))

	sem := make(chan struct{}, e.concurrency)
	var wg sync.WaitGroup
	for i, text := range texts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		wg.Add(1)
		go func(i int, text string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			ids := e.tok.Encode(text, e.maxLen)
			vec, err := e.model.Forward(ids)
			if err != nil {
				errs[i] = fmt.Errorf("input %d: %w", i, err)
				return
			}
			out[i] = vec
		}(i, text)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ---------- weight binding ----------

// prefixes are the layouts published models actually use. A sentence-
// transformers export and a plain BERT export differ only by this prefix, so
// trying each is cheaper than making the user rename tensors.
var prefixes = []string{"", "bert.", "encoder.", "0_Transformer.auto_model.", "model."}

func buildModel(cfg Config, w *Weights) (*Model, error) {
	prefix, err := detectPrefix(w)
	if err != nil {
		return nil, err
	}
	get := func(name string) (*Tensor, error) {
		full := prefix + name
		t, ok := w.Get(full)
		if !ok {
			// Distinguish "not in the file" from "present but undecodable":
			// the first means the wrong checkpoint, the second means a dtype
			// this loader does not handle, and the fixes are different.
			if dtype, skipped := w.SkippedDType(full); skipped {
				return nil, fmt.Errorf("tensor %q has dtype %s, which this loader cannot read", full, dtype)
			}
			return nil, fmt.Errorf("missing tensor %q", full)
		}
		return t, nil
	}

	m := &Model{Cfg: cfg}

	we, err := get("embeddings.word_embeddings.weight")
	if err != nil {
		return nil, err
	}
	if we.Cols() != cfg.HiddenSize {
		return nil, fmt.Errorf("word embeddings are %d wide, config says hidden_size %d",
			we.Cols(), cfg.HiddenSize)
	}
	// Trust the checkpoint over the config: some are exported with a padded
	// embedding matrix, and indexing past the real row count would read
	// another tensor's memory.
	m.Cfg.VocabSize = we.Rows()
	m.wordEmb = we.Data

	pe, err := get("embeddings.position_embeddings.weight")
	if err != nil {
		return nil, err
	}
	m.posEmb = pe.Data
	if pe.Rows() < cfg.MaxPosition {
		m.Cfg.MaxPosition = pe.Rows()
	}

	if te, err := get("embeddings.token_type_embeddings.weight"); err == nil {
		m.typeEmb = te.Data
	}

	if m.embNorm, err = getNorm(get, "embeddings.LayerNorm"); err != nil {
		return nil, err
	}

	m.blocks = make([]block, cfg.NumLayers)
	for i := 0; i < cfg.NumLayers; i++ {
		base := fmt.Sprintf("encoder.layer.%d.", i)
		b := &m.blocks[i]

		for _, spec := range []struct {
			name string
			dst  *linear
			in   int
			out  int
		}{
			{base + "attention.self.query", &b.query, cfg.HiddenSize, cfg.HiddenSize},
			{base + "attention.self.key", &b.key, cfg.HiddenSize, cfg.HiddenSize},
			{base + "attention.self.value", &b.value, cfg.HiddenSize, cfg.HiddenSize},
			{base + "attention.output.dense", &b.attnOut, cfg.HiddenSize, cfg.HiddenSize},
			{base + "intermediate.dense", &b.inter, cfg.HiddenSize, cfg.IntermediateSize},
			{base + "output.dense", &b.outer, cfg.IntermediateSize, cfg.HiddenSize},
		} {
			l, err := getLinear(get, spec.name, spec.in, spec.out)
			if err != nil {
				return nil, err
			}
			*spec.dst = l
		}
		if b.attnNorm, err = getNorm(get, base+"attention.output.LayerNorm"); err != nil {
			return nil, err
		}
		if b.outNorm, err = getNorm(get, base+"output.LayerNorm"); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// detectPrefix finds which naming layout this checkpoint uses.
func detectPrefix(w *Weights) (string, error) {
	for _, p := range prefixes {
		if _, ok := w.Get(p + "embeddings.word_embeddings.weight"); ok {
			return p, nil
		}
	}
	names := w.Names()
	if len(names) > 8 {
		names = names[:8]
	}
	return "", fmt.Errorf("could not find embeddings.word_embeddings.weight under any known prefix; "+
		"first tensors are %v", names)
}

func getLinear(get func(string) (*Tensor, error), name string, in, out int) (linear, error) {
	wt, err := get(name + ".weight")
	if err != nil {
		return linear{}, err
	}
	// nn.Linear stores [out, in]. A mismatch here almost always means the
	// config and the checkpoint disagree, which is worth saying plainly rather
	// than letting it surface as garbage vectors.
	if wt.Rows() != out || wt.Cols() != in {
		return linear{}, fmt.Errorf("%s.weight is %v, expected [%d %d]", name, wt.Shape, out, in)
	}
	l := linear{w: wt.Data, in: in, out: out}
	if bt, err := get(name + ".bias"); err == nil {
		if len(bt.Data) != out {
			return linear{}, fmt.Errorf("%s.bias has %d values, expected %d", name, len(bt.Data), out)
		}
		l.b = bt.Data
	}
	return l, nil
}

func getNorm(get func(string) (*Tensor, error), name string) (layerNorm, error) {
	var ln layerNorm
	g, err := get(name + ".weight")
	if err != nil {
		// Some exports spell these gamma/beta.
		if g2, err2 := get(name + ".gamma"); err2 == nil {
			g = g2
		} else {
			return ln, err
		}
	}
	ln.gamma = g.Data
	b, err := get(name + ".bias")
	if err != nil {
		if b2, err2 := get(name + ".beta"); err2 == nil {
			b = b2
		} else {
			return ln, nil // a norm with no bias is valid
		}
	}
	ln.beta = b.Data
	return ln, nil
}

// Discover finds a usable model directory without configuration.
//
// It looks under <stateDir>/models for anything with the right files, so a
// model downloaded once is picked up automatically by every later command.
// Making the user repeat a path they have already committed to is the kind of
// friction that turns an available feature into an unused one.
func Discover(stateDir string) (string, bool) {
	root := filepath.Join(stateDir, "models")
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", false
	}
	// Deterministic: the same directory is chosen on every run.
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		dir := filepath.Join(root, name)
		if LooksLikeModelDir(dir) == nil {
			return dir, true
		}
	}
	return "", false
}

// LooksLikeModelDir reports whether a directory has the files Load needs, so
// the CLI can give a precise error instead of a parse failure.
func LooksLikeModelDir(dir string) error {
	for _, name := range []string{"config.json", "vocab.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("%s is missing from %s", name, dir)
		}
	}
	if _, err := findSafetensors(dir); err != nil {
		return err
	}
	return nil
}

// ModelHint is the download instruction shown when no model is configured.
const ModelHint = `A local embedding model needs a HuggingFace model directory containing
config.json, vocab.txt, and model.safetensors. all-MiniLM-L6-v2 is a good
default: 6 layers, 384 dims, about 90 MB.

  git clone https://huggingface.co/sentence-transformers/all-MiniLM-L6-v2

Then pass -embed-model <dir>.`
