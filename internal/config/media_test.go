package config

import (
	"strings"
	"testing"
)

const mediaBase = `version: 1
providers:
  p: {type: anthropic, keys_env: [K]}
agents:
  habakkuk: {model: p/m}
`

func TestMediaDefaults(t *testing.T) {
	cfg, err := Parse([]byte(mediaBase + `
channels:
  tg: {type: telegram, token_env: T, voice_replies: mirror}
tts: {provider: openai}
images: {provider: fal}
search: {provider: brave}
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TTS.KeyEnv != "OPENAI_API_KEY" || cfg.TTS.Format != "opus" || cfg.TTS.MaxChars != 2000 {
		t.Fatalf("tts: %+v", cfg.TTS)
	}
	if cfg.Images.KeyEnv != "FAL_KEY" || cfg.Search.KeyEnv != "BRAVE_API_KEY" {
		t.Fatalf("keys: %s %s", cfg.Images.KeyEnv, cfg.Search.KeyEnv)
	}
	ch := cfg.Channels["tg"]
	if ch.VoiceReplies != "mirror" || ch.VoiceText == nil || !*ch.VoiceText {
		t.Fatalf("channel: %+v", ch)
	}
	cfg, err = Parse([]byte(mediaBase + "channels:\n  tg: {type: telegram, token_env: T}\n"))
	if err != nil || cfg.Channels["tg"].VoiceReplies != "off" {
		t.Fatalf("voice replies default: %v", err)
	}
}

func TestMediaValidation(t *testing.T) {
	cases := map[string]string{
		"tts: {provider: polly}":                                                "tts: provider",
		"tts: {provider: openai, format: wav}":                                  "tts: format",
		"tts: {provider: elevenlabs}":                                           "needs voice",
		"images: {provider: midjourney}":                                        "images: provider",
		"search: {provider: google}":                                            "search: provider",
		"search: {provider: searxng}":                                           "searxng needs url",
		"search: {provider: brave, url: ftp://x}":                               "must start with http",
		"channels: {tg: {type: telegram, token_env: T, voice_replies: always}}": "needs a tts section",
		"channels: {tg: {type: telegram, token_env: T, voice_replies: loud}}\ntts: {provider: edge}": "must be off, mirror or always",
	}
	for extra, want := range cases {
		_, err := Parse([]byte(mediaBase + extra + "\n"))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", extra, want, err)
		}
	}
	for _, ok := range []string{
		"tts: {provider: edge, voice: en-GB-SoniaNeural, format: mp3}",
		"tts: {provider: elevenlabs, voice: abc, format: pcm_16000}",
		"search: {provider: searxng, url: http://localhost:8888}",
		"search: {provider: duckduckgo}",
	} {
		if _, err := Parse([]byte(mediaBase + ok + "\n")); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
}

func TestGuestApprovalsValidation(t *testing.T) {
	const slack = "channels: {s: {type: slack, app_token_env: A, bot_tokens_env: [B], %s}}\n"
	cases := map[string]string{
		`allow_from: ["*"], guest_approvals: maybe`: "must be deny or home",
		`allow_from: ["U1"], guest_approvals: home`: `needs allow_from "*"`,
		`allow_from: ["*"], guest_approvals: home`:  "needs a home chat on agent",
	}
	for extra, want := range cases {
		_, err := Parse([]byte(mediaBase + strings.Replace(slack, "%s", extra, 1)))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", extra, want, err)
		}
	}
	withHome := strings.Replace(mediaBase, "{model: p/m}", "{model: p/m, home: \"s:D1\"}", 1)
	if _, err := Parse([]byte(withHome + strings.Replace(slack, "%s", `allow_from: ["*"], guest_approvals: home`, 1))); err != nil {
		t.Errorf("guest_approvals: home with a home chat: %v", err)
	}
}
