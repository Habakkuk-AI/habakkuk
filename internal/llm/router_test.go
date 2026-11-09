package llm

import (
	"context"
	"errors"
	"testing"
	"time"
)

// scripted returns the queued results in order, recording which key was used.
type scripted struct {
	results []error
	keys    []string
}

func (s *scripted) Stream(ctx context.Context, key string, req Request, on func(Event)) (*Response, error) {
	s.keys = append(s.keys, key)
	if len(s.results) == 0 {
		return &Response{Message: Message{Role: Assistant, Blocks: []Block{{Type: Text, Text: "ok from " + req.Model}}}}, nil
	}
	err := s.results[0]
	s.results = s.results[1:]
	if err == nil {
		return &Response{Message: Message{Role: Assistant, Blocks: []Block{{Type: Text, Text: "ok from " + req.Model}}}}, nil
	}
	return nil, err
}

func newRouter(eps ...*Endpoint) *Router {
	r := &Router{Endpoints: map[string]*Endpoint{}}
	for _, e := range eps {
		r.Endpoints[e.Name] = e
	}
	r.defaults()
	r.sleep = func(context.Context, time.Duration) error { return nil }
	return r
}

func ep(name string, p Provider, keys ...string) *Endpoint {
	e := &Endpoint{Name: name, Provider: p}
	for _, k := range keys {
		e.Credentials = append(e.Credentials, Credential{Name: k, Key: k})
	}
	return e
}

func TestRetryTransientThenSucceed(t *testing.T) {
	p := &scripted{results: []error{&APIError{Status: 500, Message: "boom"}, nil}}
	r := newRouter(ep("a", p, "k1"))
	resp, err := r.Send(context.Background(), []Target{{"a", "m1"}}, Request{}, nil)
	if err != nil || resp.Model != "a/m1" || len(p.keys) != 2 {
		t.Fatalf("resp %+v err %v calls %d", resp, err, len(p.keys))
	}
}

func TestRotateKeyOnRateLimit(t *testing.T) {
	p := &scripted{results: []error{&APIError{Status: 429, Message: "slow down"}, nil}}
	r := newRouter(ep("a", p, "k1", "k2"))
	if _, err := r.Send(context.Background(), []Target{{"a", "m1"}}, Request{}, nil); err != nil {
		t.Fatal(err)
	}
	if p.keys[0] != "k1" || p.keys[1] != "k2" {
		t.Fatalf("keys used %v", p.keys)
	}
	// k1 is cooling down, so the next request starts on k2.
	r.Send(context.Background(), []Target{{"a", "m1"}}, Request{}, nil)
	if p.keys[2] != "k2" {
		t.Fatalf("cooled key reused: %v", p.keys)
	}
}

func TestFallbackToNextModelOnOverload(t *testing.T) {
	primary := &scripted{results: []error{&APIError{Status: 529, Message: "overloaded"}, &APIError{Status: 529, Message: "overloaded"}, &APIError{Status: 529, Message: "overloaded"}}}
	backup := &scripted{}
	r := newRouter(ep("a", primary, "k"), ep("b", backup, "k"))
	var retries int
	resp, err := r.Send(context.Background(), []Target{{"a", "big"}, {"b", "small"}}, Request{}, func(e Event) {
		if e.Type == EventRetry {
			retries++
		}
	})
	if err != nil || resp.Model != "b/small" {
		t.Fatalf("resp %+v err %v", resp, err)
	}
	if retries == 0 {
		t.Fatal("client was not told about the fallback")
	}
}

func TestBadRequestDoesNotFallBack(t *testing.T) {
	primary := &scripted{results: []error{&APIError{Status: 400, Message: "invalid"}}}
	backup := &scripted{}
	r := newRouter(ep("a", primary, "k"), ep("b", backup, "k"))
	if _, err := r.Send(context.Background(), []Target{{"a", "m"}, {"b", "m"}}, Request{}, nil); err == nil {
		t.Fatal("expected error")
	}
	if len(backup.keys) != 0 {
		t.Fatal("bad request should not be sent to the fallback model")
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		err  error
		kind ErrorKind
	}{
		{&APIError{Status: 429}, ErrRateLimit},
		{&APIError{Status: 529}, ErrOverloaded},
		{&APIError{Status: 401}, ErrAuth},
		{&APIError{Status: 400, Message: "prompt is too long: 1200000 tokens"}, ErrContextOverflow},
		{&APIError{Status: 400, Message: "Your credit balance is too low"}, ErrBilling},
		{&APIError{Status: 503}, ErrServer},
		{&APIError{Status: 400, Message: "bad field"}, ErrBadRequest},
		{errors.New(`received error while streaming: {"type":"overloaded_error"}`), ErrOverloaded},
		{context.Canceled, ErrCanceled},
	}
	for _, c := range cases {
		if got := Classify(c.err).Kind; got != c.kind {
			t.Errorf("Classify(%v) = %s, want %s", c.err, got, c.kind)
		}
	}
}
