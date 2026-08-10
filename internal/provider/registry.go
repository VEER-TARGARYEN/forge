package provider

import (
	"time"

	"github.com/VEER-TARGARYEN/forge/internal/config"
)

// Registry holds one live client per configured provider.
type Registry struct {
	byName map[string]Provider
	order  []string
}

// NewRegistry constructs clients for every enabled provider block. Providers
// missing an API key are still constructed — Configured() reports false and
// the router skips them — so `forge doctor` can tell you *why* a target is
// unavailable instead of it silently vanishing.
func NewRegistry(cfg *config.Config) *Registry {
	r := &Registry{byName: make(map[string]Provider, len(cfg.Providers))}
	for _, p := range cfg.Providers {
		if !p.IsEnabled() {
			continue
		}
		timeout := time.Duration(p.TimeoutSec) * time.Second
		client := NewOpenAICompat(OpenAIOpts{
			Name:           p.Name,
			BaseURL:        p.BaseURL,
			APIKey:         p.APIKey,
			Headers:        p.Headers,
			Timeout:        timeout,
			RequiresKey:    p.NeedsKey(),
			StreamUsage:    p.WantStreamUsage(),
			JSONSchema:     p.HasJSONSchema(),
			MaxTokensField: p.MaxTokensField,
		})
		r.byName[p.Name] = client
		r.order = append(r.order, p.Name)
	}
	return r
}

func (r *Registry) Get(name string) (Provider, bool) {
	p, ok := r.byName[name]
	return p, ok
}

// Names returns provider names in config order.
func (r *Registry) Names() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}
