package channel

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/config"
)

// phoneRecorder is a recorder whose conversations are phone numbers
// written as "<digits>@phone", as WhatsApp writes them as JIDs.
type phoneRecorder struct{ *recorder }

func (phoneRecorder) NormalizeTarget(conv string) (string, error) {
	if strings.Contains(conv, "@") {
		return conv, nil
	}
	digits := strings.TrimPrefix(conv, "+")
	if digits == "" || strings.Trim(digits, "0123456789") != "" {
		return "", Undeliverable{Err: errors.New("not a phone number")}
	}
	return digits + "@phone", nil
}

func TestSendNormalizesTheTarget(t *testing.T) {
	r := newRig(t, &config.Channel{})
	r.g.Adapters["tg"] = phoneRecorder{r.a}
	ctx := context.Background()
	if err := r.g.SendText(ctx, "send_1", Address{Account: "tg", Conversation: "+12025550142"}, "hello"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the message", hasText(r.a, "hello"))
	r.a.mu.Lock()
	got := r.a.sent[0].To.Conversation
	r.a.mu.Unlock()
	if got != "12025550142@phone" {
		t.Fatalf("sent to %q, want 12025550142@phone", got)
	}
	err := r.g.SendText(ctx, "send_2", Address{Account: "tg", Conversation: "not-a-number"}, "hello")
	if !errors.As(err, new(Undeliverable)) {
		t.Fatalf("a target that can never work was queued: %v", err)
	}
	home, err := r.g.home("tg:+12025550142")
	if err != nil || home.Conversation != "12025550142@phone" {
		t.Fatalf("home = %+v, %v", home, err)
	}
}
