package embed

import (
	"fmt"
	"math"
)

// Config mirrors the fields of a HuggingFace BERT config.json that actually
// affect the forward pass.
type Config struct {
	VocabSize        int     `json:"vocab_size"`
	HiddenSize       int     `json:"hidden_size"`
	NumLayers        int     `json:"num_hidden_layers"`
	NumHeads         int     `json:"num_attention_heads"`
	IntermediateSize int     `json:"intermediate_size"`
	MaxPosition      int     `json:"max_position_embeddings"`
	TypeVocabSize    int     `json:"type_vocab_size"`
	LayerNormEps     float32 `json:"layer_norm_eps"`
}

func (c *Config) applyDefaults() {
	if c.LayerNormEps == 0 {
		c.LayerNormEps = 1e-12
	}
	if c.TypeVocabSize == 0 {
		c.TypeVocabSize = 2
	}
	if c.MaxPosition == 0 {
		c.MaxPosition = 512
	}
}

func (c *Config) validate() error {
	switch {
	case c.HiddenSize <= 0:
		return fmt.Errorf("hidden_size must be positive")
	case c.NumHeads <= 0:
		return fmt.Errorf("num_attention_heads must be positive")
	case c.HiddenSize%c.NumHeads != 0:
		return fmt.Errorf("hidden_size %d is not divisible by num_attention_heads %d",
			c.HiddenSize, c.NumHeads)
	case c.NumLayers <= 0:
		return fmt.Errorf("num_hidden_layers must be positive")
	case c.IntermediateSize <= 0:
		return fmt.Errorf("intermediate_size must be positive")
	}
	return nil
}

// linear is a weight/bias pair in nn.Linear layout: weight is [out, in].
type linear struct {
	w   []float32
	b   []float32
	in  int
	out int
}

type layerNorm struct {
	gamma []float32
	beta  []float32
}

type block struct {
	query, key, value linear
	attnOut           linear
	attnNorm          layerNorm
	inter             linear
	outer             linear
	outNorm           layerNorm
}

// Model is a loaded encoder.
type Model struct {
	Cfg Config

	wordEmb []float32
	posEmb  []float32
	typeEmb []float32
	embNorm layerNorm

	blocks []block
}

// scratch holds the per-call working buffers.
//
// Allocated once per Encode rather than per layer: at 6 layers and a few
// hundred tokens the garbage from per-layer allocation dominates the actual
// arithmetic.
type scratch struct {
	hidden  []float32 // [T, H]
	q, k, v []float32
	ctx     []float32
	proj    []float32
	inter   []float32
	scores  []float32 // [T] per head-row, reused
}

func newScratch(t, h, inter int) *scratch {
	return &scratch{
		hidden: make([]float32, t*h),
		q:      make([]float32, t*h),
		k:      make([]float32, t*h),
		v:      make([]float32, t*h),
		ctx:    make([]float32, t*h),
		proj:   make([]float32, t*h),
		inter:  make([]float32, t*inter),
		scores: make([]float32, t),
	}
}

// Forward runs the encoder over one sequence and returns the mean-pooled,
// L2-normalised embedding.
//
// Mean pooling rather than the [CLS] vector: sentence-transformers models are
// trained with mean pooling, and using [CLS] on one of them produces a vector
// that is confidently wrong rather than obviously broken.
func (m *Model) Forward(ids []int32) ([]float32, error) {
	t := len(ids)
	if t == 0 {
		return nil, fmt.Errorf("empty input")
	}
	if t > m.Cfg.MaxPosition {
		return nil, fmt.Errorf("sequence of %d exceeds max_position_embeddings %d", t, m.Cfg.MaxPosition)
	}
	h := m.Cfg.HiddenSize
	s := newScratch(t, h, m.Cfg.IntermediateSize)

	// Embeddings: word + position + token type, then LayerNorm.
	for i, id := range ids {
		if int(id) < 0 || int(id) >= m.Cfg.VocabSize {
			return nil, fmt.Errorf("token id %d out of range for vocab %d", id, m.Cfg.VocabSize)
		}
		dst := s.hidden[i*h : i*h+h]
		copy(dst, m.wordEmb[int(id)*h:int(id)*h+h])
		AddInPlace(dst, m.posEmb[i*h:i*h+h])
		if m.typeEmb != nil {
			AddInPlace(dst, m.typeEmb[0:h]) // single segment: token_type 0
		}
	}
	LayerNorm(s.hidden, m.embNorm.gamma, m.embNorm.beta, t, h, m.Cfg.LayerNormEps)

	for i := range m.blocks {
		m.block(&m.blocks[i], s, t)
	}

	// Mean pool. There is no padding here — sequences are encoded one at a
	// time, so every position is real and no mask is needed.
	out := make([]float32, h)
	for i := 0; i < t; i++ {
		AddInPlace(out, s.hidden[i*h:i*h+h])
	}
	inv := float32(1) / float32(t)
	for i := range out {
		out[i] *= inv
	}
	Normalize(out)
	return out, nil
}

func (m *Model) block(b *block, s *scratch, t int) {
	h := m.Cfg.HiddenSize
	heads := m.Cfg.NumHeads
	dk := h / heads
	scale := float32(1 / math.Sqrt(float64(dk)))

	MatMulT(s.q, s.hidden, b.query.w, b.query.b, t, h, h)
	MatMulT(s.k, s.hidden, b.key.w, b.key.b, t, h, h)
	MatMulT(s.v, s.hidden, b.value.w, b.value.b, t, h, h)

	// Attention, one head at a time. Heads occupy contiguous slices of each
	// row, so a head's Q/K/V are strided by the full hidden size.
	parallelFor(heads, func(lo, hi int) {
		scores := make([]float32, t)
		for head := lo; head < hi; head++ {
			off := head * dk
			for i := 0; i < t; i++ {
				qi := s.q[i*h+off : i*h+off+dk]
				for j := 0; j < t; j++ {
					scores[j] = dotUnrolled(qi, s.k[j*h+off:j*h+off+dk]) * scale
				}
				SoftmaxRow(scores)

				ctx := s.ctx[i*h+off : i*h+off+dk]
				for d := range ctx {
					ctx[d] = 0
				}
				for j := 0; j < t; j++ {
					w := scores[j]
					if w == 0 {
						continue
					}
					vj := s.v[j*h+off : j*h+off+dk]
					for d := range ctx {
						ctx[d] += w * vj[d]
					}
				}
			}
		}
	})

	// Attention output projection, residual, norm.
	MatMulT(s.proj, s.ctx, b.attnOut.w, b.attnOut.b, t, h, h)
	AddInPlace(s.hidden, s.proj)
	LayerNorm(s.hidden, b.attnNorm.gamma, b.attnNorm.beta, t, h, m.Cfg.LayerNormEps)

	// Feed-forward, residual, norm.
	MatMulT(s.inter, s.hidden, b.inter.w, b.inter.b, t, h, m.Cfg.IntermediateSize)
	GELU(s.inter)
	MatMulT(s.proj, s.inter, b.outer.w, b.outer.b, t, m.Cfg.IntermediateSize, h)
	AddInPlace(s.hidden, s.proj)
	LayerNorm(s.hidden, b.outNorm.gamma, b.outNorm.beta, t, h, m.Cfg.LayerNormEps)
}
