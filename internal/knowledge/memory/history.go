package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory/history"
)

// ErrNoHistory is returned by the history operations of a store without
// one.
var ErrNoHistory = errors.New("memory history is not available here")

// UseHistory records every change to notes and USER.md in h, and notes
// edited by hand when the index notices them. Call it before the store is
// used.
func (s *Store) UseHistory(h *history.Log) { s.hist = h }

// History is the change log in use, or nil.
func (s *Store) History() *history.Log { return s.hist }

// writeTracked writes a note or the profile atomically, recording the
// change when there is a history.
func (s *Store) writeTracked(ctx context.Context, t history.Target, data []byte) error {
	write := func() error {
		if err := os.MkdirAll(filepath.Dir(t.Path), 0o700); err != nil {
			return err
		}
		return store.WriteFileAtomic(t.Path, data, 0o600)
	}
	if s.hist == nil {
		return write()
	}
	_, err := s.hist.Write(ctx, t, data, write)
	return err
}

// removeTracked removes a note with remove, recording the change.
func (s *Store) removeTracked(ctx context.Context, t history.Target, remove func() error) error {
	if s.hist == nil {
		return remove()
	}
	_, err := s.hist.Remove(ctx, t, remove)
	return err
}

// Restore puts back one side of a recorded change (history.Before undoes
// it, history.After redoes it) and brings the index up to date. With agent
// set, only that agent's changes may be restored. The restore is recorded
// as a change by the context's actor; it is nil when the file already held
// that version.
func (s *Store) Restore(ctx context.Context, agent, id, version string) (history.Change, *history.Change, error) {
	if s.hist == nil {
		return history.Change{}, nil, ErrNoHistory
	}
	c, err := s.hist.Get(ctx, id)
	if err != nil {
		return c, nil, err
	}
	if agent != "" && c.Agent != agent {
		return c, nil, fmt.Errorf("no change %s for agent %s", id, agent)
	}
	s.mu.Lock()
	c, r, err := s.hist.Restore(ctx, c.ID, version)
	if err == nil && c.Kind == history.KindNote {
		err = s.reindex(ctx, c.Agent, s.Dir(c.Agent))
	}
	s.mu.Unlock()
	if err == nil && c.Kind == history.KindNote {
		s.EmbedPending(ctx, c.Agent)
	}
	return c, r, err
}

// Pinned reports whether background jobs must leave a note alone.
func (s *Store) Pinned(ctx context.Context, agent, slug string) bool {
	return s.pinned(ctx, agent, history.KindNote, slug)
}

// ProfilePinned reports whether background jobs must leave USER.md alone.
func (s *Store) ProfilePinned(ctx context.Context, agent string) bool {
	return s.pinned(ctx, agent, history.KindProfile, history.ProfileFile)
}

func (s *Store) pinned(ctx context.Context, agent, kind, name string) bool {
	if s.hist == nil {
		return false
	}
	ok, err := s.hist.Pinned(ctx, agent, kind, name)
	if err != nil {
		s.log.Warn("memory history: pin check failed", "agent", agent, "name", name, "err", err)
		return true // when in doubt, leave it alone
	}
	return ok
}

// ScanHistory records hand edits to an agent's notes, USER.md, IDENTITY.md
// and skills since they were last seen.
func (s *Store) ScanHistory(ctx context.Context, agent string) error {
	if s.hist == nil {
		return nil
	}
	return s.hist.Scan(ctx, agent)
}
