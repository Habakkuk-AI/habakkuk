package config

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestVoiceDefaults(t *testing.T) {
	cfg, err := Parse([]byte(mediaBase + "realtime: {}\ntalk: {wake: [hey habakkuk]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.Realtime
	if r.Provider != "openai" || r.Model != "gpt-realtime-2.1" || r.Voice != "marin" || r.KeyEnv != "OPENAI_API_KEY" ||
		r.BaseURL != "https://api.openai.com/v1" || r.Transcription != "gpt-transcribe" || r.TTL != 10*time.Minute {
		t.Fatalf("realtime: %+v", r)
	}
	tk := cfg.Talk
	if tk.FollowUp != 30*time.Second || tk.Silence != 800*time.Millisecond || tk.Threshold != 0.02 || !*tk.BargeIn ||
		!slices.Equal(tk.Wake, []string{"hey habakkuk"}) {
		t.Fatalf("talk: %+v", tk)
	}
	if !slices.Contains(cfg.SecretEnvNames(), "OPENAI_API_KEY") {
		t.Fatal("realtime key_env is not a secret name")
	}
	if d := TalkDefaults(nil); d.FollowUp != 30*time.Second || d.BargeIn == nil || len(d.Wake) != 0 {
		t.Fatalf("talk defaults: %+v", d)
	}
	if cfg, err := Parse([]byte(mediaBase)); err != nil || cfg.Realtime != nil || cfg.Talk != nil {
		t.Fatalf("voice sections appear without config: %v", err)
	}
}

func TestVoiceValidation(t *testing.T) {
	for _, c := range []struct{ yaml, want string }{
		{"realtime: {provider: gemini}", `realtime: provider "gemini" must be openai`},
		{"realtime: {base_url: api.example.com}", "must start with https://"},
		{"realtime: {ttl: 3h}", "ttl 3h0m0s must be between 10s and 2h"},
		{"talk: {input: dshow}", `talk: input "dshow"`},
		{"talk: {player: vlc}", `talk: player "vlc"`},
		{"talk: {threshold: 2}", "threshold 2 must be between 0 and 1"},
		{"talk: {silence: 50ms}", "silence 50ms must be between"},
		{"talk: {wake: ['  ']}", "wake phrases must not be empty"},
	} {
		_, err := Parse([]byte(mediaBase + c.yaml + "\n"))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.yaml, err, c.want)
		}
	}
}
