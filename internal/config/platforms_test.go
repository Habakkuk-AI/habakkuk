package config

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func withChannels(channels, http string) string {
	return strings.Replace(Template, "\nworkers:", "\nchannels:"+channels+http+"\nworkers:", 1)
}

const allPlatforms = `
  mail:
    type: email
    allow_from: [Ay@Example.com]
    email:
      address: habakkuk@example.com
      imap: imap.example.com:993
      smtp: smtp.example.com:587
      password_env: MAIL_PASSWORD
  sms:
    type: sms
    sms: {account_sid: AC1, auth_token_env: TWILIO_TOKEN, from: "+15551234567"}
  teams:
    type: msteams
    msteams: {app_id: app-1, app_password_env: TEAMS_SECRET}
  gchat:
    type: googlechat
    googlechat: {audience: "123456", credentials_env: GCHAT_KEY}
  imessage:
    type: bluebubbles
    bluebubbles: {url: "http://mac.local:1234", password_env: BB_PW, webhook_secret_env: BB_HOOK}
  line:
    type: line
    line: {channel_secret_env: LINE_SECRET, access_token_env: LINE_TOKEN}
  mm:
    type: mattermost
    mattermost: {url: "https://chat.example.com", token_env: MM_TOKEN}
  irc:
    type: irc
    irc: {server: "irc.libera.chat:6697", nick: habakkuk, password_env: IRC_PW, channels: ["#habakkuk"]}
  push:
    type: ntfy
    ntfy: {topic: habakkuk-alerts, subscribe: habakkuk-in}
`

const httpSection = `
http:
  listen: 127.0.0.1:8484
  public_url: https://habakkuk.example.com/
`

func TestPlatformChannels(t *testing.T) {
	cfg, err := Parse([]byte(withChannels(allPlatforms, httpSection)))
	if err != nil {
		t.Fatal(err)
	}
	mail := cfg.Channels["mail"]
	if mail.Email.Folder != "INBOX" || mail.Email.Username != "habakkuk@example.com" || mail.Email.Poll != time.Minute ||
		mail.Email.IMAPTLS != "auto" || mail.SessionIdle != 30*24*time.Hour || mail.AllowFrom[0] != "ay@example.com" {
		t.Fatalf("email defaults: %+v %+v", mail, mail.Email)
	}
	if irc := cfg.Channels["irc"].IRC; !*irc.TLS || irc.SASLUser != "habakkuk" {
		t.Fatalf("irc defaults: %+v", irc)
	}
	if cfg.Channels["push"].Ntfy.URL != "https://ntfy.sh" || cfg.Channels["imessage"].BlueBubbles.Mention != "Habakkuk" {
		t.Fatal("ntfy or bluebubbles defaults missing")
	}
	if cfg.HTTP.PublicURL != "https://habakkuk.example.com" {
		t.Fatalf("public_url = %q", cfg.HTTP.PublicURL)
	}
	vars := cfg.Channels["imessage"].SecretVars()
	if !slices.Equal(vars, []string{"BB_PW", "BB_HOOK"}) {
		t.Fatalf("secret vars = %v", vars)
	}
}

func TestPlatformValidation(t *testing.T) {
	for _, tc := range []struct{ channels, http, want string }{
		{"\n  sms:\n    type: sms\n    sms: {account_sid: AC1, auth_token_env: T, from: \"+15551234567\"}\n", "", "needs the http section"},
		{"\n  sms:\n    type: sms\n    sms: {account_sid: AC1, auth_token_env: T, from: \"+15551234567\"}\n", "\nhttp: {listen: \"127.0.0.1:8484\"}\n", "sms needs http.public_url"},
		{"\n  sms:\n    type: sms\n    sms: {account_sid: AC1, auth_token_env: T, from: \"555\"}\n", httpSection, "E.164"},
		{"\n  sms:\n    type: sms\n    sms: {account_sid: AC1, auth_token_env: T}\n", httpSection, "exactly one of sms.from"},
		{"\n  mail:\n    type: email\n", "", `needs its settings in an "email:" block`},
		{"\n  mail:\n    type: email\n    email: {address: nope, imap: imap.example.com, smtp: s:25, password_env: P}\n", "", `email.imap "imap.example.com" must be host:port`},
		{"\n  mail:\n    type: email\n    email: {address: a@b.c, imap: i:993, smtp: s:25, password_env: P, imap_tls: maybe}\n", "", "email.imap_tls"},
		{"\n  tg:\n    type: telegram\n    token_env: T\n    irc: {server: \"x:6667\", nick: f}\n", "", `an "irc:" block does not belong`},
		{"\n  my teams:\n    type: msteams\n    msteams: {app_id: a, app_password_env: P}\n", httpSection, "part of the webhook URL"},
		{"\n  irc:\n    type: irc\n    irc: {server: \"x:6667\", nick: \"9bad\", channels: [habakkuk]}\n", "", `not a channel name`},
		{"\n  push:\n    type: ntfy\n    ntfy: {topic: \"a/b\"}\n", "", "ntfy topic"},
		{"\n  x:\n    type: carrier-pigeon\n", "", `type "carrier-pigeon" must be`},
		{"\n  sms:\n    type: sms\n    sms: {account_sid: AC1, auth_token_env: T, from: \"+15551234567\"}\n", "\nhttp:\n  public_url: \"ftp://x\"\n", "public_url"},
	} {
		_, err := Parse([]byte(withChannels(tc.channels, tc.http)))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("channels %q: err = %v, want %q", tc.channels, err, tc.want)
		}
	}
}

// The commented channel and http examples in the template must parse once
// uncommented.
func TestTemplateChannelExamples(t *testing.T) {
	var out []string
	in := false
	for _, line := range strings.Split(Template, "\n") {
		switch line {
		case "# channels:", "# http:":
			in = true
		case "":
			in = false
		}
		body, ok := strings.CutPrefix(line, "# ")
		if in && ok && (body == "channels:" || body == "http:" || strings.HasPrefix(body, "  ")) {
			line = body
		}
		out = append(out, line)
	}
	cfg, err := Parse([]byte(strings.Join(out, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"telegram", "wagent", "mail", "irc", "ntfy", "sms", "teams", "gchat", "imessage", "line"} {
		if cfg.Channels[name] == nil {
			t.Errorf("example channel %s missing", name)
		}
	}
}

func TestWhatsAppAgentChannel(t *testing.T) {
	cfg, err := Parse([]byte(withChannels("\n  wagent:\n    type: whatsappagent\n    token_env: WA_AGENT_KEY\n", "")))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Channels["wagent"].SecretVars(); !slices.Contains(got, "WA_AGENT_KEY") {
		t.Fatalf("secret vars = %v", got)
	}
	for _, tc := range []struct{ channels, want string }{
		{"\n  wagent:\n    type: whatsappagent\n", "token_env is required for whatsappagent"},
		{"\n  wagent:\n    type: whatsappagent\n    token_env: K\n    api_url: api.whatsapp.com\n", "must start with http"},
	} {
		if _, err := Parse([]byte(withChannels(tc.channels, ""))); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: err = %v, want %q", tc.channels, err, tc.want)
		}
	}
}

func TestSlackListenChannels(t *testing.T) {
	cfg, err := Parse([]byte(withChannels(`
  slack:
    type: slack
    app_token_env: SLACK_APP_TOKEN
    bot_tokens_env: [SLACK_BOT_TOKEN]
    listen_channels: [C0123ABC, T0456/C0456DEF]
`, "")))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Channels["slack"].ListenChannels; !slices.Equal(got, []string{"C0123ABC", "T0456/C0456DEF"}) {
		t.Fatalf("listen_channels = %v", got)
	}
	for _, tc := range []struct{ channels, want string }{
		{"\n  slack:\n    type: slack\n    app_token_env: A\n    bot_tokens_env: [B]\n    listen_channels: [\"#general\"]\n", "not a Slack channel id"},
		{"\n  tg:\n    type: telegram\n    token_env: T\n    listen_channels: [C0123ABC]\n", "listen_channels is a slack setting"},
	} {
		if _, err := Parse([]byte(withChannels(tc.channels, ""))); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: err = %v, want %q", tc.channels, err, tc.want)
		}
	}
}

func TestWhatsAppAllowFromTakesPhoneNumbersAndLIDs(t *testing.T) {
	cfg, err := Parse([]byte(withChannels(`
  whatsapp:
    type: whatsapp
    allow_from: ["+1 202-555-0142", "98765432101234@lid", "15551234567@s.whatsapp.net", "447700900123"]
`, "")))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"12025550142", "98765432101234", "15551234567", "447700900123"}
	if got := cfg.Channels["whatsapp"].AllowFrom; !slices.Equal(got, want) {
		t.Fatalf("allow_from = %q, want %q", got, want)
	}
}
