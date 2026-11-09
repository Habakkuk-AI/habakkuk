package config

import (
	"strings"
	"testing"
	"time"
)

func TestMeetingDefaults(t *testing.T) {
	cfg, err := Parse([]byte(Template))
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.MeetingConfig()
	if cfg.Meeting != nil || !*m.Announce || !*m.Headless || m.Name != "Habakkuk (notetaker)" || m.MaxDuration != 3*time.Hour ||
		m.LobbyTimeout != 15*time.Minute || m.AloneTimeout != 10*time.Minute || m.SummaryModel != "" {
		t.Fatalf("defaults = %+v", m)
	}
	cfg, err = Parse([]byte(Template + "meeting:\n  announce: false\n  name: Notes\n  max_duration: 90m\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m := cfg.MeetingConfig(); *m.Announce || m.Name != "Notes" || m.MaxDuration != 90*time.Minute || m.LobbyTimeout != 15*time.Minute {
		t.Fatalf("configured = %+v", m)
	}
}

func TestMeetingValidation(t *testing.T) {
	for in, want := range map[string]string{
		"lobby_timeout: 5m":                "",
		"max_duration: -1m":                "must not be negative",
		"max_duration: 48h":                "at most 24h",
		"summary_model: nobody/model":      `unknown provider "nobody"`,
		"summary_model: no-slash":          "summary_model",
		"name: " + strings.Repeat("x", 61): "at most 60",
		"anounce: false":                   "not found",
	} {
		_, err := Parse([]byte(Template + "meeting:\n  " + in + "\n"))
		switch {
		case want == "" && err != nil:
			t.Errorf("%q: %v", in, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%q: err = %v, want %q", in, err, want)
		}
	}
}
