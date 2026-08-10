// Package config loads forge.json. It is stdlib-only by design: the whole
// project has zero external dependencies, and JSON with ${ENV} expansion is
// enough configuration language for a personal agent.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type Provider struct {
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key,omitempty"`
	Enabled *bool  `json:"enabled,omitempty"`

	// RequiresKey false means a local backend that needs no auth.
	RequiresKey *bool `json:"requires_key,omitempty"`
	// StreamUsage sends stream_options.include_usage. Turn it off for
	// backends that reject the field rather than ignoring it.
	StreamUsage *bool `json:"stream_usage,omitempty"`
	// JSONSchema advertises response_format json_schema support.
	JSONSchema *bool `json:"json_schema,omitempty"`

	TimeoutSec     int               `json:"timeout_sec,omitempty"`
	MaxTokensField string            `json:"max_tokens_field,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	Note           string            `json:"note,omitempty"`
}

func (p Provider) IsEnabled() bool       { return p.Enabled == nil || *p.Enabled }
func (p Provider) NeedsKey() bool        { return p.RequiresKey == nil || *p.RequiresKey }
func (p Provider) WantStreamUsage() bool { return p.StreamUsage == nil || *p.StreamUsage }
func (p Provider) HasJSONSchema() bool   { return p.JSONSchema != nil && *p.JSONSchema }

// Target is one (provider, model) pair inside a class chain. Order in the
// chain is priority order: first healthy target wins.
type Target struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// MaxContext lets the router skip a target whose window is too small for
	// the current request instead of burning a call to learn that.
	MaxContext int `json:"max_context,omitempty"`
	// MaxTokens caps output for this target specifically.
	MaxTokens int    `json:"max_tokens,omitempty"`
	Note      string `json:"note,omitempty"`
}

func (t Target) Key() string { return t.Provider + "|" + t.Model }

type Defaults struct {
	Temperature float64 `json:"temperature"`
	MaxTokens   int     `json:"max_tokens"`
}

type Policy struct {
	// Cooldown seconds applied per error kind when a target fails.
	RateLimitCooldownSec  int `json:"rate_limit_cooldown_sec"`
	QuotaCooldownSec      int `json:"quota_cooldown_sec"`
	AuthCooldownSec       int `json:"auth_cooldown_sec"`
	ServerCooldownSec     int `json:"server_cooldown_sec"`
	BadRequestCooldownSec int `json:"bad_request_cooldown_sec"`
	MaxCooldownSec        int `json:"max_cooldown_sec"`
	// SameTargetRetries is how many times to retry a transient failure on the
	// same target before moving down the chain.
	SameTargetRetries int `json:"same_target_retries"`

	// RateLimitWaitSec is the longest a rate limit will be waited out inline
	// rather than demoting the target.
	//
	// Free tiers throttle on tokens per minute, so a provider routinely says
	// "try again in 1.2s" mid-run. Treating that as a failure ends the run
	// over a wait shorter than a single model call, which defeats the entire
	// point of building on free tiers.
	RateLimitWaitSec int `json:"rate_limit_wait_sec"`
	// RateLimitWaits bounds how many such pauses one target may take per call.
	RateLimitWaits int `json:"rate_limit_waits"`
}

type Server struct {
	Addr         string `json:"addr"`
	DefaultClass string `json:"default_class"`
	// APIKey, if set, is required as a bearer token by the local endpoint.
	// Useful only if you bind to something other than loopback.
	APIKey string `json:"api_key,omitempty"`
}

type Config struct {
	Providers []Provider          `json:"providers"`
	Classes   map[string][]Target `json:"classes"`
	Defaults  Defaults            `json:"defaults"`
	Policy    Policy              `json:"policy"`
	Server    Server              `json:"server"`
	StateDir  string              `json:"state_dir,omitempty"`

	path string
}

func (c *Config) Path() string { return c.path }

// Dir returns the directory used for the usage ledger, health state, and
// bench results.
func (c *Config) Dir() string {
	if c.StateDir != "" {
		return c.StateDir
	}
	if c.path != "" {
		return filepath.Dir(c.path)
	}
	return "."
}

// Provider looks up a provider block by name.
func (c *Config) ProviderByName(name string) (Provider, bool) {
	for _, p := range c.Providers {
		if strings.EqualFold(p.Name, name) {
			return p, true
		}
	}
	return Provider{}, false
}

func (c *Config) ClassNames() []string {
	out := make([]string, 0, len(c.Classes))
	for k := range c.Classes {
		out = append(out, k)
	}
	// Stable, meaningful order rather than map order.
	pref := map[string]int{"planner": 0, "coder": 1, "cheap": 2, "local": 3}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			a, b := out[j-1], out[j]
			ra, oka := pref[a]
			rb, okb := pref[b]
			if !oka {
				ra = 100
			}
			if !okb {
				rb = 100
			}
			if ra > rb || (ra == rb && a > b) {
				out[j-1], out[j] = out[j], out[j-1]
			} else {
				break
			}
		}
	}
	return out
}

// DefaultPath resolves the config location: FORGE_CONFIG, then ./forge.json,
// then ~/.forge/config.json.
func DefaultPath() string {
	if p := os.Getenv("FORGE_CONFIG"); p != "" {
		return p
	}
	if _, err := os.Stat("forge.json"); err == nil {
		abs, _ := filepath.Abs("forge.json")
		return abs
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "forge.json"
	}
	return filepath.Join(home, ".forge", "config.json")
}

var envPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv replaces ${VAR} with the environment value, or empty string when
// unset. An unset key is not an error: it just means that provider stays
// unconfigured and the router skips it.
func expandEnv(s string) string {
	return envPattern.ReplaceAllStringFunc(s, func(m string) string {
		return os.Getenv(m[2 : len(m)-1])
	})
}

func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath()
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		// Retry permissively so a stray field from a newer version is a
		// warning-shaped failure, not a hard stop.
		if err2 := json.Unmarshal(raw, &c); err2 != nil {
			return nil, fmt.Errorf("parse config %s: %w", path, err2)
		}
	}
	c.path = path

	for i := range c.Providers {
		c.Providers[i].APIKey = expandEnv(c.Providers[i].APIKey)
		c.Providers[i].BaseURL = expandEnv(c.Providers[i].BaseURL)
		for k, v := range c.Providers[i].Headers {
			c.Providers[i].Headers[k] = expandEnv(v)
		}
	}
	c.StateDir = expandEnv(c.StateDir)
	c.applyDefaults()
	return &c, c.validate()
}

func (c *Config) applyDefaults() {
	if c.Defaults.Temperature == 0 {
		c.Defaults.Temperature = 0.2
	}
	if c.Defaults.MaxTokens == 0 {
		c.Defaults.MaxTokens = 4096
	}
	if c.Policy.RateLimitCooldownSec == 0 {
		c.Policy.RateLimitCooldownSec = 20
	}
	if c.Policy.QuotaCooldownSec == 0 {
		c.Policy.QuotaCooldownSec = 3600
	}
	if c.Policy.AuthCooldownSec == 0 {
		c.Policy.AuthCooldownSec = 86400
	}
	if c.Policy.ServerCooldownSec == 0 {
		c.Policy.ServerCooldownSec = 30
	}
	if c.Policy.BadRequestCooldownSec == 0 {
		c.Policy.BadRequestCooldownSec = 600
	}
	if c.Policy.MaxCooldownSec == 0 {
		c.Policy.MaxCooldownSec = 900
	}
	if c.Policy.SameTargetRetries == 0 {
		c.Policy.SameTargetRetries = 1
	}
	if c.Policy.RateLimitWaitSec == 0 {
		c.Policy.RateLimitWaitSec = 30
	}
	if c.Policy.RateLimitWaits == 0 {
		c.Policy.RateLimitWaits = 3
	}
	if c.Server.Addr == "" {
		c.Server.Addr = "127.0.0.1:4000"
	}
	if c.Server.DefaultClass == "" {
		c.Server.DefaultClass = "coder"
	}
}

func (c *Config) validate() error {
	if len(c.Providers) == 0 {
		return fmt.Errorf("config has no providers")
	}
	if len(c.Classes) == 0 {
		return fmt.Errorf("config has no classes")
	}
	seen := map[string]bool{}
	for _, p := range c.Providers {
		if p.Name == "" {
			return fmt.Errorf("provider with empty name")
		}
		if seen[strings.ToLower(p.Name)] {
			return fmt.Errorf("duplicate provider %q", p.Name)
		}
		seen[strings.ToLower(p.Name)] = true
		if p.BaseURL == "" {
			return fmt.Errorf("provider %q has no base_url", p.Name)
		}
	}
	for class, targets := range c.Classes {
		if len(targets) == 0 {
			return fmt.Errorf("class %q has no targets", class)
		}
		for _, t := range targets {
			if _, ok := c.ProviderByName(t.Provider); !ok {
				return fmt.Errorf("class %q references unknown provider %q", class, t.Provider)
			}
			if t.Model == "" {
				return fmt.Errorf("class %q has a target with no model", class)
			}
		}
	}
	if _, ok := c.Classes[c.Server.DefaultClass]; !ok {
		return fmt.Errorf("server.default_class %q is not a defined class", c.Server.DefaultClass)
	}
	return nil
}

func Save(c *Config, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o600)
}

func boolPtr(b bool) *bool { return &b }

// Default returns a starter config wired to the free tiers worth stacking.
// Model ids drift; `forge models <provider>` prints the live list so you can
// correct any of these without guessing.
func Default() *Config {
	c := &Config{
		Providers: []Provider{
			{
				Name: "cerebras", BaseURL: "https://api.cerebras.ai/v1",
				APIKey: "${CEREBRAS_API_KEY}", JSONSchema: boolPtr(true),
				TimeoutSec: 120,
				Note:       "Free tier, extremely fast. Get a key at cloud.cerebras.ai",
			},
			{
				Name: "groq", BaseURL: "https://api.groq.com/openai/v1",
				APIKey: "${GROQ_API_KEY}", JSONSchema: boolPtr(true),
				TimeoutSec: 120,
				Note:       "Free tier, very fast. console.groq.com/keys",
			},
			{
				Name: "gemini", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai",
				APIKey: "${GEMINI_API_KEY}", JSONSchema: boolPtr(true),
				StreamUsage: boolPtr(false), TimeoutSec: 180,
				Note: "Free tier, 1M context. aistudio.google.com/apikey",
			},
			{
				Name: "openrouter", BaseURL: "https://openrouter.ai/api/v1",
				APIKey: "${OPENROUTER_API_KEY}", JSONSchema: boolPtr(true),
				TimeoutSec: 180,
				Headers:    map[string]string{"X-Title": "forge"},
				Note:       "Use :free model suffixes. openrouter.ai/keys",
			},
			{
				Name: "github", BaseURL: "https://models.github.ai/inference",
				APIKey: "${GITHUB_MODELS_TOKEN}", TimeoutSec: 120,
				Note: "Free with a GitHub PAT (models:read scope). Low rate limits.",
			},
			{
				Name: "local", BaseURL: "http://127.0.0.1:8080/v1",
				RequiresKey: boolPtr(false), JSONSchema: boolPtr(true),
				TimeoutSec: 1800,
				Note:       "llama-server. CPU prefill is slow, hence the long timeout.",
			},
			{
				Name: "ollama", BaseURL: "http://127.0.0.1:11434/v1",
				RequiresKey: boolPtr(false), JSONSchema: boolPtr(true),
				TimeoutSec: 1800,
				Note:       "Alternative local backend if you prefer Ollama.",
			},
		},
		Classes: map[string][]Target{
			// planner: hard reasoning, whole-repo context. Prefer big windows.
			"planner": {
				{Provider: "gemini", Model: "gemini-2.5-flash", MaxContext: 1048576},
				{Provider: "cerebras", Model: "gpt-oss-120b", MaxContext: 131072},
				{Provider: "groq", Model: "moonshotai/kimi-k2-instruct", MaxContext: 131072},
				{Provider: "openrouter", Model: "deepseek/deepseek-chat-v3-0324:free", MaxContext: 65536},
				{Provider: "local", Model: "qwen2.5-coder-7b-instruct", MaxContext: 32768},
			},
			// coder: the agent loop workhorse. Favour speed and tool-calling.
			"coder": {
				{Provider: "cerebras", Model: "gpt-oss-120b", MaxContext: 131072},
				{Provider: "groq", Model: "openai/gpt-oss-120b", MaxContext: 131072},
				{Provider: "gemini", Model: "gemini-2.5-flash", MaxContext: 1048576},
				{Provider: "openrouter", Model: "qwen/qwen3-coder:free", MaxContext: 65536},
				{Provider: "local", Model: "qwen2.5-coder-7b-instruct", MaxContext: 32768},
			},
			// cheap: summarization, compaction, classification. Local first,
			// because these are exactly the calls not worth a hosted quota.
			"cheap": {
				{Provider: "local", Model: "qwen2.5-coder-7b-instruct", MaxContext: 32768},
				{Provider: "groq", Model: "llama-3.3-70b-versatile", MaxContext: 131072},
				{Provider: "gemini", Model: "gemini-2.5-flash-lite", MaxContext: 1048576},
			},
			// embed: vectors for semantic code search. Local first — this is
			// the highest-volume, lowest-value-per-call traffic in the whole
			// system, and a 137M-parameter embedder is fast even on CPU.
			"embed": {
				{Provider: "ollama", Model: "nomic-embed-text"},
				{Provider: "local", Model: "nomic-embed-text"},
				{Provider: "gemini", Model: "text-embedding-004"},
			},
			// local: forced offline path, used for privacy-sensitive work.
			"local": {
				{Provider: "local", Model: "qwen2.5-coder-7b-instruct", MaxContext: 32768},
				{Provider: "ollama", Model: "qwen2.5-coder:7b-instruct-q4_K_M", MaxContext: 32768},
			},
		},
		Defaults: Defaults{Temperature: 0.2, MaxTokens: 4096},
		Server:   Server{Addr: "127.0.0.1:4000", DefaultClass: "coder"},
	}
	c.applyDefaults()
	return c
}
