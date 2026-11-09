package llm

import (
	"context"
	"errors"
	"net"
	"strings"
)

type ErrorKind string

const (
	ErrRateLimit       ErrorKind = "rate_limit"
	ErrOverloaded      ErrorKind = "overloaded"
	ErrServer          ErrorKind = "server"
	ErrNetwork         ErrorKind = "network"
	ErrAuth            ErrorKind = "auth"
	ErrBilling         ErrorKind = "billing"
	ErrContextOverflow ErrorKind = "context_overflow"
	ErrBadRequest      ErrorKind = "bad_request"
	ErrNotFound        ErrorKind = "not_found"
	ErrCanceled        ErrorKind = "canceled"
	ErrBudget          ErrorKind = "budget"
	ErrUnknown         ErrorKind = "unknown"
)

// APIError is what adapters return for an HTTP failure from a provider.
type APIError struct {
	Status  int
	Message string
	Err     error
}

func (e *APIError) Error() string { return e.Message }
func (e *APIError) Unwrap() error { return e.Err }

// BudgetError is a model call refused before it was sent because a
// spending limit is used up. Retrying cannot help until the period resets or
// the limit is raised, so it is never retried and ends a turn with Message.
type BudgetError struct {
	Message string // for the user: which limit, when it resets, how to raise it
	// Key names the limit and its period ("agent:main:daily:2026-11-09"),
	// so background work can tell the user once per period.
	Key string
	// Unpriced: the model has no known price and budget.unknown_price is
	// block. Another model in the chain may still be allowed.
	Unpriced bool
}

func (e *BudgetError) Error() string { return e.Message }

// Permanent tells the job queue not to retry a job that failed with it.
func (e *BudgetError) Permanent() bool { return true }

// Action is what the router should do about an error.
type Action struct {
	Kind     ErrorKind
	Retry    bool // same model, same key, after backoff
	Rotate   bool // try the next credential for this provider
	Fallback bool // move to the next model in the chain
}

// Classify maps an error to the router action. The rules come from what
// actually recovers in production: rate limits recover on another key or after
// waiting, overloads on another model, auth and billing only on another key or
// provider, and bad requests never by retrying the same request.
func Classify(err error) Action {
	if err == nil {
		return Action{}
	}
	if errors.Is(err, context.Canceled) {
		return Action{Kind: ErrCanceled}
	}
	if errors.As(err, new(*BudgetError)) {
		return Action{Kind: ErrBudget}
	}
	var api *APIError
	if errors.As(err, &api) {
		msg := strings.ToLower(api.Message)
		switch {
		case api.Status == 429:
			return Action{Kind: ErrRateLimit, Retry: true, Rotate: true, Fallback: true}
		case api.Status == 529 || strings.Contains(msg, "overloaded"):
			return Action{Kind: ErrOverloaded, Retry: true, Fallback: true}
		case api.Status == 401 || api.Status == 403:
			return Action{Kind: ErrAuth, Rotate: true, Fallback: true}
		case api.Status == 402 || strings.Contains(msg, "credit balance") || strings.Contains(msg, "billing"):
			return Action{Kind: ErrBilling, Rotate: true, Fallback: true}
		case api.Status == 404:
			return Action{Kind: ErrNotFound, Fallback: true}
		case api.Status == 413 || isContextOverflow(msg):
			return Action{Kind: ErrContextOverflow, Fallback: true}
		case api.Status == 408 || api.Status >= 500:
			return Action{Kind: ErrServer, Retry: true, Fallback: true}
		case api.Status >= 400:
			return Action{Kind: ErrBadRequest}
		}
	}
	// Errors delivered mid-stream arrive as SSE error events without a status.
	if msg := strings.ToLower(err.Error()); strings.Contains(msg, "overloaded") {
		return Action{Kind: ErrOverloaded, Retry: true, Fallback: true}
	} else if strings.Contains(msg, "rate_limit") || strings.Contains(msg, "rate limit") {
		return Action{Kind: ErrRateLimit, Retry: true, Rotate: true, Fallback: true}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Action{Kind: ErrNetwork, Retry: true, Fallback: true}
	}
	var netErr net.Error
	if errors.As(err, &netErr) || strings.Contains(err.Error(), "connection reset") ||
		strings.Contains(err.Error(), "unexpected EOF") {
		return Action{Kind: ErrNetwork, Retry: true, Fallback: true}
	}
	return Action{Kind: ErrUnknown, Retry: true}
}

func isContextOverflow(msg string) bool {
	for _, s := range []string{"prompt is too long", "context length", "context window", "maximum context", "too many tokens"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}
