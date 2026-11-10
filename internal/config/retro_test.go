package config

import (
	"strings"
	"testing"
)

func withRetro(section string) (*Config, error) {
	text := Template + "channels:\n  tg:\n    type: telegram\n    token_env: TG_TOKEN\n"
	if section != "" {
		text += "retro:\n" + indent(section, "  ")
	}
	return Parse([]byte(text))
}

func TestRetroConfig(t *testing.T) {
	for _, tc := range []struct {
		section, spec string
	}{
		{"every: weekly", "0 17 * * 5"},
		{"every: weekly\nat: \"mon 09:30\"", "30 9 * * 1"},
		{"every: Weekly\nat: \"Sunday 18:00\"", "0 18 * * 0"},
		{"every: weekly\nat: \"Wednesday 07:05\"", "5 7 * * 3"},
		{"every: daily", "0 17 * * *"},
		{"every: daily\nat: \"08:15\"", "15 8 * * *"},
		{"every: \"0 9 * * 1-5\"", "0 9 * * 1-5"},
		{"every: \"@daily\"", "@daily"},
	} {
		cfg, err := withRetro(tc.section)
		if err != nil {
			t.Fatalf("%q: %v", tc.section, err)
		}
		if spec, ok := cfg.RetroSpec(); !ok || spec != tc.spec {
			t.Fatalf("%q: spec %q %v, want %q", tc.section, spec, ok, tc.spec)
		}
		if cfg.RetroApply() != RetroPropose {
			t.Fatalf("%q: apply %q", tc.section, cfg.RetroApply())
		}
	}

	// Off: no section, every off, or an empty section.
	for _, section := range []string{"", "every: off", "apply: safe"} {
		cfg, err := withRetro(section)
		if err != nil {
			t.Fatalf("%q: %v", section, err)
		}
		if spec, ok := cfg.RetroSpec(); ok {
			t.Fatalf("%q: retros on: %q", section, spec)
		}
	}

	cfg, err := withRetro("every: weekly\nagents: [habakkuk]\nmodel: anthropic/claude-opus-5-5\napply: safe\ndeliver: tg:12345")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RetroApply() != RetroSafe || cfg.RetroDeliver("habakkuk") != "tg:12345" {
		t.Fatalf("retro %+v", cfg.Retro)
	}
	// Deliver defaults to the agent's home chat; none is no chat.
	cfg, _ = withRetro("every: weekly")
	cfg.Agents["habakkuk"].Home = "tg:999"
	if got := cfg.RetroDeliver("habakkuk"); got != "tg:999" {
		t.Fatalf("default deliver %q", got)
	}
	cfg.Retro.Deliver = RetroDeliverNone
	if got := cfg.RetroDeliver("habakkuk"); got != "" {
		t.Fatalf("deliver none %q", got)
	}
}

func TestRetroConfigValidation(t *testing.T) {
	for _, tc := range []struct{ section, want string }{
		{"every: hourly", "must be off, daily, weekly or a cron expression"},
		{"every: \"*/5 * * * *\"", "more than once an hour"},
		{"every: daily\nat: \"fri 17:00\"", "a daily retro takes HH:MM"},
		{"every: weekly\nat: \"someday 17:00\"", "is not a weekday"},
		{"every: weekly\nat: \"monsoon 17:00\"", "is not a weekday"},
		{"every: weekly\nat: \"tu 17:00\"", "is not a weekday"},
		{"every: weekly\nat: \"25:00\"", "want HH:MM"},
		{"every: \"0 9 * * *\"\nat: \"09:00\"", "carries its own time"},
		{"at: \"09:00\"", "needs every"},
		{"agents: [nobody]", `agent "nobody" is not configured`},
		{"model: nowhere/model-x", `unknown provider "nowhere"`},
		{"model: just-a-model", "retro: model"},
		{"apply: always", `apply "always" must be propose or safe`},
		{"deliver: tg", "want CHANNEL:CHAT"},
		{"deliver: slack:C123", `names channel "slack"`},
		{"evry: weekly", "evry"},
	} {
		_, err := withRetro(tc.section)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: got %v, want %q", tc.section, err, tc.want)
		}
	}
}

func TestTemplateRetroSectionParses(t *testing.T) {
	// The commented example in the starter config is valid once uncommented.
	i := strings.Index(Template, "# retro:\n")
	if i < 0 {
		t.Fatal("no retro example in the template")
	}
	var b strings.Builder
	for _, line := range strings.Split(Template[i:], "\n") {
		if !strings.HasPrefix(line, "#") || line == "#" {
			break
		}
		b.WriteString(strings.TrimPrefix(line, "# ") + "\n")
	}
	if _, err := Parse([]byte(Template + b.String())); err != nil {
		t.Fatalf("uncommented example:\n%s\n%v", b.String(), err)
	}
}
