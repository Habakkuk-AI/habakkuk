package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProgressAndApprovalsDetailSettings(t *testing.T) {
	channels := func(extra string) string {
		return mediaBase + "channels:\n  tg: {type: telegram, token_env: T" + extra + "}\n"
	}
	for extra, want := range map[string][2]string{
		"":                            {ProgressPlain, ApprovalsSummary}, // plain is the default on every channel
		", progress: plain":           {ProgressPlain, ApprovalsSummary},
		", progress: detailed":        {ProgressDetailed, ApprovalsSummary},
		", progress: draft":           {ProgressDetailed, ApprovalsSummary}, // the old name still works
		", progress: off":             {ProgressOff, ApprovalsSummary},
		", approvals_detail: full":    {ProgressPlain, ApprovalsFull},
		", approvals_detail: summary": {ProgressPlain, ApprovalsSummary},
	} {
		cfg, err := Parse([]byte(channels(extra)))
		if err != nil {
			t.Fatalf("%q: %v", extra, err)
		}
		ch := cfg.Channels["tg"]
		if got := [2]string{ch.ProgressMode(), ch.ApprovalsDetailMode()}; got != want {
			t.Errorf("%q: %v, want %v", extra, got, want)
		}
	}
	for extra, want := range map[string]string{
		", progress: loud":           `progress "loud" must be plain, detailed or off`,
		", approvals_detail: always": `approvals_detail "always" must be summary or full`,
	} {
		if _, err := Parse([]byte(channels(extra))); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: want %q, got %v", extra, want, err)
		}
	}
	var none *Channel
	if none.ProgressMode() != ProgressPlain || none.ApprovalsDetailMode() != ApprovalsSummary {
		t.Error("a missing channel does not get the defaults")
	}
}

const displayConfig = `# My settings. Do not lose this comment.
version: 1
providers:
  p: {type: anthropic, keys_env: [K]} # the model provider

agents:
  habakkuk: {model: p/m}

# Where I talk to it.
channels:
  # The work Slack.
  slack:
    type: slack
    app_token_env: SLACK_APP_TOKEN # Socket Mode
    bot_tokens_env: [SLACK_BOT_TOKEN]
    allow_from: [U1] # me
    progress: draft # the old name
  tg:
    type: telegram
    token_env: T
`

func TestSetChannelDisplayEditsInPlace(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "config.yaml")
	if err := os.WriteFile(path, []byte(displayConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	ch, err := SetChannelDisplay(home, "slack", "plain", "full")
	if err != nil {
		t.Fatal(err)
	}
	if ch.ProgressMode() != ProgressPlain || ch.ApprovalsDetailMode() != ApprovalsFull {
		t.Fatalf("returned channel: %+v", ch)
	}
	out, _ := os.ReadFile(path)
	text := string(out)
	for _, keep := range []string{
		"# My settings. Do not lose this comment.", "# the model provider", "# Where I talk to it.", "# The work Slack.",
		"# Socket Mode", "# me", "progress: plain # the old name", "approvals_detail: full", "token_env: T",
	} {
		if !strings.Contains(text, keep) {
			t.Errorf("config.yaml lost %q:\n%s", keep, text)
		}
	}
	if bak, _ := os.ReadFile(path + ".bak"); string(bak) != displayConfig {
		t.Errorf("config.yaml.bak is not the previous file:\n%s", bak)
	}
	cfg, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Channels["slack"]; got.Progress != "plain" || got.ApprovalsDetail != "full" || got.AllowFrom[0] != "U1" {
		t.Fatalf("reloaded slack channel: %+v", got)
	}
	if got := cfg.Channels["tg"]; got.Progress != "" || got.ApprovalsDetail != "" {
		t.Fatalf("the other channel changed: %+v", got)
	}

	// One setting at a time leaves the other alone; draft is written as
	// detailed.
	if _, err := SetChannelDisplay(home, "slack", "draft", ""); err != nil {
		t.Fatal(err)
	}
	cfg, _ = Load(home)
	if got := cfg.Channels["slack"]; got.Progress != "detailed" || got.ApprovalsDetail != "full" {
		t.Fatalf("after changing only progress: %+v", got)
	}

	// Nothing is written for a value or a channel that does not exist, or
	// for no change at all.
	before, _ := os.ReadFile(path)
	bak, _ := os.ReadFile(path + ".bak")
	for _, bad := range [][3]string{{"slack", "loud", ""}, {"slack", "", "always"}, {"nope", "off", ""}} {
		if _, err := SetChannelDisplay(home, bad[0], bad[1], bad[2]); err == nil {
			t.Errorf("%v: no error", bad)
		}
	}
	if _, err := SetChannelDisplay(home, "slack", "detailed", "full"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	bakAfter, _ := os.ReadFile(path + ".bak")
	if string(after) != string(before) || string(bakAfter) != string(bak) {
		t.Fatal("config.yaml or its backup was rewritten although nothing changed")
	}
}
