package web

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

const settingsConfig = `# Our settings. Keep this comment.
version: 1
providers:
  p: {type: anthropic, keys_env: [K]}
agents:
  habakkuk: {model: p/m}
channels:
  # The work Slack.
  slack:
    type: slack
    app_token_env: SLACK_APP_TOKEN # Socket Mode
    bot_tokens_env: [SLACK_BOT_TOKEN]
    allow_from: [U1]
  sms:
    type: telegram
    token_env: T
`

// chat is a messaging adapter that does nothing; with edits it can also
// edit its messages, as Slack and Telegram can.
type chat struct{}

func (chat) Run(ctx context.Context, _ channel.Inbox) error              { <-ctx.Done(); return nil }
func (chat) Send(context.Context, channel.Address, string, string) error { return nil }
func (chat) Format(md string) string                                     { return md }
func (chat) MaxLen() int                                                 { return 4000 }
func (chat) StaleAfter() time.Duration                                   { return time.Minute }

type editingChat struct{ chat }

func (editingChat) Post(context.Context, channel.Address, string) (string, error) { return "1", nil }
func (editingChat) Edit(context.Context, channel.Address, string, string) error   { return nil }
func (editingChat) Delete(context.Context, channel.Address, string) error         { return nil }

// newSettingsRig is a dashboard rig with two messaging channels whose
// display settings are saved to a real config.yaml, as serve does.
func newSettingsRig(t *testing.T) (*dashRig, string) {
	t.Helper()
	r := newDashRigWith(t, func(g *channel.Gateway) {
		g.Channels["slack"] = &config.Channel{Type: "slack", Agent: "habakkuk", AllowFrom: []string{"U1"}}
		g.Channels["sms"] = &config.Channel{Type: "telegram", Agent: "habakkuk"}
		g.Adapters["slack"], g.Adapters["sms"] = editingChat{}, chat{}
	})
	path := filepath.Join(r.home, "config.yaml")
	if err := os.WriteFile(path, []byte(settingsConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	home := r.home
	r.s.Gateway.SaveDisplay = func(account, progress, detail string) (*config.Channel, error) {
		return config.SetChannelDisplay(home, account, progress, detail)
	}
	return r, path
}

func TestDashChannelSettings(t *testing.T) {
	r, path := newSettingsRig(t)
	j := r.get(t, "/api/channels")
	chans := list(j, "channels")
	if len(chans) != 2 || j["writable"] != true {
		t.Fatalf("channels: %v", j)
	}
	slack, sms := chans[0].(map[string]any), chans[1].(map[string]any)
	if slack["account"] != "slack" || slack["type"] != "slack" || slack["progress"] != "plain" || slack["approvals_detail"] != "summary" || slack["live_progress"] != true {
		t.Fatalf("slack: %v", slack)
	}
	if sms["account"] != "sms" || sms["live_progress"] != false {
		t.Fatalf("sms: %v", sms)
	}
	if got := list(j, "progress"); len(got) != 3 || got[0] != "plain" || got[1] != "detailed" || got[2] != "off" {
		t.Fatalf("progress values: %v", got)
	}
	if got := list(j, "approvals_detail"); len(got) != 2 || got[0] != "summary" || got[1] != "full" {
		t.Fatalf("approvals_detail values: %v", got)
	}

	// Saving edits config.yaml in place, keeps its comments and a backup,
	// and the gateway uses the new value at once.
	saved := r.send(t, "PUT", "/api/channels/slack", `{"progress":"detailed"}`, http.StatusOK)
	if saved["progress"] != "detailed" || saved["approvals_detail"] != "summary" {
		t.Fatalf("saved: %v", saved)
	}
	saved = r.send(t, "PUT", "/api/channels/slack", `{"approvals_detail":"full","progress":"off"}`, http.StatusOK)
	if saved["progress"] != "off" || saved["approvals_detail"] != "full" {
		t.Fatalf("saved: %v", saved)
	}
	if d := r.s.Gateway.Display("slack"); d.Progress != config.ProgressOff || d.ApprovalsDetail != config.ApprovalsFull {
		t.Fatalf("the gateway did not pick the change up: %+v", d)
	}
	if d := r.s.Gateway.Display("sms"); d.Progress != config.ProgressPlain || d.ApprovalsDetail != config.ApprovalsSummary {
		t.Fatalf("the other channel changed: %+v", d)
	}
	data, _ := os.ReadFile(path)
	for _, keep := range []string{"# Our settings. Keep this comment.", "# The work Slack.", "# Socket Mode", "progress: off", "approvals_detail: full", "token_env: T"} {
		if !strings.Contains(string(data), keep) {
			t.Errorf("config.yaml lost %q:\n%s", keep, data)
		}
	}
	if bak, err := os.ReadFile(path + ".bak"); err != nil || !strings.Contains(string(bak), "# Our settings. Keep this comment.") {
		t.Errorf("no usable config.yaml.bak: %v", err)
	}
	if cfg, err := config.Load(r.home); err != nil || cfg.Channels["slack"].Progress != "off" || cfg.Channels["sms"].Progress != "" {
		t.Fatalf("config.yaml after the save: %v", err)
	}
	// The list and the config view show what is in effect now.
	if got := list(r.get(t, "/api/channels"), "channels")[0].(map[string]any); got["progress"] != "off" || got["approvals_detail"] != "full" {
		t.Fatalf("list after the save: %v", got)
	}

	// Bad requests change nothing.
	before, _ := os.ReadFile(path)
	for body, want := range map[string]int{
		`{"progress":"loud"}`:           http.StatusBadRequest,
		`{"approvals_detail":"always"}`: http.StatusBadRequest,
		`{"progress":""}`:               http.StatusBadRequest,
		`{}`:                            http.StatusBadRequest,
		`{"agent":"ops"}`:               http.StatusBadRequest, // only the two display settings can be set here
	} {
		r.send(t, "PUT", "/api/channels/slack", body, want)
	}
	r.send(t, "PUT", "/api/channels/nope", `{"progress":"off"}`, http.StatusNotFound)
	r.send(t, "PUT", "/api/channels/web", `{"progress":"off"}`, http.StatusNotFound) // the web chat has no such settings
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("a refused request changed config.yaml")
	}

	// The same guards as the rest of the dashboard API.
	if code, _ := r.do(t, "GET", "/api/channels", "", "", ""); code != http.StatusUnauthorized {
		t.Errorf("list without a token: %d", code)
	}
	if code, _ := r.do(t, "PUT", "/api/channels/slack", "", "application/json", `{"progress":"plain"}`); code != http.StatusUnauthorized {
		t.Errorf("save without a token: %d", code)
	}
	if code, _ := r.do(t, "PUT", "/api/channels/slack", token, "application/json", `{"progress":"plain"}`, "Sec-Fetch-Site", "cross-site"); code != http.StatusForbidden {
		t.Errorf("cross-site save: %d", code)
	}
	if code, _ := r.do(t, "PUT", "/api/channels/slack", token, "text/plain", `{"progress":"plain"}`); code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain save: %d", code)
	}
	if d := r.s.Gateway.Display("slack"); d.Progress != config.ProgressOff {
		t.Fatalf("a refused request changed the setting: %+v", d)
	}

	// A process that cannot save says so.
	r.s.Gateway.SaveDisplay = nil
	r.send(t, "PUT", "/api/channels/slack", `{"progress":"plain"}`, http.StatusNotImplemented)
	if r.get(t, "/api/channels")["writable"] != false {
		t.Error("the list still says the settings can be saved")
	}
}

// The first-run pointers on the Overview page are for an install nobody
// has used yet.
func TestDashOverviewKnowsAnEstablishedInstall(t *testing.T) {
	ctx := context.Background()
	fresh := newDashRig(t)
	if o := fresh.get(t, "/api/overview"); o["established"] != false {
		t.Fatalf("a fresh install: established = %v", o["established"])
	}
	// One messaging channel next to the web chat is still a fresh install:
	// that is what setup leaves. A second one is not.
	one := newDashRigWith(t, func(g *channel.Gateway) { g.Channels["tg"] = &config.Channel{Type: "telegram"} })
	if o := one.get(t, "/api/overview"); o["established"] != false {
		t.Fatalf("one channel: established = %v", o["established"])
	}
	two := newDashRigWith(t, func(g *channel.Gateway) {
		g.Channels["tg"], g.Channels["slack"] = &config.Channel{Type: "telegram"}, &config.Channel{Type: "slack"}
	})
	if o := two.get(t, "/api/overview"); o["established"] != true {
		t.Fatalf("two channels: established = %v", o["established"])
	}

	scheduled := newDashRig(t)
	scheduled.send(t, "POST", "/api/schedules", `{"name":"brief","agent":"habakkuk","when":"@every 1h","prompt":"brief me"}`, http.StatusCreated)
	if o := scheduled.get(t, "/api/overview"); o["established"] != true {
		t.Fatalf("with a schedule: established = %v", o["established"])
	}

	chatted := newDashRig(t)
	sess, err := chatted.s.Gateway.Runner.NewSession(ctx, "habakkuk")
	if err != nil {
		t.Fatal(err)
	}
	if o := chatted.get(t, "/api/overview"); o["established"] != false {
		t.Fatalf("an empty session: established = %v", o["established"])
	}
	st := chatted.s.Gateway.Runner.Sessions
	st.Append(ctx, sess.ID, "", 0, llm.UserText("hi"), "", nil)
	st.Append(ctx, sess.ID, "", 1, llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: "hello"}}}, "fake/m", nil)
	if o := chatted.get(t, "/api/overview"); o["established"] != true {
		t.Fatalf("after a chat turn: established = %v", o["established"])
	}
}
