package config

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Settings for the channel types beyond Telegram, Slack and WhatsApp. Each
// type has its own block under the channel, named after the type; only the
// block matching the channel's type may be set. Secrets are named, never
// stored: *_env fields name variables in the environment or .env.

// Email is a mailbox read over IMAP (IDLE when the server has it, else
// polling) and answered over SMTP. Each thread is a conversation.
type Email struct {
	Address     string        `yaml:"address"`      // Habakkuk's address: replies come from it
	Name        string        `yaml:"name"`         // display name on replies (default Habakkuk)
	IMAP        string        `yaml:"imap"`         // host:port, e.g. imap.example.com:993
	SMTP        string        `yaml:"smtp"`         // host:port, e.g. smtp.example.com:587
	IMAPTLS     string        `yaml:"imap_tls"`     // auto | tls | starttls | none (default auto: tls on 993, else starttls)
	SMTPTLS     string        `yaml:"smtp_tls"`     // auto | tls | starttls | none (default auto: tls on 465, else starttls)
	Username    string        `yaml:"username"`     // default address
	PasswordEnv string        `yaml:"password_env"` // IMAP and SMTP password (an app password)
	Folder      string        `yaml:"folder"`       // default INBOX
	Poll        time.Duration `yaml:"poll"`         // check interval when the server has no IDLE (default 1m)
	// AllowUnauthenticated accepts mail whose receiving server did not
	// record a passing DMARC or aligned DKIM result. Off by default: a From
	// header alone is trivial to forge.
	AllowUnauthenticated bool `yaml:"allow_unauthenticated"`
}

// SMS is a Twilio number. Twilio posts inbound messages to
// <http.public_url>/channels/<account>.
type SMS struct {
	AccountSID          string `yaml:"account_sid"`
	AuthTokenEnv        string `yaml:"auth_token_env"`
	From                string `yaml:"from"`                  // E.164 sender number, e.g. +15551234567
	MessagingServiceSID string `yaml:"messaging_service_sid"` // instead of from
}

// Teams is a Bot Framework bot (Azure Bot resource) whose messaging endpoint
// is <http.public_url>/channels/<account>.
type Teams struct {
	AppID          string `yaml:"app_id"`
	AppPasswordEnv string `yaml:"app_password_env"` // client secret
	TenantID       string `yaml:"tenant_id"`        // single-tenant bots; default botframework.com
}

// GoogleChat is a Chat app with an HTTP endpoint URL of
// <http.public_url>/channels/<account>.
type GoogleChat struct {
	// Audience is what Chat puts in its bearer token: the Google Cloud
	// project number, or the endpoint URL, as set under "Authentication
	// Audience" in the Chat API configuration.
	Audience string `yaml:"audience"`
	// CredentialsEnv names a variable holding the service account's JSON
	// key, or a path to it. Replies are sent with it.
	CredentialsEnv string `yaml:"credentials_env"`
}

// BlueBubbles is iMessage through a BlueBubbles server. Its webhook is
// <http.public_url>/channels/<account>?secret=<webhook secret>.
type BlueBubbles struct {
	URL              string `yaml:"url"`                // the BlueBubbles server, e.g. http://mac.local:1234
	PasswordEnv      string `yaml:"password_env"`       // server password
	WebhookSecretEnv string `yaml:"webhook_secret_env"` // must match ?secret= on the webhook URL
	PrivateAPI       bool   `yaml:"private_api"`        // the server has the Private API: typing, replies
	Mention          string `yaml:"mention"`            // in group chats, a message naming this is for Habakkuk (default Habakkuk)
}

// LINE is a Messaging API channel whose webhook URL is
// <http.public_url>/channels/<account>.
type LINE struct {
	ChannelSecretEnv string `yaml:"channel_secret_env"`
	AccessTokenEnv   string `yaml:"access_token_env"` // long-lived channel access token
}

// Mattermost is a bot account on a Mattermost server.
type Mattermost struct {
	URL      string `yaml:"url"`       // e.g. https://chat.example.com
	TokenEnv string `yaml:"token_env"` // the bot's access token
}

// IRC is one nick on one network.
type IRC struct {
	Server      string   `yaml:"server"`       // host:port, e.g. irc.libera.chat:6697
	TLS         *bool    `yaml:"tls"`          // default true
	Nick        string   `yaml:"nick"`         //
	Username    string   `yaml:"username"`     // default nick
	SASLUser    string   `yaml:"sasl_user"`    // default nick; used when password_env is set
	PasswordEnv string   `yaml:"password_env"` // SASL PLAIN password
	Channels    []string `yaml:"channels"`     // joined on connect, e.g. ["#habakkuk"]
}

// Ntfy publishes to a topic, and with subscribe set also reads one.
type Ntfy struct {
	URL       string `yaml:"url"`       // default https://ntfy.sh
	Topic     string `yaml:"topic"`     // replies and notifications go here
	Subscribe string `yaml:"subscribe"` // a topic whose messages become chat messages; may equal topic
	TokenEnv  string `yaml:"token_env"` // access token for protected topics
}

// WebhookTypes receive over Habakkuk's HTTP server.
var WebhookTypes = map[string]bool{"sms": true, "msteams": true, "googlechat": true, "bluebubbles": true, "line": true}

// SecretVars lists the variables a channel's credentials come from.
func (ch *Channel) SecretVars() []string {
	out := append([]string{ch.TokenEnv, ch.AppTokenEnv}, ch.BotTokensEnv...)
	switch {
	case ch.Email != nil:
		out = append(out, ch.Email.PasswordEnv)
	case ch.SMS != nil:
		out = append(out, ch.SMS.AuthTokenEnv)
	case ch.Teams != nil:
		out = append(out, ch.Teams.AppPasswordEnv)
	case ch.GoogleChat != nil:
		out = append(out, ch.GoogleChat.CredentialsEnv)
	case ch.BlueBubbles != nil:
		out = append(out, ch.BlueBubbles.PasswordEnv, ch.BlueBubbles.WebhookSecretEnv)
	case ch.LINE != nil:
		out = append(out, ch.LINE.ChannelSecretEnv, ch.LINE.AccessTokenEnv)
	case ch.Mattermost != nil:
		out = append(out, ch.Mattermost.TokenEnv)
	case ch.IRC != nil:
		out = append(out, ch.IRC.PasswordEnv)
	case ch.Ntfy != nil:
		out = append(out, ch.Ntfy.TokenEnv)
	}
	out = append(out, ch.platform2Secrets()...)
	var vars []string
	for _, v := range out {
		if v != "" {
			vars = append(vars, v)
		}
	}
	return vars
}

func (c *Config) applyPlatformDefaults(ch *Channel) {
	if ch == nil {
		return
	}
	if ch.Type == "email" || ch.Type == "googlechat" {
		for i, id := range ch.AllowFrom {
			ch.AllowFrom[i] = strings.ToLower(id) // addresses arrive lower-cased
		}
	}
	if ch.Type == "whatsapp" {
		for i, id := range ch.AllowFrom {
			ch.AllowFrom[i] = whatsappID(id)
		}
	}
	switch {
	case ch.Email != nil:
		e := ch.Email
		if ch.SessionIdle == 0 {
			// Each thread is its own conversation; a reply days later
			// still belongs to it.
			ch.SessionIdle = 30 * 24 * time.Hour
		}
		if e.Name == "" {
			e.Name = "Habakkuk"
		}
		if e.Username == "" {
			e.Username = e.Address
		}
		if e.Folder == "" {
			e.Folder = "INBOX"
		}
		if e.Poll == 0 {
			e.Poll = time.Minute
		}
		if e.IMAPTLS == "" {
			e.IMAPTLS = "auto"
		}
		if e.SMTPTLS == "" {
			e.SMTPTLS = "auto"
		}
	case ch.BlueBubbles != nil:
		if ch.BlueBubbles.Mention == "" {
			ch.BlueBubbles.Mention = "Habakkuk"
		}
	case ch.IRC != nil:
		i := ch.IRC
		if i.TLS == nil {
			on := true
			i.TLS = &on
		}
		if i.Username == "" {
			i.Username = i.Nick
		}
		if i.SASLUser == "" {
			i.SASLUser = i.Nick
		}
	case ch.Ntfy != nil:
		if ch.Ntfy.URL == "" {
			ch.Ntfy.URL = "https://ntfy.sh"
		}
	}
}

var (
	ntfyTopic  = regexp.MustCompile(`^[-_A-Za-z0-9]{1,64}$`)
	ircChannel = regexp.MustCompile(`^[#&][^\s,\x07]{1,49}$`)
	ircNick    = regexp.MustCompile(`^[A-Za-z\[\]\\^_{|}` + "`" + `][-A-Za-z0-9\[\]\\^_{|}` + "`" + `]{0,29}$`)
	e164       = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)
)

// validatePlatform checks the type-specific block of a channel.
func (c *Config) validatePlatform(name string, ch *Channel) []error {
	var errs []error
	bad := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("channel %s: "+format, append([]any{name}, args...)...))
	}
	blocks := map[string]bool{
		"email": ch.Email != nil, "sms": ch.SMS != nil, "msteams": ch.Teams != nil,
		"googlechat": ch.GoogleChat != nil, "bluebubbles": ch.BlueBubbles != nil, "line": ch.LINE != nil,
		"mattermost": ch.Mattermost != nil, "irc": ch.IRC != nil, "ntfy": ch.Ntfy != nil,
	}
	for block, set := range blocks {
		if set && block != ch.Type {
			bad("an %q block does not belong on a channel of type %s", block+":", ch.Type)
		}
	}
	if _, ok := blocks[ch.Type]; ok && !blocks[ch.Type] {
		bad("type %s needs its settings in an %q block", ch.Type, ch.Type+":")
		return errs
	}
	if WebhookTypes[ch.Type] {
		if c.HTTP == nil {
			bad("type %s receives webhooks, so it needs the http section", ch.Type)
		}
		if !webhookName.MatchString(name) {
			bad("its name is part of the webhook URL, so it must be 1-64 letters, digits, _ or -")
		}
	}
	needURL := func(field, v string) {
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			bad("%s %q must be an http:// or https:// URL", field, v)
		}
	}
	needHostPort := func(field, v string) {
		if _, _, err := net.SplitHostPort(v); err != nil {
			bad("%s %q must be host:port", field, v)
		}
	}
	required := func(fields ...string) {
		for i := 0; i+1 < len(fields); i += 2 {
			if fields[i+1] == "" {
				bad("%s is required for %s", fields[i], ch.Type)
			}
		}
	}
	tlsMode := func(field, v string) {
		switch v {
		case "auto", "tls", "starttls", "none":
		default:
			bad("%s %q must be auto, tls, starttls or none", field, v)
		}
	}
	switch ch.Type {
	case "email":
		e := ch.Email
		required("email.address", e.Address, "email.imap", e.IMAP, "email.smtp", e.SMTP, "email.password_env", e.PasswordEnv)
		if e.Address != "" && !strings.Contains(e.Address, "@") {
			bad("email.address %q is not an email address", e.Address)
		}
		if e.IMAP != "" {
			needHostPort("email.imap", e.IMAP)
		}
		if e.SMTP != "" {
			needHostPort("email.smtp", e.SMTP)
		}
		tlsMode("email.imap_tls", e.IMAPTLS)
		tlsMode("email.smtp_tls", e.SMTPTLS)
		if e.Poll < 10*time.Second {
			bad("email.poll must be at least 10s")
		}
	case "sms":
		s := ch.SMS
		required("sms.account_sid", s.AccountSID, "sms.auth_token_env", s.AuthTokenEnv)
		if (s.From == "") == (s.MessagingServiceSID == "") {
			bad("set exactly one of sms.from or sms.messaging_service_sid")
		}
		if s.From != "" && !e164.MatchString(s.From) {
			bad("sms.from %q must be an E.164 number like +15551234567", s.From)
		}
		if c.HTTP != nil && c.HTTP.PublicURL == "" {
			bad("sms needs http.public_url: Twilio signs the exact URL it calls")
		}
	case "msteams":
		required("msteams.app_id", ch.Teams.AppID, "msteams.app_password_env", ch.Teams.AppPasswordEnv)
	case "googlechat":
		g := ch.GoogleChat
		required("googlechat.audience", g.Audience, "googlechat.credentials_env", g.CredentialsEnv)
	case "bluebubbles":
		b := ch.BlueBubbles
		required("bluebubbles.url", b.URL, "bluebubbles.password_env", b.PasswordEnv,
			"bluebubbles.webhook_secret_env", b.WebhookSecretEnv)
		if b.URL != "" {
			needURL("bluebubbles.url", b.URL)
		}
	case "line":
		required("line.channel_secret_env", ch.LINE.ChannelSecretEnv, "line.access_token_env", ch.LINE.AccessTokenEnv)
	case "mattermost":
		m := ch.Mattermost
		required("mattermost.url", m.URL, "mattermost.token_env", m.TokenEnv)
		if m.URL != "" {
			needURL("mattermost.url", m.URL)
		}
	case "irc":
		i := ch.IRC
		required("irc.server", i.Server, "irc.nick", i.Nick)
		if i.Server != "" {
			needHostPort("irc.server", i.Server)
		}
		if i.Nick != "" && !ircNick.MatchString(i.Nick) {
			bad("irc.nick %q is not a valid nick", i.Nick)
		}
		for _, cn := range i.Channels {
			if !ircChannel.MatchString(cn) {
				bad("irc.channels: %q is not a channel name like #habakkuk", cn)
			}
		}
	case "ntfy":
		n := ch.Ntfy
		required("ntfy.topic", n.Topic)
		needURL("ntfy.url", n.URL)
		for _, t := range []string{n.Topic, n.Subscribe} {
			if t != "" && !ntfyTopic.MatchString(t) {
				bad("ntfy topic %q must be 1-64 letters, digits, - or _", t)
			}
		}
	}
	return errs
}

// whatsappID reduces an allow_from entry to the bare digits a WhatsApp
// sender arrives as, so a phone number and a LID can each be written either
// way: "+1 202-555-0142" and "12025550142@s.whatsapp.net" become
// 12025550142, and "98765432101234@lid" becomes 98765432101234.
func whatsappID(id string) string {
	if id == "*" {
		return id
	}
	user, _, _ := strings.Cut(strings.TrimSpace(id), "@")
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune("+ -()", r) {
			return -1
		}
		return r
	}, user)
}
