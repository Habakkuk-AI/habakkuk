package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
)

func outOfCredit() error { return &APIError{Status: 402, Message: "Insufficient Balance"} }

func lowBalance() error {
	return &APIError{Status: 400, Message: "Your credit balance is too low to access the Anthropic API."}
}

func TestEveryModelOutOfCreditSaysSoPlainly(t *testing.T) {
	r := newRouter(
		ep("deepseek", &scripted{results: []error{outOfCredit(), outOfCredit()}}, "k"),
		ep("anthropic", &scripted{results: []error{lowBalance()}}, "k"),
	)
	chain := []Target{{"deepseek", "flash"}, {"deepseek", "pro"}, {"anthropic", "haiku"}}
	_, err := r.Send(context.Background(), chain, Request{}, nil)
	ue := new(UnavailableError)
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v, want an UnavailableError", err)
	}
	want := "no model could answer: deepseek is out of credit and anthropic is out of credit."
	if !strings.HasPrefix(ue.Message, want) {
		t.Errorf("message = %q, want it to start %q", ue.Message, want)
	}
	if ue.Key != "unavailable:anthropic=billing,deepseek=billing" {
		t.Errorf("key = %q", ue.Key)
	}
	if !jobs.IsPermanent(err) {
		t.Error("an outage should not be retried by the job queue")
	}
	if Classify(err).Kind != ErrBilling {
		t.Errorf("classify = %v, want billing", Classify(err).Kind)
	}

	// The next call finds both providers' keys set aside. It must still
	// say "out of credit", not "cooling down", and with the same key.
	_, err = r.Send(context.Background(), chain, Request{}, nil)
	ue2 := new(UnavailableError)
	if !errors.As(err, &ue2) || ue2.Key != ue.Key {
		t.Fatalf("second call: err = %v", err)
	}
	if strings.Contains(err.Error(), "cooling down") {
		t.Errorf("second call says %q", err)
	}
}

func TestOutageWithAWorkingFallbackIsNotAnOutage(t *testing.T) {
	r := newRouter(
		ep("deepseek", &scripted{results: []error{outOfCredit()}}, "k"),
		ep("chatgpt", &scripted{}, "k"),
	)
	resp, err := r.Send(context.Background(), []Target{{"deepseek", "flash"}, {"chatgpt", "gpt"}}, Request{}, nil)
	if err != nil || resp.Model != "chatgpt/gpt" {
		t.Fatalf("resp %+v err %v", resp, err)
	}
}

func TestMixedFailureIsNotAnOutage(t *testing.T) {
	// One model out of credit, the other down for its own reasons: that
	// isn't something a top-up fixes, so the plain errors come back.
	r := newRouter(
		ep("deepseek", &scripted{results: []error{outOfCredit()}}, "k"),
		ep("anthropic", &scripted{results: []error{
			&APIError{Status: 500, Message: "boom"}, &APIError{Status: 500, Message: "boom"}, &APIError{Status: 500, Message: "boom"}}}, "k"),
	)
	_, err := r.Send(context.Background(), []Target{{"deepseek", "flash"}, {"anthropic", "haiku"}}, Request{}, nil)
	if err == nil || errors.As(err, new(*UnavailableError)) {
		t.Fatalf("err = %v, want the plain errors", err)
	}
}

func TestRejectedKeyIsNamed(t *testing.T) {
	r := newRouter(ep("openai", &scripted{results: []error{&APIError{Status: 401, Message: "bad key"}}}, "k"))
	_, err := r.Send(context.Background(), []Target{{"openai", "m"}}, Request{}, nil)
	ue := new(UnavailableError)
	if !errors.As(err, &ue) || !strings.Contains(ue.Message, "openai turned down its key or login") || ue.Key != "unavailable:openai=auth" {
		t.Fatalf("err = %v", err)
	}
}

func TestNoKeySetIsNamed(t *testing.T) {
	r := newRouter(&Endpoint{Name: "groq", Provider: &scripted{}, Credentials: []Credential{{Name: "GROQ_API_KEY"}}})
	_, err := r.Send(context.Background(), []Target{{"groq", "m"}}, Request{}, nil)
	if err == nil || !strings.Contains(err.Error(), "turned down its key or login") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(errors.Unwrap(err).Error(), "groq has no key or login set") {
		t.Errorf("underlying error = %v", errors.Unwrap(err))
	}
}

func TestUnavailableNoticeOncePerOutage(t *testing.T) {
	r := newRouter(ep("deepseek", &scripted{}, "k"))
	key := "unavailable:deepseek=billing"
	if !r.UnavailableNotice(key, "chat") {
		t.Fatal("the first notice should be given")
	}
	if r.UnavailableNotice(key, "chat") {
		t.Fatal("the same outage was told twice")
	}
	// A notice a day old is given again.
	r.told[key+"\x00chat"] = time.Now().Add(-unavailableAgain - time.Minute)
	if !r.UnavailableNotice(key, "chat") {
		t.Fatal("a day-old notice should be given again")
	}
	// Another provider working doesn't end deepseek's outage.
	r.Endpoints["chatgpt"] = ep("chatgpt", &scripted{}, "k")
	if _, err := r.Send(context.Background(), []Target{{"chatgpt", "gpt"}}, Request{}, nil); err != nil {
		t.Fatal(err)
	}
	if r.UnavailableNotice(key, "chat") {
		t.Fatal("chatgpt working ended deepseek's outage")
	}
	// Deepseek working again does: the next outage is told at once.
	if _, err := r.Send(context.Background(), []Target{{"deepseek", "flash"}}, Request{}, nil); err != nil {
		t.Fatal(err)
	}
	if !r.UnavailableNotice(key, "chat") {
		t.Fatal("after deepseek recovered, a new outage should be told")
	}
}

func TestKeysEmptyAndRevokedStayAnOutage(t *testing.T) {
	// k1 is out of credit, k2 was revoked: both calls name the provider
	// the same way, never "cooling down".
	r := newRouter(ep("deepseek", &scripted{results: []error{outOfCredit(), &APIError{Status: 401, Message: "revoked"}}}, "k1", "k2"))
	var keys []string
	for range 2 {
		_, err := r.Send(context.Background(), []Target{{"deepseek", "flash"}}, Request{}, nil)
		ue := new(UnavailableError)
		if !errors.As(err, &ue) || !strings.Contains(ue.Message, "deepseek is out of credit") {
			t.Fatalf("err = %v", err)
		}
		keys = append(keys, ue.Key)
	}
	if keys[0] != keys[1] {
		t.Fatalf("keys differ: %q", keys)
	}
}

func TestRateLimitedKeyIsNoOutage(t *testing.T) {
	// k1 is only rate limited and k2 is out of credit: k1 is back in a
	// minute, so this is retried, whichever key is tried first.
	for _, results := range [][]error{
		{&APIError{Status: 429, Message: "slow down"}, outOfCredit()},
		{outOfCredit(), &APIError{Status: 429, Message: "slow down"}},
	} {
		r := newRouter(ep("deepseek", &scripted{results: results}, "k1", "k2"))
		_, err := r.Send(context.Background(), []Target{{"deepseek", "flash"}}, Request{}, nil)
		if err == nil || errors.As(err, new(*UnavailableError)) || jobs.IsPermanent(err) {
			t.Fatalf("results %v: err = %v, want a retryable error", results, err)
		}
	}
}

func TestLoginRefreshFailureIsNoOutage(t *testing.T) {
	r := newRouter(&Endpoint{Name: "chatgpt", Provider: &scripted{}, Credentials: []Credential{{Name: "login",
		Token: func(context.Context) (string, error) {
			return "", errors.New("chatgpt: refresh: 503 Service Unavailable")
		}}}})
	_, err := r.Send(context.Background(), []Target{{"chatgpt", "gpt"}}, Request{}, nil)
	if err == nil || errors.As(err, new(*UnavailableError)) || jobs.IsPermanent(err) {
		t.Fatalf("err = %v, want a retryable error", err)
	}
}

func TestSameProviderTwiceInAChain(t *testing.T) {
	// Out of credit on one model, a rejected key on the other: billing
	// names it, whichever comes first.
	for _, results := range [][]error{
		{outOfCredit(), &APIError{Status: 401, Message: "no access to this model"}},
		{&APIError{Status: 401, Message: "no access to this model"}, outOfCredit()},
	} {
		r := newRouter(ep("deepseek", &scripted{results: results}, "k"))
		// Each model needs its own key here, or the first refusal sets
		// the only key aside; two endpoints of one name can't exist, so
		// use two keys.
		r.Endpoints["deepseek"].Credentials = []Credential{{Name: "k1", Key: "k1"}, {Name: "k2", Key: "k2"}}
		_, err := r.Send(context.Background(), []Target{{"deepseek", "flash"}, {"deepseek", "pro"}}, Request{}, nil)
		ue := new(UnavailableError)
		if !errors.As(err, &ue) || ue.Key != "unavailable:deepseek=billing" {
			t.Fatalf("results %v: err = %v", results, err)
		}
	}
}

func TestUnavailableNoticePerChat(t *testing.T) {
	r := newRouter()
	key := "unavailable:deepseek=billing"
	if !r.UnavailableNotice(key, "chat 100") || !r.UnavailableNotice(key, "chat 200") {
		t.Fatal("each chat should be told once")
	}
	if r.UnavailableNotice(key, "chat 100") || r.UnavailableNotice(key, "chat 200") {
		t.Fatal("a chat was told twice")
	}
}

type goneLogin struct{}

func (goneLogin) Error() string   { return "login expired or revoked" }
func (goneLogin) LoginGone() bool { return true }

func TestExpiredLoginIsAnOutage(t *testing.T) {
	r := newRouter(&Endpoint{Name: "chatgpt", Provider: &scripted{}, Credentials: []Credential{{Name: "login",
		Token: func(context.Context) (string, error) {
			return "", fmt.Errorf("chatgpt: %w: 401 invalid_grant", goneLogin{})
		}}}})
	_, err := r.Send(context.Background(), []Target{{"chatgpt", "gpt"}}, Request{}, nil)
	ue := new(UnavailableError)
	if !errors.As(err, &ue) || ue.Key != "unavailable:chatgpt=auth" || !jobs.IsPermanent(err) {
		t.Fatalf("err = %v, want a chatgpt=auth outage", err)
	}
}
