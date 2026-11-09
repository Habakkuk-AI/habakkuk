// Package keychain stores connection tokens in the operating system's
// credential store: the login Keychain on macOS (through /usr/bin/security),
// the Secret Service on Linux (GNOME Keyring, KWallet) and the Credential
// Manager on Windows. It is a thin wrapper over a pure-Go library.
package keychain

import (
	"errors"

	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/connections"
	"github.com/zalando/go-keyring"
)

// Service is the name entries are filed under.
const Service = "Habakkuk Desktop"

// Store implements connections.Secrets.
type Store struct{}

var _ connections.Secrets = Store{}

func (Store) Get(id string) (string, error) {
	s, err := keyring.Get(Service, id)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", connections.ErrNoSecret
	}
	return s, err
}

func (Store) Set(id, secret string) error { return keyring.Set(Service, id, secret) }

func (Store) Delete(id string) error {
	err := keyring.Delete(Service, id)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}
