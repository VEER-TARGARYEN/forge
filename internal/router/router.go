// Package router turns a class name ("planner", "coder", "cheap") into an
// actual model call, walking an ordered chain of providers and demoting any
// target that rate-limits, runs out of quota, or breaks. It is the piece that
// makes a stack of free tiers behave like one endpoint that never runs out.
package router

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/VEER-TARGARYEN/forge/internal/config"
	"github.com/VEER-TARGARYEN/forge/internal/provider"
)

type Router struct {
	cfg    *config.Config
	reg    *provider.Registry
	health *Health
	ledger *Ledger
	// Verbose prints each attempt and demotion to stderr via the hook below.
	OnEvent func(Event)
}

// Event reports routing decisions so the CLI can show what happened without
// the router importing a logger.
type Event struct {
	Kind     string // "attempt", "success", "failure", "skip"
	Class    string
	Provider string
	Model    string
	Attempt  int
	Detail   string
	Cooldown time.Duration
}

func New(cfg *config.Config, reg *provider.Registry, health *Health, ledger *Ledger) *Router {
	return &Router{cfg: cfg, reg: reg, health: health, ledger: ledger}
}

func (r *Router) Config() *config.Config       { return r.cfg }
func (r *Router) Health() *Health              { return r.health }
func (r *Router) Registry() *provider.Registry { return r.reg }

func (r *Router) emit(e Event) {
	if r.OnEvent != nil {
		r.OnEvent(e)
	}
}

// Resolve returns the target chain for a class. It also accepts a pin of the
// form "provider:model", which bypasses the chain entirely — useful for
// benchmarking one backend or forcing a specific model from the CLI.
func (r *Router) Resolve(class string) ([]config.Target, error) {
	if p, m, ok := strings.Cut(class, ":"); ok && m != "" {
		if _, exists := r.cfg.ProviderByName(p); !exists {
			return nil, fmt.Errorf("pinned provider %q not in config", p)
		}
		return []config.Target{{Provider: p, Model: m}}, nil
	}
	targets, ok := r.cfg.Classes[class]
	if !ok {
		return nil, fmt.Errorf("unknown class %q (have: %s)", class, strings.Join(r.cfg.ClassNames(), ", "))
	}
	return targets, nil
}

// ClassContext reports the largest declared context window among a class's
// usable targets, or 0 when nothing is configured or no window is declared.
//
// The agent uses this to size its compaction threshold. It deliberately takes
// the maximum rather than the minimum: compacting down to the smallest link in
// the chain would throw away context a 1M-window planner could have used, and
// a request that no longer fits a smaller target is skipped by the router
// anyway rather than failing.
func (r *Router) ClassContext(class string) int {
	targets, err := r.Resolve(class)
	if err != nil {
		return 0
	}
	best := 0
	for _, t := range targets {
		p, ok := r.reg.Get(t.Provider)
		if !ok || !p.Configured() {
			continue
		}
		if t.MaxContext > best {
			best = t.MaxContext
		}
	}
	return best
}

// Attempt records what happened at one target during a routed call.
type Attempt struct {
	Target config.Target
	Err    error
	Kind   provider.ErrKind
	Skip   string
}

// RouteError aggregates every failed attempt so the message tells you which
// providers were tried and why each one was rejected, rather than surfacing
// only the last error.
type RouteError struct {
	Class    string
	Attempts []Attempt
}

func (e *RouteError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "class %q: all %d target(s) failed", e.Class, len(e.Attempts))
	for _, a := range e.Attempts {
		b.WriteString("\n  - ")
		b.WriteString(a.Target.Key())
		b.WriteString(": ")
		switch {
		case a.Skip != "":
			b.WriteString("skipped (" + a.Skip + ")")
		case a.Err != nil:
			b.WriteString(a.Err.Error())
		}
	}
	return b.String()
}

// Unwrap returns the last real error, so errors.As on provider.Error works.
func (e *RouteError) Unwrap() error {
	for i := len(e.Attempts) - 1; i >= 0; i-- {
		if e.Attempts[i].Err != nil {
			return e.Attempts[i].Err
		}
	}
	return nil
}

// Chat runs a non-streaming completion through the class chain.
func (r *Router) Chat(ctx context.Context, class string, req provider.Request) (*provider.Response, error) {
	return r.run(ctx, class, req, nil)
}

// Stream runs a streaming completion through the class chain.
//
// Fallback stops the moment any output has been handed to onChunk: silently
// switching providers mid-stream would splice two different answers together.
// A failure after first output is returned as an error, not retried.
func (r *Router) Stream(ctx context.Context, class string, req provider.Request, onChunk func(provider.Chunk) error) (*provider.Response, error) {
	if onChunk == nil {
		return r.run(ctx, class, req, func(provider.Chunk) error { return nil })
	}
	return r.run(ctx, class, req, onChunk)
}

func (r *Router) run(ctx context.Context, class string, req provider.Request, onChunk func(provider.Chunk) error) (*provider.Response, error) {
	targets, err := r.Resolve(class)
	if err != nil {
		return nil, err
	}
	r.applyDefaults(&req)

	routeErr := &RouteError{Class: class}
	estTokens := req.TokenEstimate()
	attemptNo := 0

	for _, t := range targets {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		p, ok := r.reg.Get(t.Provider)
		if !ok {
			routeErr.Attempts = append(routeErr.Attempts, Attempt{Target: t, Skip: "provider disabled"})
			r.emit(Event{Kind: "skip", Class: class, Provider: t.Provider, Model: t.Model, Detail: "disabled"})
			continue
		}
		if !p.Configured() {
			routeErr.Attempts = append(routeErr.Attempts, Attempt{Target: t, Skip: "no API key"})
			r.emit(Event{Kind: "skip", Class: class, Provider: t.Provider, Model: t.Model, Detail: "no API key"})
			continue
		}
		now := time.Now()
		if ok, left := r.health.Available(t.Key(), now); !ok {
			reason := fmt.Sprintf("cooling down %s", left.Round(time.Second))
			routeErr.Attempts = append(routeErr.Attempts, Attempt{Target: t, Skip: reason})
			r.emit(Event{Kind: "skip", Class: class, Provider: t.Provider, Model: t.Model, Detail: reason, Cooldown: left})
			continue
		}
		// Reserve headroom for the reply; a target whose window cannot hold
		// prompt + output is not worth a round trip.
		if t.MaxContext > 0 && estTokens+r.outputBudget(t, req) > t.MaxContext {
			reason := fmt.Sprintf("needs ~%d tok > %d window", estTokens, t.MaxContext)
			routeErr.Attempts = append(routeErr.Attempts, Attempt{Target: t, Skip: reason})
			r.emit(Event{Kind: "skip", Class: class, Provider: t.Provider, Model: t.Model, Detail: reason})
			continue
		}

		attemptReq := req
		if t.MaxTokens > 0 {
			attemptReq.MaxTokens = t.MaxTokens
		}

		retries := r.cfg.Policy.SameTargetRetries
		waits := 0
		for try := 0; ; try++ {
			attemptNo++
			r.emit(Event{Kind: "attempt", Class: class, Provider: t.Provider, Model: t.Model, Attempt: attemptNo})

			emitted := false
			var wrapped func(provider.Chunk) error
			if onChunk != nil {
				wrapped = func(c provider.Chunk) error {
					emitted = true
					return onChunk(c)
				}
			}

			start := time.Now()
			var (
				resp    *provider.Response
				callErr error
			)
			if wrapped != nil {
				resp, callErr = p.Stream(ctx, t.Model, attemptReq, wrapped)
			} else {
				resp, callErr = p.Chat(ctx, t.Model, attemptReq)
			}

			if callErr == nil {
				r.health.RecordSuccess(t.Key(), time.Now())
				_ = r.health.Flush()
				r.ledger.Append(Record{
					TS: start, Class: class, Provider: t.Provider, Model: t.Model, OK: true,
					PromptTok: resp.Usage.PromptTokens, OutTok: resp.Usage.CompletionTokens,
					Estimated: resp.Usage.Estimated,
					LatencyMS: resp.Latency.Milliseconds(), TTFTMS: resp.TTFT.Milliseconds(),
					Attempt: attemptNo, FellBackTo: attemptNo > 1,
				})
				r.emit(Event{Kind: "success", Class: class, Provider: t.Provider, Model: t.Model, Attempt: attemptNo})
				return resp, nil
			}

			kind := provider.KindOf(callErr)
			if kind == provider.ErrCanceled {
				return nil, callErr
			}

			var retryAfter time.Duration
			var pe *provider.Error
			if ok := asProviderErr(callErr, &pe); ok {
				retryAfter = pe.RetryAfter
			}

			// A short, explicit Retry-After is an instruction, not a failure.
			// Free tiers throttle on tokens per minute, so mid-run a provider
			// will say "try again in 1.2s"; demoting the target for that would
			// end runs over a pause shorter than one model call. Waited out
			// inline, without recording a failure, because nothing is wrong
			// with the target.
			maxWait := time.Duration(r.cfg.Policy.RateLimitWaitSec) * time.Second
			if kind == provider.ErrRateLimit && retryAfter > 0 && retryAfter <= maxWait &&
				waits < r.cfg.Policy.RateLimitWaits && !emitted {
				waits++
				r.emit(Event{Kind: "wait", Class: class, Provider: t.Provider, Model: t.Model,
					Detail:   fmt.Sprintf("rate limited, waiting %s", retryAfter.Round(100*time.Millisecond)),
					Cooldown: retryAfter})
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(retryAfter + 250*time.Millisecond):
				}
				continue
			}
			cd := r.health.RecordFailure(t.Key(), kind, retryAfter, callErr.Error(), time.Now())
			_ = r.health.Flush()
			r.ledger.Append(Record{
				TS: start, Class: class, Provider: t.Provider, Model: t.Model, OK: false,
				ErrKind: kind.String(), Err: truncate(callErr.Error(), 400),
				LatencyMS: time.Since(start).Milliseconds(), Attempt: attemptNo,
			})
			r.emit(Event{Kind: "failure", Class: class, Provider: t.Provider, Model: t.Model,
				Attempt: attemptNo, Detail: callErr.Error(), Cooldown: cd})

			// Partial output already reached the caller; falling through to
			// another provider would concatenate two different answers.
			if emitted {
				routeErr.Attempts = append(routeErr.Attempts, Attempt{Target: t, Err: callErr, Kind: kind})
				return nil, fmt.Errorf("stream failed after partial output from %s: %w", t.Key(), callErr)
			}

			if kind.Retryable() && try < retries {
				backoff := time.Duration(1<<try) * time.Second
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(backoff):
				}
				continue
			}

			routeErr.Attempts = append(routeErr.Attempts, Attempt{Target: t, Err: callErr, Kind: kind})
			break
		}
	}

	return nil, routeErr
}

// Embed vectorises inputs through a class chain, with the same skip, cooldown,
// and ledger behaviour as a chat call. Targets whose provider cannot embed are
// skipped rather than attempted.
func (r *Router) Embed(ctx context.Context, class string, inputs []string) (*provider.EmbedResponse, error) {
	targets, err := r.Resolve(class)
	if err != nil {
		return nil, err
	}
	routeErr := &RouteError{Class: class}

	for _, t := range targets {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p, ok := r.reg.Get(t.Provider)
		if !ok {
			routeErr.Attempts = append(routeErr.Attempts, Attempt{Target: t, Skip: "provider disabled"})
			continue
		}
		if !p.Configured() {
			routeErr.Attempts = append(routeErr.Attempts, Attempt{Target: t, Skip: "no API key"})
			continue
		}
		emb, ok := p.(provider.Embedder)
		if !ok {
			routeErr.Attempts = append(routeErr.Attempts, Attempt{Target: t, Skip: "cannot embed"})
			continue
		}
		if ok, left := r.health.Available(t.Key(), time.Now()); !ok {
			routeErr.Attempts = append(routeErr.Attempts,
				Attempt{Target: t, Skip: fmt.Sprintf("cooling down %s", left.Round(time.Second))})
			continue
		}

		start := time.Now()
		r.emit(Event{Kind: "attempt", Class: class, Provider: t.Provider, Model: t.Model})
		resp, callErr := emb.Embed(ctx, t.Model, inputs)
		if callErr == nil {
			r.health.RecordSuccess(t.Key(), time.Now())
			_ = r.health.Flush()
			r.ledger.Append(Record{
				TS: start, Class: class, Provider: t.Provider, Model: t.Model, OK: true,
				PromptTok: resp.Usage.PromptTokens, LatencyMS: resp.Latency.Milliseconds(),
			})
			r.emit(Event{Kind: "success", Class: class, Provider: t.Provider, Model: t.Model})
			return resp, nil
		}

		kind := provider.KindOf(callErr)
		if kind == provider.ErrCanceled {
			return nil, callErr
		}
		var retryAfter time.Duration
		var pe *provider.Error
		if ok := asProviderErr(callErr, &pe); ok {
			retryAfter = pe.RetryAfter
		}
		cd := r.health.RecordFailure(t.Key(), kind, retryAfter, callErr.Error(), time.Now())
		_ = r.health.Flush()
		r.ledger.Append(Record{
			TS: start, Class: class, Provider: t.Provider, Model: t.Model, OK: false,
			ErrKind: kind.String(), Err: truncate(callErr.Error(), 400),
			LatencyMS: time.Since(start).Milliseconds(),
		})
		r.emit(Event{Kind: "failure", Class: class, Provider: t.Provider, Model: t.Model,
			Detail: callErr.Error(), Cooldown: cd})
		routeErr.Attempts = append(routeErr.Attempts, Attempt{Target: t, Err: callErr, Kind: kind})
	}
	return nil, routeErr
}

// ClassUsable reports whether a class has at least one target that could
// actually serve a request right now.
//
// This is deliberately distinct from Resolve, which only reports whether the
// class is *defined*. A config can name a class whose every target lacks an
// API key — the default config's "cheap" chain does exactly that until a
// provider is configured — and callers that fall back on class existence alone
// would route to something guaranteed to fail.
func (r *Router) ClassUsable(class string) bool {
	targets, err := r.Resolve(class)
	if err != nil {
		return false
	}
	for _, t := range targets {
		p, ok := r.reg.Get(t.Provider)
		if ok && p.Configured() {
			return true
		}
	}
	return false
}

// CanEmbed reports whether any target in a class is currently able to embed.
// Callers use it to degrade to keyword-only search instead of failing.
func (r *Router) CanEmbed(class string) bool {
	targets, err := r.Resolve(class)
	if err != nil {
		return false
	}
	for _, t := range targets {
		p, ok := r.reg.Get(t.Provider)
		if !ok || !p.Configured() {
			continue
		}
		if _, ok := p.(provider.Embedder); ok {
			return true
		}
	}
	return false
}

func (r *Router) applyDefaults(req *provider.Request) {
	if req.Temperature == 0 {
		req.Temperature = r.cfg.Defaults.Temperature
	}
	if req.MaxTokens == 0 {
		req.MaxTokens = r.cfg.Defaults.MaxTokens
	}
}

func (r *Router) outputBudget(t config.Target, req provider.Request) int {
	if t.MaxTokens > 0 {
		return t.MaxTokens
	}
	if req.MaxTokens > 0 {
		return req.MaxTokens
	}
	return r.cfg.Defaults.MaxTokens
}

// asProviderErr is a tiny errors.As wrapper kept local so router.go does not
// need to import errors just for one call site.
func asProviderErr(err error, out **provider.Error) bool {
	for err != nil {
		if pe, ok := err.(*provider.Error); ok {
			*out = pe
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
