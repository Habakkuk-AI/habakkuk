package keychain

import (
	"errors"
	"testing"

	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/connections"
	"github.com/zalando/go-keyring"
)

func TestStore(t *testing.T) {
	keyring.MockInit() // an in-memory keyring; nothing touches the real one
	var s Store
	if _, err := s.Get("c1"); !errors.Is(err, connections.ErrNoSecret) {
		t.Fatalf("Get on empty: %v, want ErrNoSecret", err)
	}
	if err := s.Set("c1", "tok"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get("c1"); err != nil || got != "tok" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if err := s.Delete("c1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("c1"); err != nil {
		t.Fatalf("second Delete: %v, want nil", err)
	}
}
