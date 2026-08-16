package router

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/VEER-TARGARYEN/forge/internal/config"
	"github.com/VEER-TARGARYEN/forge/internal/provider"
)

// Entry is the health record for one (provider, model) target.
type Entry struct {
	CooldownUntil time.Time `json:"cooldown_until,omitzero"`
	Fails         int       `json:"fails,omitempty"`
	LastErrKind   string    `json:"last_err_kind,omitempty"`
	LastErr       string    `json:"last_err,omitempty"`
	LastOK        time.Time `json:"last_ok,omitzero"`
	Calls         int       `json:"calls,omitempty"`
}

// Health tracks per-target cooldowns and persists them, so restarting forge
// does not immediately re-hammer a provider that just rate-limited you. That
// persistence is the difference between "free tier" and "banned free tier".
type Health struct {
	mu      sync.Mutex
	entries map[string]*Entry
	path    string
	policy  config.Policy
	dirty   bool
}

func NewHealth(dir string, policy config.Policy) *Health {
	h := &Health{
		entries: map[string]*Entry{},
		path:    filepath.Join(dir, "health.json"),
		policy:  policy,
	}
	h.load()
	return h
}

func (h *Health) load() {
	raw, err := os.ReadFile(h.path)
	if err != nil {
		return
	}
	var m map[string]*Entry
	if err := json.Unmarshal(raw, &m); err != nil {
		return
	}
	for k, v := range m {
		if v != nil {
			h.entries[k] = v
		}
	}
}

// Flush writes state to disk. Cheap enough to call after every request.
func (h *Health) Flush() error {
	h.mu.Lock()
	if !h.dirty {
		h.mu.Unlock()
		return nil
	}
	b, err := json.MarshalIndent(h.entries, "", "  ")
	h.dirty = false
	h.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(h.path), 0o755); err != nil {
		return err
	}
	tmp := h.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, h.path)
}

func (h *Health) get(key string) *Entry {
	e, ok := h.entries[key]
	if !ok {
		e = &Entry{}
		h.entries[key] = e
	}
	return e
}

// Available reports whether a target may be attempted now, and if not, how
// long remains on its cooldown.
func (h *Health) Available(key string, now time.Time) (bool, time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.entries[key]
	if !ok || e.CooldownUntil.IsZero() || now.After(e.CooldownUntil) {
		return true, 0
	}
	return false, e.CooldownUntil.Sub(now)
}

func (h *Health) Snapshot() map[string]Entry {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]Entry, len(h.entries))
	for k, v := range h.entries {
		out[k] = *v
	}
	return out
}

func (h *Health) RecordSuccess(key string, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.get(key)
	e.Fails = 0
	e.LastErr = ""
	e.LastErrKind = ""
	e.CooldownUntil = time.Time{}
	e.LastOK = now
	e.Calls++
	h.dirty = true
}

// RecordFailure applies the cooldown policy for an error kind and returns the
// cooldown that was set.
func (h *Health) RecordFailure(key string, kind provider.ErrKind, retryAfter time.Duration, msg string, now time.Time) time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.get(key)
	e.Fails++
	e.LastErrKind = kind.String()
	e.LastErr = msg
	e.Calls++
	h.dirty = true

	cd := h.cooldownFor(kind, e.Fails, retryAfter)
	if cd <= 0 {
		e.CooldownUntil = time.Time{}
		return 0
	}
	e.CooldownUntil = now.Add(cd)
	return cd
}

// Clear removes cooldowns, used by `forge doctor --reset`.
func (h *Health) Clear() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries = map[string]*Entry{}
	h.dirty = true
}

// cooldownFor implements the policy table. Exponential backoff on the
// transient kinds; long fixed penalties on the structural ones. A provider's
// own Retry-After always wins when it gives one, because guessing shorter
// than the provider asked is how you get a longer ban.
func (h *Health) cooldownFor(kind provider.ErrKind, fails int, retryAfter time.Duration) time.Duration {
	max := time.Duration(h.policy.MaxCooldownSec) * time.Second

	var base time.Duration
	switch kind {
	case provider.ErrRateLimit:
		if retryAfter > 0 {
			// Add a small margin; providers round down.
			return retryAfter + time.Second
		}
		base = time.Duration(h.policy.RateLimitCooldownSec) * time.Second
	case provider.ErrQuota:
		// Daily quota: do not exponentiate, just wait it out.
		return time.Duration(h.policy.QuotaCooldownSec) * time.Second
	case provider.ErrAuth:
		// Structural misconfiguration. Park it for the day and let
		// `forge doctor` surface the reason.
		return time.Duration(h.policy.AuthCooldownSec) * time.Second
	case provider.ErrModelNotFound:
		// A missing model is not a missing credential. On a local backend it
		// usually means the weights have not been pulled yet, which is fixed
		// in minutes — parking the target for a day then hides the model that
		// has since arrived, and the run fails for a reason that is no longer
		// true. Sit out the current run and re-check on the next one.
		return time.Duration(h.policy.ModelNotFoundCooldownSec) * time.Second
	case provider.ErrBadRequest:
		return time.Duration(h.policy.BadRequestCooldownSec) * time.Second
	case provider.ErrServer, provider.ErrNetwork, provider.ErrTimeout:
		base = time.Duration(h.policy.ServerCooldownSec) * time.Second
	case provider.ErrContextLength:
		// Not the target's fault in any lasting sense — the next request may
		// well fit. Skip it for this call only.
		return 0
	case provider.ErrCanceled:
		return 0
	default:
		base = time.Duration(h.policy.ServerCooldownSec) * time.Second
	}

	cd := base
	for i := 1; i < fails && cd < max; i++ {
		cd *= 2
	}
	if cd > max {
		cd = max
	}
	return cd
}
