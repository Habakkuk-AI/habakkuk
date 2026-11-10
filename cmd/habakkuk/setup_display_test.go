package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const displayHome = `# Written by hand. Keep me.
version: 1
providers:
  p: {type: anthropic, keys_env: [K]}
agents:
  habakkuk: {model: p/m}
channels:
  work:
    type: slack
    app_token_env: SLACK_APP_TOKEN # Socket Mode
    bot_tokens_env: [SLACK_BOT_TOKEN]
    allow_from: [U1]
  family:
    type: telegram
    token_env: TELEGRAM_BOT_TOKEN
  other:
    type: telegram
    token_env: OTHER_BOT_TOKEN
`

// hk setup channel NAME -progress ... -approvals-detail ... changes
// what one channel shows and nothing else: no wizard, no prompts.
func TestSetupChannelDisplayFlags(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "config.yaml")
	if err := os.WriteFile(path, []byte(displayHome), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &setupRun{}
	out, err := r.run(t, home, "channel", "work", "-progress", "detailed", "-approvals-detail", "full")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"Updated", "config.yaml.bak", "work: while the agent works the chat shows every step; approval requests show the exact command as well.", "No restart is needed."} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	cfg := mustLoad(t, home)
	if ch := cfg.Channels["work"]; ch.Progress != "detailed" || ch.ApprovalsDetail != "full" {
		t.Fatalf("work: %+v", ch)
	}
	if ch := cfg.Channels["family"]; ch.Progress != "" || ch.ApprovalsDetail != "" {
		t.Fatalf("family changed: %+v", ch)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "# Written by hand. Keep me.") || !strings.Contains(string(raw), "# Socket Mode") {
		t.Fatalf("comments lost:\n%s", raw)
	}
	if bak, _ := os.ReadFile(path + ".bak"); string(bak) != displayHome {
		t.Fatal("config.yaml.bak is not the previous file")
	}
	if r.served || r.chatted {
		t.Fatal("the display flags started something")
	}

	// One flag is enough; the type names the account when only one has it.
	if out, err := r.run(t, home, "channel", "slack", "-progress", "plain"); err != nil || !strings.Contains(out, "work: while the agent works the chat shows a short status line; approval requests show the exact command as well.") {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err := r.run(t, home, "channel", "work", "-progress", "plain"); err != nil || !strings.Contains(out, "needs no change") {
		t.Fatalf("%v\n%s", err, out)
	}

	before, _ := os.ReadFile(path)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"channel", "telegram", "-progress", "off"}, "more than one telegram channel is set up (family, other)"},
		{[]string{"channel", "nope", "-progress", "off"}, `no channel "nope" in config.yaml; the channels are: family, other, work`},
		{[]string{"channel", "work", "-progress", "loud"}, `-progress "loud" must be plain, detailed or off`},
		{[]string{"channel", "work", "-approvals-detail", "always"}, `-approvals-detail "always" must be summary or full`},
		{[]string{"channel", "-progress", "off"}, "hk setup channel NAME"},
		{[]string{"-progress", "off"}, "hk setup channel NAME"},
		{[]string{"model", "-progress", "off"}, "hk setup channel NAME"},
	} {
		if out, err := r.run(t, home, c.args...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: want %q, got %v\n%s", c.args, c.want, err, out)
		}
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("a refused command changed config.yaml")
	}
}
