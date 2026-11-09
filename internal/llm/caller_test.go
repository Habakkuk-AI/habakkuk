package llm

import (
	"context"
	"errors"
	"net"
	"testing"
)

func TestTokenCredentialsAndUsageHook(t *testing.T) {
	p := &scripted{}
	calls := 0
	e := &Endpoint{Name: "a", Provider: p, Credentials: []Credential{
		{Name: "login:x", Token: func(context.Context) (string, error) { calls++; return "fresh", nil }},
	}}
	r := newRouter(e)
	var got []string
	r.OnUsage = func(ctx context.Context, t Target, u Usage) {
		c := CallerFrom(ctx)
		got = append(got, t.String()+" "+c.Agent+" "+c.Session)
	}
	ctx := WithCaller(context.Background(), Caller{Agent: "habakkuk", Session: "ses_1"})
	if _, err := r.Send(ctx, []Target{{"a", "m"}}, Request{}, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || p.keys[0] != "fresh" {
		t.Fatalf("token calls %d keys %v", calls, p.keys)
	}
	if len(got) != 1 || got[0] != "a/m habakkuk ses_1" {
		t.Fatalf("usage hook %v", got)
	}
}

func TestFailedLoginFallsBack(t *testing.T) {
	login := &Endpoint{Name: "sub", Provider: &scripted{}, Credentials: []Credential{
		{Name: "login:x", Token: func(context.Context) (string, error) { return "", errors.New("not logged in") }},
	}}
	backup := &scripted{}
	r := newRouter(login, ep("api", backup, "k"))
	resp, err := r.Send(context.Background(), []Target{{"sub", "m"}, {"api", "m2"}}, Request{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Model != "api/m2" {
		t.Fatalf("answered by %s", resp.Model)
	}
}

func TestRefreshNetworkErrorIsRetryable(t *testing.T) {
	var netErr net.Error = &net.DNSError{Err: "no such host", IsTemporary: true}
	login := &Endpoint{Name: "sub", Provider: &scripted{}, Credentials: []Credential{
		{Name: "login:x", Token: func(context.Context) (string, error) { return "", netErr }},
	}}
	r := newRouter(login)
	_, err := r.Send(context.Background(), []Target{{"sub", "m"}}, Request{}, nil)
	if Classify(err).Kind != ErrNetwork {
		t.Fatalf("err = %v (%s)", err, Classify(err).Kind)
	}
}

func TestKeylessCredentialIsUsable(t *testing.T) {
	p := &scripted{}
	r := newRouter(&Endpoint{Name: "local", Provider: p, Credentials: []Credential{
		{Name: "none", Token: func(context.Context) (string, error) { return "", nil }},
	}})
	if _, err := r.Send(context.Background(), []Target{{"local", "llama"}}, Request{}, nil); err != nil {
		t.Fatal(err)
	}
	if len(p.keys) != 1 || p.keys[0] != "" {
		t.Fatalf("keys %v", p.keys)
	}
}
