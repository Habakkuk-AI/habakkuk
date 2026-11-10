package main

import (
	"strings"
	"testing"
)

func TestKeyFix(t *testing.T) {
	if got := keyFix("/h", []string{"ANTHROPIC_API_KEY"}); !strings.Contains(got, "ANTHROPIC_API_KEY=...") || !strings.Contains(got, "habakkuk setup model") {
		t.Fatalf("keyFix = %q", got)
	}
}
