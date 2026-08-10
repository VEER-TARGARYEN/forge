// Package bench measures what your hardware and your free tiers actually
// deliver. Every later decision — context budget, which class runs the agent
// loop, whether a 7B is usable at all — depends on these numbers, so the
// harness is deliberately paranoid about not measuring the wrong thing.
package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/VEER-TARGARYEN/forge/internal/config"
	"github.com/VEER-TARGARYEN/forge/internal/provider"
)

type Result struct {
	Provider     string        `json:"provider"`
	Model        string        `json:"model"`
	TargetPrompt int           `json:"target_prompt_tokens"`
	PromptTok    int           `json:"prompt_tokens"`
	OutTok       int           `json:"completion_tokens"`
	Estimated    bool          `json:"estimated_usage,omitempty"`
	TTFT         time.Duration `json:"ttft"`
	Total        time.Duration `json:"total"`
	PrefillTPS   float64       `json:"prefill_tps"`
	DecodeTPS    float64       `json:"decode_tps"`
	Err          string        `json:"error,omitempty"`
}

type Report struct {
	StartedAt time.Time `json:"started_at"`
	GOOS      string    `json:"goos"`
	GOARCH    string    `json:"goarch"`
	NumCPU    int       `json:"num_cpu"`
	Results   []Result  `json:"results"`
}

type Options struct {
	// PromptSizes are approximate prompt lengths in tokens.
	PromptSizes []int
	// GenTokens is how many tokens to generate per run.
	GenTokens int
	// Repeats runs each (target, size) N times and keeps the best decode
	// rate, which filters out background-load noise on a laptop.
	Repeats int
	// Warmup issues one throwaway call per target so model load time and TLS
	// handshakes do not land inside a measurement.
	Warmup bool
	Out    io.Writer
}

func DefaultOptions() Options {
	return Options{
		PromptSizes: []int{256, 1024, 4096},
		GenTokens:   128,
		Repeats:     2,
		Warmup:      true,
		Out:         os.Stdout,
	}
}

// filler is deliberately code-shaped: tokenizers compress prose and code very
// differently, and an agent's context is mostly code.
const filler = `func process(items []Record, limit int) (map[string][]Record, error) {
	out := make(map[string][]Record, len(items))
	for i, it := range items {
		if it.Key == "" { return nil, fmt.Errorf("record %d has empty key", i) }
		if limit > 0 && len(out[it.Key]) >= limit { continue }
		out[it.Key] = append(out[it.Key], it)
	}
	return out, nil
}
`

// buildPrompt produces roughly n tokens of code-like text. The salt goes at
// the FRONT so a server-side prefix KV cache cannot short-circuit prefill:
// benching a cache hit would report a prefill rate you will never see on a
// real first turn.
func buildPrompt(n int, salt string) string {
	const charsPerToken = 3.6
	want := int(float64(n) * charsPerToken)
	var b strings.Builder
	b.WriteString("// session ")
	b.WriteString(salt)
	b.WriteString("\n")
	for b.Len() < want {
		b.WriteString(filler)
	}
	return b.String()
}

// Run benchmarks each target across each prompt size.
func Run(ctx context.Context, reg *provider.Registry, targets []config.Target, opts Options) (*Report, error) {
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	if opts.Repeats < 1 {
		opts.Repeats = 1
	}
	rep := &Report{
		StartedAt: time.Now(),
		GOOS:      runtime.GOOS,
		GOARCH:    runtime.GOARCH,
		NumCPU:    runtime.NumCPU(),
	}

	for _, t := range targets {
		p, ok := reg.Get(t.Provider)
		if !ok {
			fmt.Fprintf(opts.Out, "skip %s: provider disabled\n", t.Key())
			continue
		}
		if !p.Configured() {
			fmt.Fprintf(opts.Out, "skip %s: no API key\n", t.Key())
			continue
		}

		if opts.Warmup {
			fmt.Fprintf(opts.Out, "warming up %s ...\n", t.Key())
			warmCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
			_, err := p.Chat(warmCtx, t.Model, provider.Request{
				Messages:  []provider.Message{{Role: provider.RoleUser, Content: "Reply with the single word: ok"}},
				MaxTokens: 8, Temperature: 0,
			})
			cancel()
			if err != nil {
				fmt.Fprintf(opts.Out, "  warmup failed: %v\n", err)
				rep.Results = append(rep.Results, Result{Provider: t.Provider, Model: t.Model, Err: err.Error()})
				continue
			}
		}

		for _, size := range opts.PromptSizes {
			if t.MaxContext > 0 && size+opts.GenTokens > t.MaxContext {
				fmt.Fprintf(opts.Out, "skip %s @ %d tok: exceeds declared window %d\n", t.Key(), size, t.MaxContext)
				continue
			}
			best := Result{Provider: t.Provider, Model: t.Model, TargetPrompt: size}
			for run := 0; run < opts.Repeats; run++ {
				salt := fmt.Sprintf("%d-%d-%d", size, run, time.Now().UnixNano())
				req := provider.Request{
					Messages: []provider.Message{
						{Role: provider.RoleSystem, Content: "You are a benchmark target. Answer tersely."},
						{Role: provider.RoleUser, Content: buildPrompt(size, salt) +
							"\n\nSummarize what the function above does. Write exactly one paragraph, then keep writing plausible continuation text until you are cut off."},
					},
					MaxTokens:   opts.GenTokens,
					Temperature: 0.7,
				}
				fmt.Fprintf(opts.Out, "  %s  prompt≈%d  run %d/%d ... ", t.Key(), size, run+1, opts.Repeats)
				resp, err := p.Stream(ctx, t.Model, req, func(provider.Chunk) error { return nil })
				if err != nil {
					fmt.Fprintf(opts.Out, "ERROR: %v\n", err)
					if best.Err == "" {
						best.Err = err.Error()
					}
					continue
				}
				r := Result{
					Provider: t.Provider, Model: t.Model, TargetPrompt: size,
					PromptTok: resp.Usage.PromptTokens, OutTok: resp.Usage.CompletionTokens,
					Estimated: resp.Usage.Estimated,
					TTFT:      resp.TTFT, Total: resp.Latency,
					PrefillTPS: resp.PrefillTPS(), DecodeTPS: resp.DecodeTPS(),
				}
				fmt.Fprintf(opts.Out, "ttft %6s  decode %s  prefill %s\n",
					r.TTFT.Round(time.Millisecond), rate(r.DecodeTPS), rate(r.PrefillTPS))
				// Keep the best run: the slow ones are measuring your OS
				// scheduler or someone else's queue, not the model. A first
				// successful result always wins over "nothing yet", even if
				// its rate came back unmeasurable.
				if best.PromptTok == 0 || r.DecodeTPS > best.DecodeTPS {
					keepErr := best.Err
					best = r
					best.Err = keepErr
				}
			}
			if best.PromptTok > 0 || best.Err != "" {
				if best.PromptTok > 0 {
					best.Err = ""
				}
				rep.Results = append(rep.Results, best)
			}
		}
	}
	return rep, nil
}

// WriteMarkdown prints the results table plus the interpretation that
// actually matters: how long one agent turn would take on each target.
func WriteMarkdown(w io.Writer, rep *Report) {
	fmt.Fprintf(w, "\n# forge bench\n\n")
	fmt.Fprintf(w, "host: %s/%s, %d logical CPUs\n\n", rep.GOOS, rep.GOARCH, rep.NumCPU)

	fmt.Fprintln(w, "| target | prompt tok | out tok | TTFT | prefill tok/s | decode tok/s |")
	fmt.Fprintln(w, "|---|---:|---:|---:|---:|---:|")
	for _, r := range rep.Results {
		name := r.Provider + "/" + r.Model
		if r.Err != "" {
			fmt.Fprintf(w, "| %s | — | — | — | — | **failed** |\n", name)
			continue
		}
		est := ""
		if r.Estimated {
			est = " ~"
		}
		fmt.Fprintf(w, "| %s | %d%s | %d | %s | %s | **%s** |\n",
			name, r.PromptTok, est, r.OutTok,
			r.TTFT.Round(time.Millisecond), rate(r.PrefillTPS), rate(r.DecodeTPS))
	}

	fmt.Fprintf(w, "\n## What this means for an agent turn\n\n")
	fmt.Fprintln(w, "Assuming a realistic turn of 8,000 prompt tokens and 600 generated tokens:")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| target | prefill | decode | total per turn |")
	fmt.Fprintln(w, "|---|---:|---:|---:|")

	// Use the largest successful prompt size per target: prefill rate at 256
	// tokens is dominated by fixed overhead and flatters the model.
	bestBySize := map[string]Result{}
	for _, r := range rep.Results {
		if r.Err != "" || r.DecodeTPS <= 0 {
			continue
		}
		k := r.Provider + "/" + r.Model
		if prev, ok := bestBySize[k]; !ok || r.PromptTok > prev.PromptTok {
			bestBySize[k] = r
		}
	}
	for _, r := range sortedKeys(bestBySize) {
		res := bestBySize[r]
		var prefill time.Duration
		if res.PrefillTPS > 0 {
			prefill = time.Duration(8000 / res.PrefillTPS * float64(time.Second))
		}
		decode := time.Duration(600 / res.DecodeTPS * float64(time.Second))
		fmt.Fprintf(w, "| %s | %s | %s | **%s** |\n", r,
			prefill.Round(time.Second), decode.Round(time.Second), (prefill + decode).Round(time.Second))
	}
	fmt.Fprintln(w, "\nFor hosted providers TTFT includes network round-trip and queueing, so treat")
	fmt.Fprintln(w, "their prefill column as an upper bound on latency, not a hardware measurement.")
	fmt.Fprintln(w, "For a local model it is very close to true prefill throughput.")
	fmt.Fprintf(w, "\nA rate of `n/a` means the span was shorter than the %s clock floor, not that\n",
		provider.ClockFloor)
	fmt.Fprintln(w, "the call failed — Go's monotonic clock on Windows ticks at roughly 0.6 ms.")
}

// rate formats a throughput figure, distinguishing "too fast to measure" from
// a genuine zero.
func rate(v float64) string {
	if v <= 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f", v)
}

func sortedKeys(m map[string]Result) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Save writes the raw report next to the config so runs can be compared after
// you change threads, quantization, or context size.
func Save(dir string, rep *Report) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("bench-%s.json", rep.StartedAt.Format("20060102-150405"))
	path := filepath.Join(dir, name)
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return "", err
	}
	return path, os.WriteFile(path, append(b, '\n'), 0o644)
}
