package config

import (
	"slices"
	"strings"
	"testing"
	"time"
)

const opsBase = `version: 1
providers:
  anthropic: {type: anthropic, keys_env: [ANTHROPIC_API_KEY, ANTHROPIC_API_KEY_2]}
agents:
  habakkuk: {model: anthropic/claude-opus-5-5}
`

func TestOpsDefaults(t *testing.T) {
	cfg, err := Parse([]byte(opsBase + "telemetry: {otlp_endpoint: \"http://localhost:4318\"}\nsecrets:\n  sources: {1password: {}, vault: {type: command, command: x}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Logging.MaxMB != 20 || cfg.Logging.Backups != 5 || cfg.Metrics.Public || cfg.Tools.AllowPrivateURLs {
		t.Fatalf("%+v %+v", cfg.Logging, cfg.Metrics)
	}
	if cfg.Telemetry.ServiceName != "habakkuk" || cfg.Telemetry.IncludeContent {
		t.Fatalf("%+v", cfg.Telemetry)
	}
	if s := cfg.Secrets.Sources["1password"]; s.Type != "1password" || s.Cache != 5*time.Minute {
		t.Fatalf("%+v", s)
	}
}

func TestOpsValidation(t *testing.T) {
	for _, c := range []struct{ yaml, want string }{
		{"telemetry: {otlp_endpoint: \"localhost:4318\"}\n", "otlp_endpoint"},
		{"logging: {max_mb: -1}\n", "max_mb"},
		{"secrets:\n  sources: {vault: {}}\n", `type "" must be`},
		{"secrets:\n  sources: {pass: {type: command}}\n", "command is required"},
		{"secrets:\n  sources: {file: {command: cat}}\n", "applies to type command only"},
		{"secrets:\n  refs: {OPENAI_API_KEY: \"op://v/i/f\"}\n", "needs a source of type 1password"},
		{"secrets:\n  sources: {file: {}}\n  refs: {OPENAI_API_KEY: \"vault:x\"}\n", "names no source"},
		{"secrets:\n  sources: {file: {}}\n  refs: {\"bad-name\": \"file:x\"}\n", "is not a variable name"},
		{"tools: {allow_privat_urls: true}\n", "allow_privat_urls"},
	} {
		_, err := Parse([]byte(opsBase + c.yaml))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: got %v, want %q", c.yaml, err, c.want)
		}
	}
}

func TestSecretRef(t *testing.T) {
	cfg, err := Parse([]byte(opsBase + "secrets:\n  sources: {op: {type: 1password}, file: {}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if src, rest, err := cfg.SecretRef("op://Private/x/y"); src != "op" || rest != "op://Private/x/y" || err != nil {
		t.Fatal(src, rest, err)
	}
	if src, rest, err := cfg.SecretRef("file:~/k"); src != "file" || rest != "~/k" || err != nil {
		t.Fatal(src, rest, err)
	}
}

func TestSecretEnvNames(t *testing.T) {
	cfg, err := Parse([]byte(opsBase + `channels:
  tg: {type: telegram, token_env: TELEGRAM_BOT_TOKEN}
  sl: {type: slack, app_token_env: SLACK_APP_TOKEN, bot_tokens_env: [SLACK_BOT_A, SLACK_BOT_B]}
http:
  token_env: HABAKKUK_HTTP_TOKEN
  webhooks:
    alerts: {secret_env: ALERTS_SECRET}
mcp_servers:
  gh: {command: gh-mcp, env: {GITHUB_PERSONAL_ACCESS_TOKEN: GITHUB_TOKEN}}
  web: {url: "https://mcp.example.com", headers: {Authorization: "Bearer ${SEARCH_KEY}", X-Team: TEAM_VAR}}
telemetry: {otlp_endpoint: "https://otel.example.com", headers_env: OTLP_HEADERS}
secrets:
  sources: {file: {}}
  refs: {EXTRA_SECRET: "file:x"}
`))
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.SecretEnvNames()
	for _, want := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_API_KEY_2", "TELEGRAM_BOT_TOKEN", "SLACK_APP_TOKEN",
		"SLACK_BOT_A", "SLACK_BOT_B", "HABAKKUK_HTTP_TOKEN", "ALERTS_SECRET", "GITHUB_TOKEN", "SEARCH_KEY", "TEAM_VAR",
		"OTLP_HEADERS", "EXTRA_SECRET"} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	if !slices.IsSorted(got) {
		t.Errorf("not sorted: %v", got)
	}
}
