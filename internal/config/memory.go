package config

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Memory configures what Habakkuk does with agents' long-term memory beyond
// the notes and their full-text index: semantic search and background
// review. A nil section keeps full-text search only and no review.
type Memory struct {
	// Embeddings adds semantic search: notes are embedded when saved and
	// search fuses full-text and vector rankings. Nil is full-text only.
	Embeddings *Embeddings `yaml:"embeddings"`
	// Review turns on background consolidation: after a session goes idle a
	// model reviews what is new in it and saves, updates and merges notes
	// and the user profile; a nightly pass merges near-duplicates and
	// archives stale notes.
	Review bool `yaml:"review"`
	// ReviewModel is "<provider>/<model>"; default the last model in each
	// agent's chain (its cheapest fallback, by convention).
	ReviewModel string `yaml:"review_model"`
	// ReviewIdle is how long a session must be quiet before it is reviewed
	// (default 15m).
	ReviewIdle time.Duration `yaml:"review_idle"`
	// Nightly is the local time of the nightly pass, "HH:MM" (default
	// "03:30"), or "off".
	Nightly string `yaml:"nightly"`
	// StaleDays archives notes neither updated nor read for this many days
	// in the nightly pass (default 365; -1 never). Notes tagged pinned or
	// keep are never archived.
	StaleDays int `yaml:"stale_days"`
	// HistoryDays is how long changes to memory, USER.md, IDENTITY.md and
	// skills are kept (default 90; -1 keeps them all). See memoryhistory.go.
	HistoryDays int `yaml:"history_days"`
}

// Embeddings names the provider and model that embed memory notes. The
// provider must be of type openai (any OpenAI-compatible /embeddings
// endpoint, including local servers such as preset ollama) or gemini.
type Embeddings struct {
	Provider   string `yaml:"provider"`
	Model      string `yaml:"model"`      // e.g. text-embedding-3-small, nomic-embed-text, gemini-embedding-001
	Dimensions int    `yaml:"dimensions"` // ask for shorter vectors where the model supports it; 0 is the model's default
}

func (c *Config) applyMemoryDefaults() {
	m := c.Memory
	if m == nil {
		return
	}
	if m.ReviewIdle == 0 {
		m.ReviewIdle = 15 * time.Minute
	}
	if m.Nightly == "" {
		m.Nightly = "03:30"
	}
	if m.StaleDays == 0 {
		m.StaleDays = 365
	}
}

// NightlyAt is the nightly pass's offset from local midnight; ok is false
// when it is off.
func (m *Memory) NightlyAt() (time.Duration, bool) {
	if m == nil || strings.EqualFold(m.Nightly, "off") {
		return 0, false
	}
	t, err := time.Parse("15:04", m.Nightly)
	if err != nil {
		return 0, false
	}
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute, true
}

func (c *Config) validateMemory() []error {
	m := c.Memory
	if m == nil {
		return nil
	}
	var errs []error
	if e := m.Embeddings; e != nil {
		p, ok := c.Providers[e.Provider]
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("memory.embeddings: provider %q is not configured", e.Provider))
		case p.Auth != "":
			errs = append(errs, fmt.Errorf("memory.embeddings: provider %q signs in with a login; embeddings need an API key provider", e.Provider))
		case p.Type != TypeOpenAI && p.Type != TypeGemini:
			errs = append(errs, fmt.Errorf("memory.embeddings: provider %q must be of type openai or gemini", e.Provider))
		}
		if e.Model == "" {
			errs = append(errs, errors.New("memory.embeddings: model is required"))
		}
		if e.Dimensions < 0 {
			errs = append(errs, errors.New("memory.embeddings: dimensions must not be negative"))
		}
	}
	if m.ReviewModel != "" {
		if ref, err := ParseModelRef(m.ReviewModel); err != nil {
			errs = append(errs, fmt.Errorf("memory.review_model: %w", err))
		} else if _, ok := c.Providers[ref.Provider]; !ok {
			errs = append(errs, fmt.Errorf("memory.review_model: unknown provider %q", ref.Provider))
		}
	}
	if m.ReviewIdle < time.Minute {
		errs = append(errs, errors.New("memory.review_idle must be at least 1m"))
	}
	if _, ok := m.NightlyAt(); !ok && !strings.EqualFold(m.Nightly, "off") {
		errs = append(errs, fmt.Errorf("memory.nightly %q must be HH:MM or off", m.Nightly))
	}
	if m.StaleDays < -1 {
		errs = append(errs, errors.New("memory.stale_days must be a number of days, or -1 for never"))
	}
	errs = append(errs, m.validateHistory()...)
	return errs
}
