package embed

import (
	"math"
	"runtime"
	"sync"
)

// The kernels.
//
// Go has no SIMD intrinsics, so the levers available are memory layout and
// instruction-level parallelism: keep both operands contiguous along the
// reduction axis, and unroll the accumulator so the CPU can keep several
// independent FMA chains in flight instead of serialising on one running sum.
// That is most of the difference between a naive loop and something usable.

// MatMulT computes out = x @ wᵀ + bias.
//
//	x    [n, k]  activations
//	w    [m, k]  weights, in the layout PyTorch's nn.Linear already uses
//	out  [n, m]
//
// The transposed weight layout is not a conversion step we chose to add — it
// is how HuggingFace stores Linear weights, and it happens to be the layout
// that makes the inner loop a contiguous dot product over both operands.
func MatMulT(out, x, w, bias []float32, n, k, m int) {
	parallelFor(n, func(lo, hi int) {
		for i := lo; i < hi; i++ {
			xi := x[i*k : i*k+k]
			oi := out[i*m : i*m+m]
			for j := 0; j < m; j++ {
				oi[j] = dotUnrolled(xi, w[j*k:j*k+k])
			}
			if bias != nil {
				for j := 0; j < m; j++ {
					oi[j] += bias[j]
				}
			}
		}
	})
}

// dotUnrolled is the inner kernel. Four accumulators, because a single running
// sum makes every multiply-add wait on the previous one.
func dotUnrolled(a, b []float32) float32 {
	var s0, s1, s2, s3 float32
	i := 0
	n := len(a)
	for ; i+8 <= n; i += 8 {
		s0 += a[i] * b[i]
		s1 += a[i+1] * b[i+1]
		s2 += a[i+2] * b[i+2]
		s3 += a[i+3] * b[i+3]
		s0 += a[i+4] * b[i+4]
		s1 += a[i+5] * b[i+5]
		s2 += a[i+6] * b[i+6]
		s3 += a[i+7] * b[i+7]
	}
	var rest float32
	for ; i < n; i++ {
		rest += a[i] * b[i]
	}
	return (s0 + s1) + (s2 + s3) + rest
}

// Dot is the exported single dot product, for callers outside the hot path.
func Dot(a, b []float32) float32 { return dotUnrolled(a, b) }

// LayerNorm normalises each row of x in place.
//
// Mean and variance are accumulated in float64. In float32 the sum of squares
// over a 768-wide row loses enough precision to shift the result visibly, and
// the cost of the wider accumulator is nil next to the matmuls around it.
func LayerNorm(x, gamma, beta []float32, n, d int, eps float32) {
	parallelFor(n, func(lo, hi int) {
		for i := lo; i < hi; i++ {
			row := x[i*d : i*d+d]
			var mean float64
			for _, v := range row {
				mean += float64(v)
			}
			mean /= float64(d)

			var variance float64
			for _, v := range row {
				diff := float64(v) - mean
				variance += diff * diff
			}
			variance /= float64(d)

			inv := float32(1 / math.Sqrt(variance+float64(eps)))
			m := float32(mean)
			for j, v := range row {
				norm := (v - m) * inv
				if gamma != nil {
					norm *= gamma[j]
				}
				if beta != nil {
					norm += beta[j]
				}
				row[j] = norm
			}
		}
	})
}

// GELU applies the tanh approximation in place, which is what BERT was trained
// with — the exact erf form gives slightly different activations.
func GELU(x []float32) {
	const c = 0.7978845608028654 // sqrt(2/pi)
	for i, v := range x {
		v64 := float64(v)
		inner := c * (v64 + 0.044715*v64*v64*v64)
		x[i] = float32(0.5 * v64 * (1 + math.Tanh(inner)))
	}
}

// SoftmaxRow normalises one row in place, subtracting the max first so a large
// logit cannot overflow the exponential.
func SoftmaxRow(row []float32) {
	if len(row) == 0 {
		return
	}
	max := row[0]
	for _, v := range row[1:] {
		if v > max {
			max = v
		}
	}
	var sum float64
	for i, v := range row {
		e := math.Exp(float64(v - max))
		row[i] = float32(e)
		sum += e
	}
	if sum == 0 {
		return
	}
	inv := float32(1 / sum)
	for i := range row {
		row[i] *= inv
	}
}

// AddInPlace computes a += b.
func AddInPlace(a, b []float32) {
	for i := range a {
		a[i] += b[i]
	}
}

// Normalize scales a vector to unit length, which turns a dot product into a
// cosine for everything downstream.
func Normalize(v []float32) {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return
	}
	inv := float32(1 / math.Sqrt(sum))
	for i := range v {
		v[i] *= inv
	}
}

// workers is the parallel width. Physical cores, not logical: this workload is
// memory-bandwidth bound, and hyperthreads contend for the same load ports
// rather than adding throughput.
var workers = func() int {
	n := runtime.NumCPU()
	if n > 2 {
		n /= 2
	}
	if n < 1 {
		n = 1
	}
	return n
}()

// parallelFor splits [0,n) across workers, running inline when the work is too
// small to be worth the goroutine overhead.
func parallelFor(n int, fn func(lo, hi int)) {
	if n <= 0 {
		return
	}
	if workers == 1 || n < 4 {
		fn(0, n)
		return
	}
	w := workers
	if w > n {
		w = n
	}
	chunk := (n + w - 1) / w

	var wg sync.WaitGroup
	for start := 0; start < n; start += chunk {
		end := start + chunk
		if end > n {
			end = n
		}
		wg.Add(1)
		go func(lo, hi int) {
			defer wg.Done()
			fn(lo, hi)
		}(start, end)
	}
	wg.Wait()
}
