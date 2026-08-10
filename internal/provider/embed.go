package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"
)

// EmbedResponse carries a batch of embeddings back from a provider.
type EmbedResponse struct {
	Vectors  [][]float32
	Model    string
	Provider string
	Usage    Usage
	Latency  time.Duration
}

// Embedder is separate from Provider because embedding support is optional.
// A backend that only serves chat should not have to stub out a method, and
// the router needs to be able to ask "can this target embed at all".
type Embedder interface {
	Embed(ctx context.Context, model string, inputs []string) (*EmbedResponse, error)
}

// embedBatch is the per-request input cap. Providers differ on their real
// limits; 64 short code chunks is comfortably inside every free tier we
// target and keeps a failed request cheap to retry.
const embedBatch = 64

// Embed vectorises inputs, splitting into batches and preserving input order.
//
// Order preservation is not incidental: the caller maps vector i back to chunk
// i. Providers are permitted to return results out of order with an explicit
// index field, and at least some do.
func (c *OpenAICompat) Embed(ctx context.Context, model string, inputs []string) (*EmbedResponse, error) {
	if len(inputs) == 0 {
		return &EmbedResponse{Model: model, Provider: c.name}, nil
	}
	start := time.Now()
	out := &EmbedResponse{
		Vectors:  make([][]float32, 0, len(inputs)),
		Model:    model,
		Provider: c.name,
	}

	for i := 0; i < len(inputs); i += embedBatch {
		end := i + embedBatch
		if end > len(inputs) {
			end = len(inputs)
		}
		vecs, usage, err := c.embedOnce(ctx, model, inputs[i:end])
		if err != nil {
			return nil, err
		}
		if len(vecs) != end-i {
			return nil, &Error{
				Kind: ErrUnknown, Provider: c.name, Model: model,
				Body: fmt.Sprintf("asked for %d embeddings, got %d", end-i, len(vecs)),
			}
		}
		out.Vectors = append(out.Vectors, vecs...)
		out.Usage.PromptTokens += usage.PromptTokens
		out.Usage.TotalTokens += usage.TotalTokens
	}
	out.Latency = time.Since(start)
	return out, nil
}

type embedItem struct {
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}

func (c *OpenAICompat) embedOnce(ctx context.Context, model string, inputs []string) ([][]float32, Usage, error) {
	body := map[string]any{"model": model, "input": inputs}

	httpReq, err := c.newRequest(ctx, http.MethodPost, "/embeddings", body)
	if err != nil {
		return nil, Usage{}, &Error{Kind: ErrBadRequest, Provider: c.name, Model: model, Err: err}
	}
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, Usage{}, c.wrapErr(model, nil, "", err)
	}
	defer resp.Body.Close()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if resp.StatusCode >= 400 {
		return nil, Usage{}, c.wrapErr(model, resp, string(raw), nil)
	}
	if readErr != nil {
		return nil, Usage{}, c.wrapErr(model, nil, "", readErr)
	}

	var wire struct {
		Data  []embedItem `json:"data"`
		Model string      `json:"model"`
		Usage *Usage      `json:"usage"`
		Error *wireError  `json:"error"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, Usage{}, &Error{
			Kind: ErrUnknown, Provider: c.name, Model: model,
			Body: truncate(string(raw), 400), Err: err,
		}
	}
	if wire.Error != nil && wire.Error.Message != "" {
		return nil, Usage{}, &Error{
			Kind: classifyHTTP(200, wire.Error.Message, 0), Provider: c.name,
			Model: model, Body: wire.Error.Message,
		}
	}
	if len(wire.Data) == 0 {
		return nil, Usage{}, &Error{
			Kind: ErrUnknown, Provider: c.name, Model: model, Body: "no embeddings returned",
		}
	}

	sort.Slice(wire.Data, func(i, j int) bool { return wire.Data[i].Index < wire.Data[j].Index })
	vecs := make([][]float32, 0, len(wire.Data))
	dim := len(wire.Data[0].Embedding)
	for _, d := range wire.Data {
		if len(d.Embedding) != dim {
			return nil, Usage{}, &Error{
				Kind: ErrUnknown, Provider: c.name, Model: model,
				Body: fmt.Sprintf("ragged embedding dimensions: %d vs %d", len(d.Embedding), dim),
			}
		}
		vecs = append(vecs, d.Embedding)
	}
	var usage Usage
	if wire.Usage != nil {
		usage = *wire.Usage
	}
	return vecs, usage, nil
}
