package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/VEER-TARGARYEN/forge/internal/embed"
)

func cmdEmbed(args []string) error {
	fs := flag.NewFlagSet("embed", flag.ExitOnError)
	modelDir := fs.String("model", "", "path to a HuggingFace model directory")
	bench := fs.Bool("bench", false, "measure kernel throughput without loading a model")
	maxTokens := fs.Int("max-tokens", 256, "truncate input to this many tokens")
	showVec := fs.Bool("vector", false, "print the full vector, not just a summary")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *bench {
		fmt.Printf("encoder kernels at all-MiniLM-L6-v2 dimensions (384 hidden, 1536 ffn, 6 layers)\n")
		fmt.Printf("host: %s/%s, %d logical CPUs\n\n", runtime.GOOS, runtime.GOARCH, runtime.NumCPU())
		embed.Benchmark(embed.MiniLMBench(), os.Stdout)
		fmt.Printf("\nNo model weights are needed for this: a matmul costs the same\n" +
			"regardless of the values in it.\n")
		return nil
	}

	if *modelDir == "" {
		return fmt.Errorf("pass -model <dir>, or -bench to measure throughput\n\n%s", embed.ModelHint)
	}
	if err := embed.LooksLikeModelDir(*modelDir); err != nil {
		return fmt.Errorf("%v\n\n%s", err, embed.ModelHint)
	}

	loadStart := time.Now()
	em, err := embed.Load(*modelDir, embed.Options{MaxTokens: *maxTokens})
	if err != nil {
		return err
	}
	fmt.Printf("model:  %s\n", em.Describe())
	fmt.Printf("loaded: %s\n\n", time.Since(loadStart).Round(time.Millisecond))

	texts := fs.Args()
	if len(texts) == 0 {
		texts = []string{"the quick brown fox jumps over the lazy dog"}
	}

	start := time.Now()
	vecs, err := em.Embed(context.Background(), texts)
	if err != nil {
		return err
	}
	elapsed := time.Since(start)

	for i, v := range vecs {
		fmt.Printf("%q\n", trim(texts[i], 70))
		if *showVec {
			fmt.Printf("  %v\n", v)
		} else {
			fmt.Printf("  %d dims, first 6: [%.4f %.4f %.4f %.4f %.4f %.4f …]\n",
				len(v), v[0], v[1], v[2], v[3], v[4], v[5])
		}
	}

	// Cosine between the inputs, which is the only thing an embedding is
	// actually for.
	if len(vecs) > 1 {
		fmt.Printf("\ncosine similarity\n")
		for i := 0; i < len(vecs); i++ {
			for j := i + 1; j < len(vecs); j++ {
				fmt.Printf("  [%d]x[%d]  %+.4f\n", i, j, embed.Dot(vecs[i], vecs[j]))
			}
		}
	}
	fmt.Printf("\n%d sequence(s) in %s (%s each)\n",
		len(texts), elapsed.Round(time.Millisecond),
		(elapsed / time.Duration(len(texts))).Round(time.Millisecond))
	return nil
}
