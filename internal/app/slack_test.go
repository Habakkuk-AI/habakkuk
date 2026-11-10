package app

import (
	"slices"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/channel/slack"
)

// Two Slack apps, one per workspace, are two channels of type slack: each
// gets its own adapter (its own Socket Mode connection) and both can route
// to the same agent.
func TestTwoSlackAppsGetTheirOwnAdapters(t *testing.T) {
	t.Setenv("SLACK_APP_TOKEN", "xapp-one")
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-one")
	t.Setenv("SLACK2_APP_TOKEN", "xapp-two")
	t.Setenv("SLACK2_BOT_TOKEN", "xoxb-two")
	a, _, _ := openTestApp(t, `
channels:
  slack:
    type: slack
    agent: habakkuk
    app_token_env: SLACK_APP_TOKEN
    bot_tokens_env: [SLACK_BOT_TOKEN]
    listen_channels: [C0123ABC]
  slack-acme:
    type: slack
    agent: habakkuk
    app_token_env: SLACK2_APP_TOKEN
    bot_tokens_env: [SLACK2_BOT_TOKEN]
`)
	one, ok1 := a.Gateway.Adapters["slack"].(*slack.Adapter)
	two, ok2 := a.Gateway.Adapters["slack-acme"].(*slack.Adapter)
	if !ok1 || !ok2 || one == two {
		t.Fatalf("adapters: %#v", a.Gateway.Adapters)
	}
	if one.AppToken != "xapp-one" || two.AppToken != "xapp-two" ||
		!slices.Equal(one.BotTokens, []string{"xoxb-one"}) || !slices.Equal(two.BotTokens, []string{"xoxb-two"}) {
		t.Fatalf("tokens: %q %q / %q %q", one.AppToken, one.BotTokens, two.AppToken, two.BotTokens)
	}
	if !slices.Equal(one.Listen, []string{"C0123ABC"}) || len(two.Listen) != 0 {
		t.Fatalf("listen: %v / %v", one.Listen, two.Listen)
	}
	if got := a.Cfg.ChannelsOf("habakkuk"); !slices.Contains(got, "slack") || !slices.Contains(got, "slack-acme") {
		t.Fatalf("channels of habakkuk: %v", got)
	}
}
