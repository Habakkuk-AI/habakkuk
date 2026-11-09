package main

import (
	"strings"
	"testing"
)

func TestSpokenReply(t *testing.T) {
	if got := spokenReply("**Done.** See `notes.md`.", 0); strings.Contains(got, "*") {
		t.Fatalf("markdown kept: %q", got)
	}
	long := strings.Repeat("This is a sentence. ", 20)
	got := spokenReply(long, 100)
	if len(got) > 100 || !strings.HasSuffix(got, "sentence. The rest is on screen.") {
		t.Fatalf("cut: %q (%d)", got, len(got))
	}
}
