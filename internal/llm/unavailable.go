package llm

import (
	"errors"
	"slices"
	"strings"
	"time"
)

// UnavailableError is a call that no model in the chain could take,
// because every provider in it turned the call down for credit (billing)
// or a rejected key or login (auth). Retrying cannot help until someone
// tops up or fixes a key, so the job queue doesn't retry it, and its
// Message says in plain words what to do.
type UnavailableError struct {
	Message string // for the user: which providers, why, and what to do
	// Key names the providers and reasons ("unavailable:anthropic=billing,
	// deepseek=billing"), so background work tells the user once per
	// outage (Router.UnavailableNotice) instead of once per run.
	Key string
	Err error // each model's own error, for the log
}

func (e *UnavailableError) Error() string { return e.Message }
func (e *UnavailableError) Unwrap() error { return e.Err }

// Permanent tells the job queue not to retry a job that failed with it.
func (e *UnavailableError) Permanent() bool { return true }

// unavailableAgain is how long a notice for one outage stays given: while
// the same providers stay refused, background work says so again after it.
const unavailableAgain = 24 * time.Hour

// refreshError is a login whose token couldn't be refreshed for a reason
// other than the login being gone (LoginGone). It counts as an auth
// failure for falling back, but never as an outage: the login server may
// only be down for a moment.
type refreshError struct{ error }

func (e refreshError) Unwrap() error { return e.error }

// LoginGone reports whether err says a login is missing, expired or
// revoked, so only signing in again helps (auth.ErrNotLoggedIn and
// auth.ErrLoginExpired say so through a LoginGone method).
func LoginGone(err error) bool {
	var g interface{ LoginGone() bool }
	return errors.As(err, &g) && g.LoginGone()
}

// refusals collects, for one Send, the providers that refused the call for
// credit or a rejected key, in chain order.
type refusals struct {
	order []string
	kind  map[string]ErrorKind
	other bool // some model failed for another reason
}

func (f *refusals) add(endpoint string, k ErrorKind) {
	if k != ErrBilling && k != ErrAuth {
		f.other = true
		return
	}
	if f.kind == nil {
		f.kind = map[string]ErrorKind{}
	}
	prev, seen := f.kind[endpoint]
	if !seen {
		f.order = append(f.order, endpoint)
	}
	// One provider can appear twice in a chain. Billing names it if
	// either time said so, so the answer doesn't depend on chain order.
	if prev != ErrBilling {
		f.kind[endpoint] = k
	}
}

// err is the error that ends a Send: an UnavailableError when every model
// was refused for credit or a key, else every model's error joined.
func (f *refusals) err(errs []error) error {
	joined := errors.Join(errs...)
	if f.other || len(f.order) == 0 {
		return joined
	}
	var parts, keys []string
	for _, name := range f.order {
		switch f.kind[name] {
		case ErrBilling:
			parts = append(parts, name+" is out of credit")
		default:
			parts = append(parts, name+" turned down its key or login")
		}
		keys = append(keys, name+"="+string(f.kind[name]))
	}
	slices.Sort(keys)
	return &UnavailableError{
		Message: "no model could answer: " + andList(parts) + ". Top up or fix the key for one of them, " +
			"or add a model you can use to this agent's fallbacks in config.yaml.",
		Key: "unavailable:" + strings.Join(keys, ","),
		Err: joined,
	}
}

// andList joins "a", "b" and "c" as "a, b and c".
func andList(s []string) string {
	if len(s) < 2 {
		return strings.Join(s, "")
	}
	return strings.Join(s[:len(s)-1], ", ") + " and " + s[len(s)-1]
}

// noCredentials is the error for an endpoint none of whose credentials can
// be used now. When every one of them is set aside for credit or a
// rejected key, the error says so and classifies that way (billing if any
// key is out of credit), so an empty account isn't called a rate limit.
// If any key is set aside for another reason, such as a rate limit, it's
// the usual cooling-down error, which is retried.
func (r *Router) noCredentials(ep *Endpoint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	configured, billing, auth := 0, 0, 0
	for _, c := range ep.Credentials {
		if c.Key == "" && c.Token == nil {
			continue
		}
		configured++
		switch r.coolKind[ep.Name+"/"+c.Name] {
		case ErrBilling:
			billing++
		case ErrAuth:
			auth++
		}
	}
	switch {
	case configured == 0:
		return &APIError{Status: 401, Message: ep.Name + " has no key or login set"}
	case billing+auth < configured:
		return &APIError{Status: 429, Message: "all credentials cooling down for " + ep.Name}
	case billing > 0:
		return &APIError{Status: 402, Message: ep.Name + " is out of credit (its keys are set aside for now)"}
	}
	return &APIError{Status: 401, Message: ep.Name + " turned down every key or login (set aside for now)"}
}

// UnavailableNotice reports whether background work (a schedule, a board
// card) should tell the chat named by where about an outage, named by an
// UnavailableError's Key, and marks it told. Each chat is told the first
// time, then again only after a day while the outage lasts. A successful
// call to any of its providers ends the outage, so the next one is told at
// once. The record is kept in memory: a restart tells once more.
func (r *Router) UnavailableNotice(key, where string) bool {
	r.defaults()
	r.mu.Lock()
	defer r.mu.Unlock()
	k := key + "\x00" + where
	if at, ok := r.told[k]; ok && time.Since(at) < unavailableAgain {
		return false
	}
	r.told[k] = time.Now()
	return true
}

// recovered forgets the outages endpoint was part of, after a call to it
// worked.
func (r *Router) recovered(endpoint string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for told := range r.told {
		key, _, _ := strings.Cut(told, "\x00")
		for _, part := range strings.Split(strings.TrimPrefix(key, "unavailable:"), ",") {
			if name, _, _ := strings.Cut(part, "="); name == endpoint {
				delete(r.told, told)
				break
			}
		}
	}
}
