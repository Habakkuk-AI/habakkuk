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
	if _, seen := f.kind[endpoint]; !seen {
		f.order = append(f.order, endpoint)
	}
	f.kind[endpoint] = k
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
// be used now. When every one of them is set aside for the same reason,
// the error says which (credit or a rejected key) and classifies as that,
// so the turn's message doesn't call an empty account a rate limit.
func (r *Router) noCredentials(ep *Endpoint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var kind ErrorKind
	same, configured := true, 0
	for _, c := range ep.Credentials {
		if c.Key == "" && c.Token == nil {
			continue
		}
		configured++
		k := r.coolKind[ep.Name+"/"+c.Name]
		if configured == 1 {
			kind = k
		} else if k != kind {
			same = false
		}
	}
	switch {
	case configured == 0:
		return &APIError{Status: 401, Message: ep.Name + " has no key or login set"}
	case same && kind == ErrBilling:
		return &APIError{Status: 402, Message: ep.Name + " is out of credit (its keys are set aside for now)"}
	case same && kind == ErrAuth:
		return &APIError{Status: 401, Message: ep.Name + " turned down every key or login (set aside for now)"}
	}
	return &APIError{Status: 429, Message: "all credentials cooling down for " + ep.Name}
}

// UnavailableNotice reports whether background work (a schedule, a board
// card) should tell the user about an outage, named by an
// UnavailableError's Key, and marks it told. It's true the first time, then
// again only after a day while the outage lasts. A successful call to any
// of its providers ends the outage, so the next one is told at once. The
// record is kept in memory: a restart tells once more.
func (r *Router) UnavailableNotice(key string) bool {
	r.defaults()
	r.mu.Lock()
	defer r.mu.Unlock()
	if at, ok := r.told[key]; ok && time.Since(at) < unavailableAgain {
		return false
	}
	r.told[key] = time.Now()
	return true
}

// recovered forgets the outages endpoint was part of, after a call to it
// worked.
func (r *Router) recovered(endpoint string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key := range r.told {
		for _, part := range strings.Split(strings.TrimPrefix(key, "unavailable:"), ",") {
			if name, _, _ := strings.Cut(part, "="); name == endpoint {
				delete(r.told, key)
				break
			}
		}
	}
}
