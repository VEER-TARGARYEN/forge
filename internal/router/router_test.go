package router

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VEER-TARGARYEN/forge/internal/config"
	"github.com/VEER-TARGARYEN/forge/internal/provider"
)

// stubServer serves a fixed status. On 200 it answers in whichever format the
// request asked for: plain JSON for a normal call, SSE when "stream":true is
// set. Router.Chat uses the non-streaming path, so a stub that only ever
// emitted SSE would fail for reasons that have nothing to do with routing.
func stubServer(status int, retryAfter string, body string, hits *int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			fmt.Fprint(w, `{"data":[{"id":"m"}]}`)
			return
		}
		atomic.AddInt32(hits, 1)
		if status != 200 {
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			w.WriteHeader(status)
			fmt.Fprint(w, body)
			return
		}

		raw, _ := io.ReadAll(r.Body)
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(raw, &probe)

		if !probe.Stream {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fl.Flush()
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2,\"total_tokens\":9}}\n\n")
		fl.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
}

func testConfig(t *testing.T, targets []config.Target, providers []config.Provider) *config.Config {
	t.Helper()
	cfg := &config.Config{
		Providers: providers,
		Classes:   map[string][]config.Target{"coder": targets},
		Defaults:  config.Defaults{Temperature: 0.2, MaxTokens: 256},
		Policy: config.Policy{
			RateLimitCooldownSec:  20,
			QuotaCooldownSec:      3600,
			AuthCooldownSec:       86400,
			ServerCooldownSec:     30,
			BadRequestCooldownSec: 600,
			MaxCooldownSec:        900,
			SameTargetRetries:     0,
		},
		Server:   config.Server{Addr: "127.0.0.1:0", DefaultClass: "coder"},
		StateDir: t.TempDir(),
	}
	return cfg
}

func newTestRouter(t *testing.T, cfg *config.Config) (*Router, *Ledger) {
	t.Helper()
	reg := provider.NewRegistry(cfg)
	health := NewHealth(cfg.Dir(), cfg.Policy)
	ledger, err := NewLedger(cfg.Dir())
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	return New(cfg, reg, health, ledger), ledger
}

// The headline behaviour: a rate-limited provider must be skipped, the next
// one used, and the failed target parked so the next call does not retry it.
func TestFallsOverOnRateLimitAndCoolsDownTheFailedTarget(t *testing.T) {
	var hitsA, hitsB int32
	a := stubServer(429, "7", `{"error":{"message":"rate limit reached"}}`, &hitsA)
	defer a.Close()
	b := stubServer(200, "", "", &hitsB)
	defer b.Close()

	cfg := testConfig(t,
		[]config.Target{{Provider: "a", Model: "m"}, {Provider: "b", Model: "m"}},
		[]config.Provider{
			{Name: "a", BaseURL: a.URL + "/v1", APIKey: "k"},
			{Name: "b", BaseURL: b.URL + "/v1", APIKey: "k"},
		})
	rt, ledger := newTestRouter(t, cfg)

	resp, err := rt.Chat(context.Background(), "coder", provider.Request{
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Provider != "b" {
		t.Errorf("served by %q, want fallback to %q", resp.Provider, "b")
	}
	if atomic.LoadInt32(&hitsA) != 1 || atomic.LoadInt32(&hitsB) != 1 {
		t.Errorf("hits a=%d b=%d, want 1 and 1", hitsA, hitsB)
	}

	// Retry-After: 7 must win over the configured 20s default — undershooting
	// what a provider explicitly asked for is how free tiers get revoked.
	ok, left := rt.Health().Available("a|m", time.Now())
	if ok {
		t.Fatal("target a should be cooling down")
	}
	if left < 6*time.Second || left > 9*time.Second {
		t.Errorf("cooldown %v, want ~8s from Retry-After: 7", left)
	}

	// A second call must not touch the cooling-down target at all.
	if _, err := rt.Chat(context.Background(), "coder", provider.Request{
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "again"}},
	}); err != nil {
		t.Fatalf("second chat: %v", err)
	}
	if got := atomic.LoadInt32(&hitsA); got != 1 {
		t.Errorf("target a was hit %d times, want 1 (should stay parked)", got)
	}

	stats, err := Summarize(ledger.Path(), time.Time{}, func(r Record) string { return r.Provider })
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	byProvider := map[string]Stat{}
	for _, s := range stats {
		byProvider[s.Key] = s
	}
	if byProvider["a"].Failures != 1 {
		t.Errorf("ledger recorded %d failures for a, want 1", byProvider["a"].Failures)
	}
	if byProvider["b"].Calls != 2 || byProvider["b"].Failures != 0 {
		t.Errorf("ledger for b = %+v, want 2 calls 0 failures", byProvider["b"])
	}
}

func TestSkipsUnconfiguredAndOversizedTargets(t *testing.T) {
	var hits int32
	b := stubServer(200, "", "", &hits)
	defer b.Close()

	cfg := testConfig(t,
		[]config.Target{
			{Provider: "nokey", Model: "m"},                 // no API key
			{Provider: "small", Model: "m", MaxContext: 10}, // window too small
			{Provider: "b", Model: "m"},
		},
		[]config.Provider{
			{Name: "nokey", BaseURL: "http://127.0.0.1:1/v1"},
			{Name: "small", BaseURL: "http://127.0.0.1:1/v1", APIKey: "k"},
			{Name: "b", BaseURL: b.URL + "/v1", APIKey: "k"},
		})
	rt, _ := newTestRouter(t, cfg)

	resp, err := rt.Chat(context.Background(), "coder", provider.Request{
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: strings.Repeat("token ", 500)}},
		MaxTokens: 256,
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Provider != "b" {
		t.Errorf("served by %q, want %q", resp.Provider, "b")
	}
	// Neither skipped target should have been dialled at all.
	if hits != 1 {
		t.Errorf("b hits = %d, want 1", hits)
	}
}

func TestAllTargetsFailedReportsEveryReason(t *testing.T) {
	var hitsA, hitsB int32
	a := stubServer(401, "", `{"error":{"message":"bad key"}}`, &hitsA)
	defer a.Close()
	b := stubServer(500, "", `{"error":{"message":"boom"}}`, &hitsB)
	defer b.Close()

	cfg := testConfig(t,
		[]config.Target{{Provider: "a", Model: "m"}, {Provider: "b", Model: "m"}},
		[]config.Provider{
			{Name: "a", BaseURL: a.URL + "/v1", APIKey: "k"},
			{Name: "b", BaseURL: b.URL + "/v1", APIKey: "k"},
		})
	rt, _ := newTestRouter(t, cfg)

	_, err := rt.Chat(context.Background(), "coder", provider.Request{
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected an error when every target fails")
	}
	re, ok := err.(*RouteError)
	if !ok {
		t.Fatalf("error type %T, want *RouteError", err)
	}
	if len(re.Attempts) != 2 {
		t.Fatalf("recorded %d attempts, want 2", len(re.Attempts))
	}
	if re.Attempts[0].Kind != provider.ErrAuth {
		t.Errorf("attempt 0 kind = %v, want auth", re.Attempts[0].Kind)
	}
	if re.Attempts[1].Kind != provider.ErrServer {
		t.Errorf("attempt 1 kind = %v, want server", re.Attempts[1].Kind)
	}
	// The aggregate message must name both providers, not just the last.
	msg := re.Error()
	if !strings.Contains(msg, "a|m") || !strings.Contains(msg, "b|m") {
		t.Errorf("error message omits a target: %s", msg)
	}
}

func TestPinBypassesTheChain(t *testing.T) {
	var hitsA, hitsB int32
	a := stubServer(200, "", "", &hitsA)
	defer a.Close()
	b := stubServer(200, "", "", &hitsB)
	defer b.Close()

	cfg := testConfig(t,
		[]config.Target{{Provider: "a", Model: "m"}},
		[]config.Provider{
			{Name: "a", BaseURL: a.URL + "/v1", APIKey: "k"},
			{Name: "b", BaseURL: b.URL + "/v1", APIKey: "k"},
		})
	rt, _ := newTestRouter(t, cfg)

	resp, err := rt.Chat(context.Background(), "b:m", provider.Request{
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("pinned chat: %v", err)
	}
	if resp.Provider != "b" {
		t.Errorf("pin routed to %q, want %q", resp.Provider, "b")
	}
	if hitsA != 0 {
		t.Errorf("pin should not touch the class chain, but a was hit %d times", hitsA)
	}
}

func TestQuotaGetsALongCooldownNotAShortOne(t *testing.T) {
	h := NewHealth(t.TempDir(), config.Policy{
		RateLimitCooldownSec: 20, QuotaCooldownSec: 3600,
		AuthCooldownSec: 86400, ServerCooldownSec: 30,
		BadRequestCooldownSec: 600, MaxCooldownSec: 900,
	})
	now := time.Now()

	if cd := h.RecordFailure("p|m", provider.ErrQuota, 0, "quota", now); cd < time.Hour {
		t.Errorf("quota cooldown = %v, want >= 1h", cd)
	}
	// Context-length failures say nothing about the target's health: the next
	// request may well fit, so they must not park it.
	if cd := h.RecordFailure("q|m", provider.ErrContextLength, 0, "too long", now); cd != 0 {
		t.Errorf("context-length cooldown = %v, want 0", cd)
	}
	if ok, _ := h.Available("q|m", now); !ok {
		t.Error("context-length failure should leave the target available")
	}
}

func TestHealthPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	pol := config.Policy{RateLimitCooldownSec: 60, MaxCooldownSec: 900, QuotaCooldownSec: 3600, AuthCooldownSec: 86400, ServerCooldownSec: 30, BadRequestCooldownSec: 600}

	h1 := NewHealth(dir, pol)
	h1.RecordFailure("p|m", provider.ErrRateLimit, 0, "429", time.Now())
	if err := h1.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// A restart that forgets cooldowns would immediately re-hammer a provider
	// that just rate-limited you — the fastest way to lose a free tier.
	h2 := NewHealth(dir, pol)
	if ok, left := h2.Available("p|m", time.Now()); ok {
		t.Error("cooldown did not survive restart")
	} else if left <= 0 {
		t.Errorf("restored cooldown = %v, want > 0", left)
	}
}

// A target with room for a useful reply must be used, with its reply budget
// trimmed to fit — not thrown away because the configured max_tokens happens
// not to fit alongside the prompt.
//
// The bug this pins: a ~4,100 token prompt against an 8,192 window was skipped
// as "needs ~4110 tok > 8192 window" because the default 4,096 max_tokens was
// added to the prompt first. There were 4,082 tokens of room. The local model
// — the one target that never runs out of quota — was unusable exactly when
// every hosted target had.
func TestTrimsReplyBudgetInsteadOfSkipping(t *testing.T) {
	var hits int32
	var gotMaxTokens int32 = -1

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			fmt.Fprint(w, `{"data":[{"id":"m"}]}`)
			return
		}
		atomic.AddInt32(&hits, 1)
		raw, _ := io.ReadAll(r.Body)
		var probe struct {
			MaxTokens int `json:"max_tokens"`
		}
		_ = json.Unmarshal(raw, &probe)
		atomic.StoreInt32(&gotMaxTokens, int32(probe.MaxTokens))
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],`+
			`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer srv.Close()

	// A 1000-token window, and a prompt big enough that the requested 4096
	// tokens of output cannot also fit.
	cfg := testConfig(t,
		[]config.Target{{Provider: "local", Model: "m", MaxContext: 1000}},
		[]config.Provider{{Name: "local", BaseURL: srv.URL + "/v1", APIKey: "k"}})
	rt, _ := newTestRouter(t, cfg)

	resp, err := rt.Chat(context.Background(), "coder", provider.Request{
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: strings.Repeat("token ", 200)}},
		MaxTokens: 4096,
	})
	if err != nil {
		t.Fatalf("chat: %v (the target should have been used with a trimmed budget)", err)
	}
	if resp.Provider != "local" {
		t.Errorf("served by %q, want %q", resp.Provider, "local")
	}
	if hits != 1 {
		t.Fatalf("hits = %d, want 1", hits)
	}
	got := int(atomic.LoadInt32(&gotMaxTokens))
	if got <= 0 || got >= 4096 {
		t.Errorf("max_tokens sent = %d, want it trimmed below the requested 4096", got)
	}
	if got < minReplyTokens {
		t.Errorf("max_tokens sent = %d, below the %d floor a call is worth making at",
			got, minReplyTokens)
	}
}

// The floor still holds: a window with no useful room left is skipped rather
// than dialled for a truncated answer.
func TestSkipsWhenNoUsefulReplyRoomRemains(t *testing.T) {
	var hits int32
	tiny := stubServer(200, "", "", &hits)
	defer tiny.Close()
	var okHits int32
	good := stubServer(200, "", "", &okHits)
	defer good.Close()

	cfg := testConfig(t,
		[]config.Target{
			{Provider: "tiny", Model: "m", MaxContext: 300},
			{Provider: "good", Model: "m"},
		},
		[]config.Provider{
			{Name: "tiny", BaseURL: tiny.URL + "/v1", APIKey: "k"},
			{Name: "good", BaseURL: good.URL + "/v1", APIKey: "k"},
		})
	rt, _ := newTestRouter(t, cfg)

	resp, err := rt.Chat(context.Background(), "coder", provider.Request{
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: strings.Repeat("token ", 200)}},
		MaxTokens: 256,
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Provider != "good" {
		t.Errorf("served by %q, want %q", resp.Provider, "good")
	}
	if hits != 0 {
		t.Errorf("the too-small target was dialled %d times, want 0", hits)
	}
}
