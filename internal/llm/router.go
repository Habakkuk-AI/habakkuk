package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"
)

// Credential is one API key for a provider.
type Credential struct {
	Name string // env var name, for logs; never the key itself
	Key  string
	// Token, when set, supplies the key at call time instead of Key: a
	// login whose access token is refreshed before it expires, or "" for a
	// local server that needs no key.
	Token func(ctx context.Context) (string, error)
}

// Endpoint is a configured provider: an adapter plus its credentials.
type Endpoint struct {
	Name        string
	Provider    Provider
	Credentials []Credential
}

// Target is one model in an agent's fallback chain.
type Target struct {
	Endpoint string
	Model    string
}

func (t Target) String() string { return t.Endpoint + "/" + t.Model }

// Router sends a request down a chain of models. For each model it retries
// transient errors with jittered backoff, rotates to the next credential on
// rate-limit and auth errors (cooling the bad key down), and falls back to the
// next model on errors another model can fix.
type Router struct {
	Endpoints  map[string]*Endpoint
	MaxRetries int           // per model and credential; default 2
	BaseDelay  time.Duration // default 1s
	Log        *slog.Logger
	// OnUsage, when set, is told about every successful call, with the
	// caller (agent and session) taken from the context.
	OnUsage func(ctx context.Context, t Target, u Usage)
	// Gate, when set, is asked before every call to a model; an error
	// refuses the call. A *BudgetError with Unpriced moves on to the next
	// model in the chain; any other error ends Send with it.
	Gate func(ctx context.Context, t Target) error

	mu       sync.Mutex
	cooldown map[string]time.Time // endpoint/credential -> usable after
	coolKind map[string]ErrorKind // endpoint/credential -> why it cooled down
	told     map[string]time.Time // UnavailableError.Key -> when the user was told
	sleep    func(context.Context, time.Duration) error
}

func (r *Router) defaults() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cooldown == nil {
		r.cooldown = map[string]time.Time{}
	}
	if r.coolKind == nil {
		r.coolKind = map[string]ErrorKind{}
	}
	if r.told == nil {
		r.told = map[string]time.Time{}
	}
	if r.sleep == nil {
		r.sleep = sleepCtx
	}
	if r.MaxRetries == 0 {
		r.MaxRetries = 2
	}
	if r.BaseDelay == 0 {
		r.BaseDelay = time.Second
	}
	if r.Log == nil {
		r.Log = slog.Default()
	}
}

// Send tries each target in order. onEvent receives stream events; an
// EventRetry means output so far should be discarded.
func (r *Router) Send(ctx context.Context, chain []Target, req Request, onEvent func(Event)) (*Response, error) {
	r.defaults()
	if onEvent == nil {
		onEvent = func(Event) {}
	}
	var errs []error
	// refused records each provider that turned the call down for credit
	// or a rejected key; other is set by any other failure. When every
	// model was refused that way, the turn ends with an UnavailableError.
	var refused refusals
	for i, t := range chain {
		ep, ok := r.Endpoints[t.Endpoint]
		if !ok {
			errs = append(errs, fmt.Errorf("%s: unknown endpoint", t))
			refused.other = true
			continue
		}
		if r.Gate != nil {
			if err := r.Gate(ctx, t); err != nil {
				var be *BudgetError
				if !errors.As(err, &be) || !be.Unpriced {
					return nil, err
				}
				errs = append(errs, err)
				refused.other = true
				continue
			}
		}
		resp, err := r.sendTarget(ctx, ep, t, req, onEvent)
		if err == nil {
			resp.Model = t.String()
			if r.OnUsage != nil {
				r.OnUsage(ctx, t, resp.Usage)
			}
			r.recovered(ep.Name)
			return resp, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", t, err))
		act := Classify(err)
		if act.Kind == ErrCanceled || ctx.Err() != nil {
			return nil, errors.Join(errs...)
		}
		if errors.As(err, new(refreshError)) {
			refused.other = true // a login refresh can fail for a passing reason
		} else {
			refused.add(ep.Name, act.Kind)
		}
		if !act.Fallback {
			return nil, refused.err(errs)
		}
		if i < len(chain)-1 {
			r.Log.Warn("falling back", "from", t.String(), "to", chain[i+1].String(), "reason", act.Kind)
			onEvent(Event{Type: EventRetry, Text: fmt.Sprintf("%s failed (%s); trying %s", t, act.Kind, chain[i+1])})
		}
	}
	return nil, refused.err(errs)
}

func (r *Router) sendTarget(ctx context.Context, ep *Endpoint, t Target, req Request, onEvent func(Event)) (*Response, error) {
	creds := r.usableCredentials(ep)
	if len(creds) == 0 {
		return nil, r.noCredentials(ep)
	}
	req.Model = t.Model
	var lastErr error
	for _, c := range creds {
		key := c.Key
		if c.Token != nil {
			var err error
			if key, err = c.Token(ctx); err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				// A login that cannot produce a token is an auth failure
				// (fall back), unless the refresh only hit the network.
				// It's wrapped as a refresh error so a refresh that failed
				// for a passing reason (the login server down) is never
				// taken for an outage.
				switch {
				case LoginGone(err):
					err = &APIError{Status: 401, Message: c.Name + ": " + err.Error(), Err: err}
				case Classify(err).Kind != ErrNetwork:
					err = &APIError{Status: 401, Message: c.Name + ": " + err.Error(), Err: refreshError{err}}
				}
				lastErr = err
				continue
			}
		}
		for attempt := 0; attempt <= r.MaxRetries; attempt++ {
			emitted := false
			resp, err := ep.Provider.Stream(ctx, key, req, func(e Event) {
				emitted = true
				onEvent(e)
			})
			if err == nil {
				return resp, nil
			}
			lastErr = err
			act := Classify(err)
			if act.Kind == ErrCanceled || ctx.Err() != nil {
				return nil, err
			}
			if act.Rotate {
				r.coolDown(ep.Name, c.Name, act.Kind)
				break // next credential
			}
			if !act.Retry || attempt == r.MaxRetries {
				return nil, err
			}
			if emitted {
				onEvent(Event{Type: EventRetry, Text: "connection interrupted; retrying"})
			}
			delay := r.BaseDelay * time.Duration(1<<attempt)
			delay += time.Duration(rand.Int64N(int64(delay)/2 + 1))
			r.Log.Warn("retrying model call", "target", t.String(), "attempt", attempt+1, "reason", act.Kind, "delay", delay)
			if err := r.sleep(ctx, delay); err != nil {
				return nil, err
			}
		}
	}
	// Every key tried is now set aside: say why for the provider as a
	// whole, so the answer is the same whichever key failed last and on
	// later calls (a key set aside for a rate limit makes it no outage).
	if lastErr != nil && len(r.usableCredentials(ep)) == 0 {
		return nil, r.noCredentials(ep)
	}
	return nil, lastErr
}

func (r *Router) usableCredentials(ep *Endpoint) []Credential {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	var out []Credential
	for _, c := range ep.Credentials {
		if c.Key == "" && c.Token == nil {
			continue
		}
		if until, ok := r.cooldown[ep.Name+"/"+c.Name]; ok && now.Before(until) {
			continue
		}
		out = append(out, c)
	}
	return out
}

func (r *Router) coolDown(endpoint, cred string, kind ErrorKind) {
	d := time.Minute
	if kind == ErrAuth || kind == ErrBilling {
		d = 30 * time.Minute
	}
	r.mu.Lock()
	r.cooldown[endpoint+"/"+cred] = time.Now().Add(d)
	r.coolKind[endpoint+"/"+cred] = kind
	r.mu.Unlock()
	r.Log.Warn("credential cooling down", "endpoint", endpoint, "credential", cred, "reason", kind, "for", d)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
