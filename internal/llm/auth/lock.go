package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/platform"
)

// lockFile takes an exclusive lock on path (flock on Unix, LockFileEx on
// Windows), waiting up to 30 seconds (or until ctx ends). The lock is
// released when the process exits, so a crash never leaves it held.
func lockFile(ctx context.Context, path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		ok, err := platform.TryLock(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		if ok {
			return func() {
				_ = platform.Unlock(f)
				f.Close()
			}, nil
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, errors.New("timed out waiting for the auth.json lock")
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
