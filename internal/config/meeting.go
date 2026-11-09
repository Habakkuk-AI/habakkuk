package config

import (
	"fmt"
	"strings"
	"time"
)

// Meeting configures meeting attendance (agents opt in by listing the
// toolset "meeting"): Habakkuk joins a Google Meet, Zoom or Microsoft Teams
// call as a named guest in a browser of its own, reads the live captions
// into a transcript, and sends notes to the chat that asked when it ends.
// The browser binary and allow_private come from the browser section.
type Meeting struct {
	// Announce posts "<name> has joined" in the meeting's chat after
	// joining, where the platform has one (default true).
	Announce *bool  `yaml:"announce"`
	Name     string `yaml:"name"` // display name (default "Habakkuk (notetaker)")
	// MaxDuration is the longest Habakkuk stays in a call, and the limit
	// when meeting_join gives none (default 3h).
	MaxDuration time.Duration `yaml:"max_duration"`
	// LobbyTimeout gives up when nobody lets Habakkuk in this long (default 15m).
	LobbyTimeout time.Duration `yaml:"lobby_timeout"`
	// AloneTimeout leaves a call Habakkuk has been alone in this long
	// (default 10m).
	AloneTimeout time.Duration `yaml:"alone_timeout"`
	Headless     *bool         `yaml:"headless"` // default true
	// SummaryModel ("<provider>/<model>") writes the notes; default the
	// agent's own chain.
	SummaryModel string `yaml:"summary_model"`
}

// MeetingConfig is the meeting section with defaults applied.
func (c *Config) MeetingConfig() Meeting {
	var m Meeting
	if c.Meeting != nil {
		m = *c.Meeting
	}
	on := true
	if m.Announce == nil {
		m.Announce = &on
	}
	if m.Headless == nil {
		m.Headless = &on
	}
	if strings.TrimSpace(m.Name) == "" {
		m.Name = "Habakkuk (notetaker)"
	}
	if m.MaxDuration == 0 {
		m.MaxDuration = 3 * time.Hour
	}
	if m.LobbyTimeout == 0 {
		m.LobbyTimeout = 15 * time.Minute
	}
	if m.AloneTimeout == 0 {
		m.AloneTimeout = 10 * time.Minute
	}
	return m
}

func (c *Config) validateMeeting() []error {
	m := c.Meeting
	if m == nil {
		return nil
	}
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf("meeting: "+format, args...)) }
	if m.MaxDuration < 0 || m.LobbyTimeout < 0 || m.AloneTimeout < 0 {
		bad("max_duration, lobby_timeout and alone_timeout must not be negative")
	}
	if m.MaxDuration > 24*time.Hour {
		bad("max_duration must be at most 24h")
	}
	if len(m.Name) > 60 {
		bad("name must be at most 60 characters")
	}
	if m.SummaryModel != "" {
		ref, err := ParseModelRef(m.SummaryModel)
		if err != nil {
			bad("summary_model: %v", err)
		} else if _, ok := c.Providers[ref.Provider]; !ok {
			bad("summary_model: unknown provider %q", ref.Provider)
		}
	}
	return errs
}
