package llm

import (
	"context"
	"errors"
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
	if !r.UnavailableNotice(key) {
		t.Fatal("the first notice should be given")
	}
	if r.UnavailableNotice(key) {
		t.Fatal("the same outage was told twice")
	}
	// A notice a day old is given again.
	r.told[key] = time.Now().Add(-unavailableAgain - time.Minute)
	if !r.UnavailableNotice(key) {
		t.Fatal("a day-old notice should be given again")
	}
	// Another provider working doesn't end deepseek's outage.
	r.Endpoints["chatgpt"] = ep("chatgpt", &scripted{}, "k")
	if _, err := r.Send(context.Background(), []Target{{"chatgpt", "gpt"}}, Request{}, nil); err != nil {
		t.Fatal(err)
	}
	if r.UnavailableNotice(key) {
		t.Fatal("chatgpt working ended deepseek's outage")
	}
	// Deepseek working again does: the next outage is told at once.
	if _, err := r.Send(context.Background(), []Target{{"deepseek", "flash"}}, Request{}, nil); err != nil {
		t.Fatal(err)
	}
	if !r.UnavailableNotice(key) {
		t.Fatal("after deepseek recovered, a new outage should be told")
	}
}
