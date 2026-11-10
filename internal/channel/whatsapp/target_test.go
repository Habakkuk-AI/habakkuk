package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go.mau.fi/whatsmeow"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

func TestNormalizeTarget(t *testing.T) {
	a := &Adapter{}
	for in, want := range map[string]string{
		"12025550142":                   "12025550142@s.whatsapp.net",
		"+12025550142":                  "12025550142@s.whatsapp.net",
		"+1 202-555-0142":               "12025550142@s.whatsapp.net",
		"12025550142@s.whatsapp.net":    "12025550142@s.whatsapp.net",
		"12025550142@c.us":              "12025550142@s.whatsapp.net",
		"98765432101234@lid":            "98765432101234@lid",
		"120363012345678901@g.us":       "120363012345678901@g.us",
		"12025550142:12@s.whatsapp.net": "12025550142@s.whatsapp.net",
	} {
		got, err := a.NormalizeTarget(in)
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "+", "mum", "12025550142@example.com", "@s.whatsapp.net", "1@2@3"} {
		if _, err := a.NormalizeTarget(in); !errors.As(err, new(channel.Undeliverable)) {
			t.Errorf("%q: err = %v, want undeliverable", in, err)
		}
	}
}

func TestBadTargetFailsFast(t *testing.T) {
	a := &Adapter{} // not connected
	ctx := context.Background()
	err := a.Send(ctx, channel.Address{Conversation: "12025550142@example.com"}, "hi", "")
	if !errors.As(err, new(channel.Undeliverable)) {
		t.Fatalf("unknown server: %v, want undeliverable", err)
	}
	// A good target while disconnected is a transient failure: retry it.
	err = a.Send(ctx, channel.Address{Conversation: "+12025550142"}, "hi", "")
	if err == nil || errors.As(err, new(channel.Undeliverable)) {
		t.Fatalf("disconnected: %v, want a retryable error", err)
	}
	if err := sendErr(fmt.Errorf("send: %w %s", whatsmeow.ErrUnknownServer, "x")); !errors.As(err, new(channel.Undeliverable)) {
		t.Fatalf("whatsmeow unknown server: %v", err)
	}
	if err := sendErr(errors.New("websocket closed")); errors.As(err, new(channel.Undeliverable)) {
		t.Fatalf("transient error marked undeliverable: %v", err)
	}
}
