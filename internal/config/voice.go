package config

import (
	"fmt"
	"strings"
	"time"
)

// Realtime configures the dashboard's Live mode: the browser talks to a
// realtime speech model over WebRTC with a short-lived client secret Habakkuk
// mints, and the model hands real work to Habakkuk through one function tool.
type Realtime struct {
	Provider string `yaml:"provider"` // openai
	Model    string `yaml:"model"`    // default gpt-realtime-2.1
	Voice    string `yaml:"voice"`    // default marin
	KeyEnv   string `yaml:"key_env"`  // variable holding the API key; default OPENAI_API_KEY
	BaseURL  string `yaml:"base_url"` // default https://api.openai.com/v1
	// Transcription is the model that writes down what the user says, for
	// the conversation's log; default gpt-transcribe.
	Transcription string `yaml:"transcription"`
	// Instructions are added to the voice model's built-in instructions.
	Instructions string `yaml:"instructions"`
	// TTL is how long a minted client secret stays valid to start a call
	// (10s to 2h); default 10m.
	TTL time.Duration `yaml:"ttl"`
}

// Talk configures `hk talk`, the local voice loop.
type Talk struct {
	// Wake phrases start a turn when an utterance begins with one; empty
	// means every utterance is a turn unless -wake is given.
	Wake []string `yaml:"wake"`
	// FollowUp is how long after a reply no wake phrase is needed; default 30s.
	FollowUp time.Duration `yaml:"follow_up"`
	// Silence ends an utterance; default 800ms.
	Silence time.Duration `yaml:"silence"`
	// Threshold is the least RMS level (0..1) counted as speech; the
	// detector also adapts to the room's noise floor. Default 0.02.
	Threshold float64 `yaml:"threshold"`
	// Input is ffmpeg's capture format: avfoundation (macOS default),
	// pulse or alsa (Linux; pulse when PulseAudio or PipeWire runs).
	Input string `yaml:"input"`
	// Device is the capture device: ":0" (avfoundation), "default"
	// (pulse, alsa) or e.g. "hw:1,0".
	Device string `yaml:"device"`
	// Player is the program that plays replies: afplay, paplay, aplay or
	// ffplay; default the first one found.
	Player string `yaml:"player"`
	// BargeIn stops a spoken reply when the user starts talking; default
	// true. Without headphones the mic may hear the speaker, so a louder
	// level is needed to interrupt.
	BargeIn *bool `yaml:"barge_in"`
}

func (c *Config) applyVoiceDefaults() {
	if r := c.Realtime; r != nil {
		if r.Provider == "" {
			r.Provider = "openai"
		}
		if r.Model == "" {
			r.Model = "gpt-realtime-2.1"
		}
		if r.Voice == "" {
			r.Voice = "marin"
		}
		if r.KeyEnv == "" {
			r.KeyEnv = "OPENAI_API_KEY"
		}
		if r.BaseURL == "" {
			r.BaseURL = "https://api.openai.com/v1"
		}
		if r.Transcription == "" {
			r.Transcription = "gpt-transcribe"
		}
		if r.TTL == 0 {
			r.TTL = 10 * time.Minute
		}
	}
	if t := c.Talk; t != nil {
		if t.FollowUp == 0 {
			t.FollowUp = 30 * time.Second
		}
		if t.Silence == 0 {
			t.Silence = 800 * time.Millisecond
		}
		if t.Threshold == 0 {
			t.Threshold = 0.02
		}
		if t.BargeIn == nil {
			on := true
			t.BargeIn = &on
		}
	}
}

func (c *Config) validateVoice() []error {
	var errs []error
	if r := c.Realtime; r != nil {
		if r.Provider != "openai" {
			errs = append(errs, fmt.Errorf("realtime: provider %q must be openai", r.Provider))
		}
		if !strings.HasPrefix(r.BaseURL, "https://") && !strings.HasPrefix(r.BaseURL, "http://") {
			errs = append(errs, fmt.Errorf("realtime: base_url %q must start with https:// or http://", r.BaseURL))
		}
		if r.TTL < 10*time.Second || r.TTL > 2*time.Hour {
			errs = append(errs, fmt.Errorf("realtime: ttl %s must be between 10s and 2h", r.TTL))
		}
	}
	if t := c.Talk; t != nil {
		switch t.Input {
		case "", "avfoundation", "pulse", "alsa":
		default:
			errs = append(errs, fmt.Errorf("talk: input %q must be avfoundation, pulse or alsa", t.Input))
		}
		switch t.Player {
		case "", "afplay", "paplay", "aplay", "ffplay":
		default:
			errs = append(errs, fmt.Errorf("talk: player %q must be afplay, paplay, aplay or ffplay", t.Player))
		}
		if t.Threshold < 0 || t.Threshold >= 1 {
			errs = append(errs, fmt.Errorf("talk: threshold %g must be between 0 and 1", t.Threshold))
		}
		if t.Silence < 200*time.Millisecond || t.Silence > 10*time.Second {
			errs = append(errs, fmt.Errorf("talk: silence %s must be between 200ms and 10s", t.Silence))
		}
		if t.FollowUp < 0 {
			errs = append(errs, fmt.Errorf("talk: follow_up must not be negative"))
		}
		for _, w := range t.Wake {
			if strings.TrimSpace(w) == "" {
				errs = append(errs, fmt.Errorf("talk: wake phrases must not be empty"))
				break
			}
		}
	}
	return errs
}

// TalkDefaults returns t with defaults filled in, or the defaults when t is
// nil (no talk section).
func TalkDefaults(t *Talk) *Talk {
	out := Talk{}
	if t != nil {
		out = *t
	}
	c := &Config{Talk: &out}
	c.applyVoiceDefaults()
	return c.Talk
}
