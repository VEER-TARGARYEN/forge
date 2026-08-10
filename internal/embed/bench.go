package embed

import (
	"fmt"
	"io"
	"math/rand"
	"time"
)

// BenchResult is one measured sequence length.
type BenchResult struct {
	Tokens    int
	PerSeq    time.Duration
	GFLOPS    float64
	SeqPerMin float64
}

// BenchConfig describes the model shape to simulate.
type BenchConfig struct {
	Hidden       int
	Intermediate int
	Layers       int
	Lengths      []int
	Repeats      int
}

// MiniLMBench is the shape of all-MiniLM-L6-v2, the recommended default.
func MiniLMBench() BenchConfig {
	return BenchConfig{
		Hidden: 384, Intermediate: 1536, Layers: 6,
		Lengths: []int{64, 128, 256}, Repeats: 3,
	}
}

// Benchmark measures the forward-pass kernels without needing real weights.
//
// The arithmetic cost of a matmul does not depend on the values in it, so a
// synthetic run gives the same timing a loaded model would — which means the
// throughput question can be answered before downloading 90 MB.
func Benchmark(cfg BenchConfig, progress io.Writer) []BenchResult {
	if cfg.Repeats <= 0 {
		cfg.Repeats = 3
	}
	rng := rand.New(rand.NewSource(1))
	rnd := func(n int) []float32 {
		v := make([]float32, n)
		for i := range v {
			v[i] = float32(rng.NormFloat64())
		}
		return v
	}

	h, inter := cfg.Hidden, cfg.Intermediate
	wSquare := rnd(h * h)
	wUp := rnd(inter * h)
	wDown := rnd(h * inter)
	biasH := rnd(h)
	biasI := rnd(inter)

	var out []BenchResult
	for _, tokens := range cfg.Lengths {
		x := rnd(tokens * h)
		outH := make([]float32, tokens*h)
		outI := make([]float32, tokens*inter)

		start := time.Now()
		for r := 0; r < cfg.Repeats; r++ {
			for l := 0; l < cfg.Layers; l++ {
				// Q, K, V, and the attention output projection.
				for i := 0; i < 4; i++ {
					MatMulT(outH, x, wSquare, biasH, tokens, h, h)
				}
				MatMulT(outI, x, wUp, biasI, tokens, h, inter)
				GELU(outI)
				MatMulT(outH, outI, wDown, biasH, tokens, inter, h)
				LayerNorm(outH, nil, nil, tokens, h, 1e-12)
			}
		}
		el := time.Since(start) / time.Duration(cfg.Repeats)

		// Attention's own score/context matmuls are excluded: they are
		// quadratic in length and small at these sizes next to the
		// projections, so counting them would overstate the FLOP rate.
		macs := float64(cfg.Layers) * float64(tokens) * float64(4*h*h+2*h*inter)
		res := BenchResult{
			Tokens: tokens,
			PerSeq: el,
			GFLOPS: 2 * macs / el.Seconds() / 1e9,
		}
		if el > 0 {
			res.SeqPerMin = 60 / el.Seconds()
		}
		out = append(out, res)
		if progress != nil {
			fmt.Fprintf(progress, "  %3d tokens: %9s per sequence   %5.1f GFLOP/s   %6.0f sequences/min\n",
				res.Tokens, res.PerSeq.Round(time.Millisecond), res.GFLOPS, res.SeqPerMin)
		}
	}
	return out
}
