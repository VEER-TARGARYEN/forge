package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ErrKind drives router policy. The whole point of classifying errors is that
// "rate limited" and "bad model name" demand opposite reactions: one means
// come back later, the other means never try this target again today.
type ErrKind int

const (
	ErrUnknown ErrKind = iota
	ErrRateLimit
	ErrQuota
	ErrAuth
	ErrContextLength
	ErrBadRequest
	ErrModelNotFound
	ErrServer
	ErrTimeout
	ErrNetwork
	ErrCanceled
)

var errKindNames = map[ErrKind]string{
	ErrUnknown:       "unknown",
	ErrRateLimit:     "rate_limit",
	ErrQuota:         "quota",
	ErrAuth:          "auth",
	ErrContextLength: "context_length",
	ErrBadRequest:    "bad_request",
	ErrModelNotFound: "model_not_found",
	ErrServer:        "server",
	ErrTimeout:       "timeout",
	ErrNetwork:       "network",
	ErrCanceled:      "canceled",
}

func (k ErrKind) String() string {
	if s, ok := errKindNames[k]; ok {
		return s
	}
	return "unknown"
}

// Retryable reports whether trying the *same* target again could plausibly
// work within a few seconds.
func (k ErrKind) Retryable() bool {
	return k == ErrServer || k == ErrNetwork || k == ErrTimeout
}

// Fatal reports whether this target is misconfigured rather than busy.
func (k ErrKind) Fatal() bool {
	return k == ErrAuth || k == ErrModelNotFound
}

type Error struct {
	Kind       ErrKind
	Provider   string
	Model      string
	Status     int
	Body       string
	RetryAfter time.Duration
	Err        error
}

func (e *Error) Error() string {
	b := strings.TrimSpace(e.Body)
	if len(b) > 300 {
		b = b[:300] + "…"
	}
	switch {
	case e.Status > 0 && b != "":
		return fmt.Sprintf("%s/%s: %s (http %d): %s", e.Provider, e.Model, e.Kind, e.Status, b)
	case e.Status > 0:
		return fmt.Sprintf("%s/%s: %s (http %d)", e.Provider, e.Model, e.Kind, e.Status)
	case e.Err != nil:
		return fmt.Sprintf("%s/%s: %s: %v", e.Provider, e.Model, e.Kind, e.Err)
	default:
		return fmt.Sprintf("%s/%s: %s", e.Provider, e.Model, e.Kind)
	}
}

func (e *Error) Unwrap() error { return e.Err }

// KindOf extracts the classification from any error in the chain, defaulting
// to ErrUnknown for non-provider errors.
func KindOf(err error) ErrKind {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Kind
	}
	return ErrUnknown
}

// classifyHTTP maps a response status and body to an ErrKind. Providers are
// wildly inconsistent about which status they use for quota exhaustion, so we
// look at the body text too — carefully.
//
// retryAfter is the value the provider supplied, and it outranks the body
// text: a provider that tells us exactly when to come back is describing a
// temporary limit, whatever words surround it.
func classifyHTTP(status int, body string, retryAfter time.Duration) ErrKind {
	// URLs are stripped before any keyword matching. Groq's 429 body ends
	// with an upgrade link whose path contains "billing", which read as quota
	// exhaustion turns a 51-second rate limit into an hour-long block. Nothing
	// in a hyperlink should ever drive retry policy.
	lb := strings.ToLower(stripURLs(body))

	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		// Some gateways return 403 for regional blocks rather than bad keys,
		// but both mean "this target is unusable", which is the same policy.
		return ErrAuth
	case http.StatusNotFound:
		return ErrModelNotFound
	case http.StatusRequestEntityTooLarge:
		// Groq answers "your request exceeds the per-minute token allowance"
		// with a 413. That is a rate limit wearing a context-overflow status
		// code: the same request succeeds a minute later, so compacting the
		// conversation to fit is the wrong response.
		if containsAny(lb, "per minute", "tpm", "tokens per minute", "rate limit") {
			return ErrRateLimit
		}
		return ErrContextLength
	case http.StatusTooManyRequests:
		// An explicit, short Retry-After is definitive: this is a per-minute
		// throttle, not an exhausted allowance.
		if retryAfter > 0 && retryAfter <= 5*time.Minute {
			return ErrRateLimit
		}
		if containsAny(lb, "quota", "daily limit", "exhausted", "credits", "billing", "insufficient_quota", "out of credit") {
			return ErrQuota
		}
		return ErrRateLimit
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		if containsAny(lb, "context length", "context_length", "maximum context", "too many tokens",
			"reduce the length", "input is too long", "exceeds the maximum", "token limit") {
			return ErrContextLength
		}
		if containsAny(lb, "model not found", "unknown model", "does not exist", "invalid model", "no such model") {
			return ErrModelNotFound
		}
		if containsAny(lb, "quota", "insufficient", "credits") {
			return ErrQuota
		}
		return ErrBadRequest
	case http.StatusPaymentRequired:
		return ErrQuota
	}

	if status >= 500 {
		return ErrServer
	}
	if status >= 400 {
		return ErrBadRequest
	}
	return ErrUnknown
}

// classifyTransport maps a Go transport-level failure.
func classifyTransport(err error) ErrKind {
	switch {
	case err == nil:
		return ErrUnknown
	case errors.Is(err, context.Canceled):
		return ErrCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return ErrTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ErrTimeout
	}
	return ErrNetwork
}

// parseRetryAfter understands both the integer-seconds and HTTP-date forms,
// plus the fractional seconds Groq sometimes returns.
func parseRetryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		v = strings.TrimSpace(h.Get("X-Ratelimit-Reset-Requests"))
	}
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil && secs >= 0 {
		return time.Duration(secs * float64(time.Second))
	}
	// Forms like "2.5s" or "1m30s".
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// urlPattern matches http(s) links so they can be removed before the body is
// searched for classification keywords.
var urlPattern = regexp.MustCompile(`https?://\S+`)

func stripURLs(s string) string { return urlPattern.ReplaceAllString(s, " ") }

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}
