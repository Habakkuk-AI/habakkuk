package app

import (
	"context"
	"os"
	"path/filepath"

	"github.com/habakkuk-ai/habakkuk/internal/channel/web"
	"github.com/habakkuk-ai/habakkuk/internal/core/session"
	"github.com/habakkuk-ai/habakkuk/internal/core/state"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory"
	"github.com/habakkuk-ai/habakkuk/internal/llm/usage"
	"github.com/habakkuk-ai/habakkuk/internal/protocols/nodes"
)

// Schema is one of Habakkuk's databases and the migrations this binary
// carries for it.
type Schema struct {
	File       string // relative to the home
	Migrations []store.Migration
}

// Latest is the newest schema version this binary knows for the file.
func (s Schema) Latest() int { return store.LatestVersion(s.Migrations) }

// Schemas lists every database whose schema Habakkuk owns. WhatsApp
// device stores (channels/*.db) are left out: their library migrates them.
func Schemas() []Schema {
	return []Schema{
		{"control.db", controlMigrations},
		{"transcripts.db", session.Migrations},
		{"memory.db", memory.Migrations},
		{"state.db", state.Migrations},
		{"usage.db", usage.Migrations},
		{"web.db", web.Migrations()},
		{"nodes.db", nodes.Migrations()},
	}
}

// SchemaStatus is a database's version on disk next to the one this binary
// knows.
type SchemaStatus struct {
	File        string
	OnDisk      int // 0 when the file does not exist yet
	Known       int
	Exists      bool
	Err         error
	NeedsNewer  bool // OnDisk > Known: written by a newer Habakkuk
	WillMigrate bool // OnDisk < Known on an existing file
}

// CheckSchemas reads each database's version under home without opening it
// for writing or migrating it.
func CheckSchemas(ctx context.Context, home string) []SchemaStatus {
	var out []SchemaStatus
	for _, s := range Schemas() {
		st := SchemaStatus{File: s.File, Known: s.Latest()}
		path := filepath.Join(home, s.File)
		st.OnDisk, st.Err = store.SchemaVersion(ctx, path)
		_, statErr := os.Stat(path)
		st.Exists = statErr == nil
		st.NeedsNewer = st.Err == nil && st.OnDisk > st.Known
		st.WillMigrate = st.Err == nil && st.Exists && st.OnDisk < st.Known
		out = append(out, st)
	}
	return out
}
