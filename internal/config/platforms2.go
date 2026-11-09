package config

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// More channel types, each configured in a block named after the type
// like those in platforms.go. All but zulip, rocketchat and twitch receive webhooks at
// <http.public_url>/channels/<account>.

// Platforms2 holds their blocks; Channel embeds it, so the blocks sit
// beside the others in YAML and in Go.
type Platforms2 struct {
	WhatsAppCloud *WhatsAppCloud `yaml:"whatsappcloud"`
	Messenger     *MetaApp       `yaml:"messenger"`
	Instagram     *MetaApp       `yaml:"instagram"`
	Feishu        *Feishu        `yaml:"feishu"`
	DingTalk      *DingTalk      `yaml:"dingtalk"`
	WeCom         *WeCom         `yaml:"wecom"`
	Zulip         *Zulip         `yaml:"zulip"`
	RocketChat    *RocketChat    `yaml:"rocketchat"`
	NextcloudTalk *NextcloudTalk `yaml:"nextcloudtalk"`
	Twitch        *Twitch        `yaml:"twitch"`
}

// WhatsAppCloud is a business phone number on the WhatsApp Business Cloud
// API (Meta's hosted API, not a linked device: see type whatsapp for that).
type WhatsAppCloud struct {
	PhoneNumberID  string `yaml:"phone_number_id"`  // the number's id in WhatsApp Manager, not the number
	AccessTokenEnv string `yaml:"access_token_env"` // a system user's permanent token
	AppSecretEnv   string `yaml:"app_secret_env"`   // the Meta app's secret: signs webhooks
	VerifyTokenEnv string `yaml:"verify_token_env"` // any string; enter the same when subscribing the webhook
	// Template is an approved message template sent when a customer last
	// wrote more than 24 hours ago, when WhatsApp allows nothing else.
	// With template_text its body has one variable, {{1}}, that carries
	// the reply; without, it goes out once as an invitation to write back.
	// Unset, such replies fail with the reason.
	Template         string `yaml:"template"`
	TemplateLanguage string `yaml:"template_language"` // default en_US
	TemplateText     bool   `yaml:"template_text"`
}

// MetaApp is a Facebook Page's Messenger inbox, or the Instagram
// professional account linked to the Page, reached with a Page token.
type MetaApp struct {
	PageID         string `yaml:"page_id"`          // optional: the Page (or Instagram account) id, to ignore others
	AccessTokenEnv string `yaml:"access_token_env"` // Page access token
	AppSecretEnv   string `yaml:"app_secret_env"`
	VerifyTokenEnv string `yaml:"verify_token_env"`
}

// Feishu is a Feishu or Lark custom app with the bot capability, subscribed
// to im.message.receive_v1 with the request URL <public_url>/channels/<account>.
type Feishu struct {
	AppID                string `yaml:"app_id"`
	AppSecretEnv         string `yaml:"app_secret_env"`
	VerificationTokenEnv string `yaml:"verification_token_env"`
	EncryptKeyEnv        string `yaml:"encrypt_key_env"` // recommended: encrypted, signed events
	Domain               string `yaml:"domain"`          // feishu (default, open.feishu.cn) or lark (open.larksuite.com)
	ReplyInThread        bool   `yaml:"reply_in_thread"` // answer group mentions in a thread under them
}

// DingTalk is an enterprise internal app's robot in HTTP mode, with the
// message receiving address <public_url>/channels/<account>.
type DingTalk struct {
	ClientID        string `yaml:"client_id"`         // the app's AppKey
	ClientSecretEnv string `yaml:"client_secret_env"` // AppSecret
	RobotCode       string `yaml:"robot_code"`        // default client_id
}

// WeCom is a WeCom self-built application whose API receiving URL is
// <public_url>/channels/<account>. WeCom only takes API calls from the
// app's trusted IPs, so add the server's address there.
type WeCom struct {
	CorpID    string `yaml:"corp_id"`
	AgentID   int    `yaml:"agent_id"`
	SecretEnv string `yaml:"secret_env"`  // the application's secret
	TokenEnv  string `yaml:"token_env"`   // the receiving URL's Token
	AESKeyEnv string `yaml:"aes_key_env"` // its EncodingAESKey
	Markdown  bool   `yaml:"markdown"`    // send markdown, which WeChat users see as plain text
}

// Zulip is a bot user in a Zulip organisation (a generic bot's email and
// API key from its settings).
type Zulip struct {
	URL       string `yaml:"url"`         // e.g. https://example.zulipchat.com
	Email     string `yaml:"email"`       // the bot's email
	APIKeyEnv string `yaml:"api_key_env"` // the bot's API key
}

// RocketChat is a bot user on a Rocket.Chat server, signed in with a
// personal access token.
type RocketChat struct {
	URL      string `yaml:"url"`       // e.g. https://chat.example.com
	UserID   string `yaml:"user_id"`   // shown with the token when it is made
	TokenEnv string `yaml:"token_env"` // the personal access token
}

// NextcloudTalk is a Talk bot installed with
// occ talk:bot:install --feature webhook --feature response --feature reaction
// <name> <secret> <public_url>/channels/<account>, then enabled in rooms.
type NextcloudTalk struct {
	URL       string `yaml:"url"`        // the Nextcloud server, e.g. https://cloud.example.com
	SecretEnv string `yaml:"secret_env"` // the bot's secret (40 to 128 characters)
	Mention   string `yaml:"mention"`    // "@" + this marks a message for Habakkuk (default Habakkuk)
}

// Twitch is a bot account in Twitch chat, over Twitch's IRC interface.
type Twitch struct {
	Login    string   `yaml:"login"`     // the bot account's login name
	TokenEnv string   `yaml:"token_env"` // a user access token with chat:read and chat:edit
	Channels []string `yaml:"channels"`  // channel logins to join, e.g. [mystream]
	Server   string   `yaml:"server"`    // default irc.chat.twitch.tv:6697 (TLS)
}

func init() {
	for _, t := range []string{"whatsappcloud", "messenger", "instagram", "feishu", "dingtalk", "wecom", "nextcloudtalk"} {
		WebhookTypes[t] = true
	}
}

// platform2Secrets lists the variables the blocks above name.
func (ch *Channel) platform2Secrets() []string {
	switch {
	case ch.WhatsAppCloud != nil:
		w := ch.WhatsAppCloud
		return []string{w.AccessTokenEnv, w.AppSecretEnv, w.VerifyTokenEnv}
	case ch.Messenger != nil:
		m := ch.Messenger
		return []string{m.AccessTokenEnv, m.AppSecretEnv, m.VerifyTokenEnv}
	case ch.Instagram != nil:
		m := ch.Instagram
		return []string{m.AccessTokenEnv, m.AppSecretEnv, m.VerifyTokenEnv}
	case ch.Feishu != nil:
		f := ch.Feishu
		return []string{f.AppSecretEnv, f.VerificationTokenEnv, f.EncryptKeyEnv}
	case ch.DingTalk != nil:
		return []string{ch.DingTalk.ClientSecretEnv}
	case ch.WeCom != nil:
		w := ch.WeCom
		return []string{w.SecretEnv, w.TokenEnv, w.AESKeyEnv}
	case ch.Zulip != nil:
		return []string{ch.Zulip.APIKeyEnv}
	case ch.RocketChat != nil:
		return []string{ch.RocketChat.TokenEnv}
	case ch.NextcloudTalk != nil:
		return []string{ch.NextcloudTalk.SecretEnv}
	case ch.Twitch != nil:
		return []string{ch.Twitch.TokenEnv}
	}
	return nil
}

func (c *Config) applyPlatform2Defaults(ch *Channel) {
	if ch == nil {
		return
	}
	switch {
	case ch.WhatsAppCloud != nil:
		if ch.WhatsAppCloud.TemplateLanguage == "" {
			ch.WhatsAppCloud.TemplateLanguage = "en_US"
		}
	case ch.Feishu != nil:
		if ch.Feishu.Domain == "" {
			ch.Feishu.Domain = "feishu"
		}
	case ch.NextcloudTalk != nil:
		if ch.NextcloudTalk.Mention == "" {
			ch.NextcloudTalk.Mention = "Habakkuk"
		}
	case ch.DingTalk != nil:
		if ch.DingTalk.RobotCode == "" {
			ch.DingTalk.RobotCode = ch.DingTalk.ClientID
		}
	}
}

var (
	digits       = regexp.MustCompile(`^[0-9]{1,32}$`)
	templateName = regexp.MustCompile(`^[a-z0-9_]{1,512}$`)
	twitchLogin  = regexp.MustCompile(`^[A-Za-z0-9_]{1,25}$`)
)

// validatePlatform2 checks the blocks above. Webhook needs (the http
// section, a URL-safe name) are checked by validatePlatform.
func (c *Config) validatePlatform2(name string, ch *Channel) []error {
	var errs []error
	bad := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("channel %s: "+format, append([]any{name}, args...)...))
	}
	blocks := map[string]bool{
		"whatsappcloud": ch.WhatsAppCloud != nil, "messenger": ch.Messenger != nil, "instagram": ch.Instagram != nil,
		"feishu": ch.Feishu != nil, "dingtalk": ch.DingTalk != nil, "wecom": ch.WeCom != nil,
		"zulip": ch.Zulip != nil, "rocketchat": ch.RocketChat != nil,
		"nextcloudtalk": ch.NextcloudTalk != nil, "twitch": ch.Twitch != nil,
	}
	for block, set := range blocks {
		if set && block != ch.Type {
			bad("an %q block does not belong on a channel of type %s", block+":", ch.Type)
		}
	}
	if set, ok := blocks[ch.Type]; !ok {
		return errs
	} else if !set {
		bad("type %s needs its settings in an %q block", ch.Type, ch.Type+":")
		return errs
	}
	required := func(fields ...string) {
		for i := 0; i+1 < len(fields); i += 2 {
			if fields[i+1] == "" {
				bad("%s is required for %s", fields[i], ch.Type)
			}
		}
	}
	switch ch.Type {
	case "whatsappcloud":
		w := ch.WhatsAppCloud
		required("whatsappcloud.phone_number_id", w.PhoneNumberID, "whatsappcloud.access_token_env", w.AccessTokenEnv,
			"whatsappcloud.app_secret_env", w.AppSecretEnv, "whatsappcloud.verify_token_env", w.VerifyTokenEnv)
		if w.PhoneNumberID != "" && !digits.MatchString(w.PhoneNumberID) {
			bad("whatsappcloud.phone_number_id %q must be the number's numeric id from WhatsApp Manager, not the phone number", w.PhoneNumberID)
		}
		if w.Template != "" && !templateName.MatchString(w.Template) {
			bad("whatsappcloud.template %q must be a template name: lower-case letters, digits and _", w.Template)
		}
		if w.TemplateText && w.Template == "" {
			bad("whatsappcloud.template_text needs whatsappcloud.template")
		}
	case "messenger", "instagram":
		m := ch.Messenger
		if ch.Type == "instagram" {
			m = ch.Instagram
		}
		required(ch.Type+".access_token_env", m.AccessTokenEnv, ch.Type+".app_secret_env", m.AppSecretEnv,
			ch.Type+".verify_token_env", m.VerifyTokenEnv)
		if m.PageID != "" && !digits.MatchString(m.PageID) {
			bad("%s.page_id %q must be a numeric id", ch.Type, m.PageID)
		}
	case "feishu":
		f := ch.Feishu
		required("feishu.app_id", f.AppID, "feishu.app_secret_env", f.AppSecretEnv,
			"feishu.verification_token_env", f.VerificationTokenEnv)
		if f.Domain != "feishu" && f.Domain != "lark" {
			bad("feishu.domain %q must be feishu or lark", f.Domain)
		}
	case "dingtalk":
		required("dingtalk.client_id", ch.DingTalk.ClientID, "dingtalk.client_secret_env", ch.DingTalk.ClientSecretEnv)
	case "wecom":
		w := ch.WeCom
		required("wecom.corp_id", w.CorpID, "wecom.secret_env", w.SecretEnv, "wecom.token_env", w.TokenEnv,
			"wecom.aes_key_env", w.AESKeyEnv)
		if w.AgentID <= 0 {
			bad("wecom.agent_id is required: the application's AgentId")
		}
	case "zulip":
		z := ch.Zulip
		required("zulip.url", z.URL, "zulip.email", z.Email, "zulip.api_key_env", z.APIKeyEnv)
		if u, err := url.Parse(z.URL); z.URL != "" && (err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "") {
			bad("zulip.url %q must be an http:// or https:// URL", z.URL)
		}
	case "rocketchat":
		r := ch.RocketChat
		required("rocketchat.url", r.URL, "rocketchat.user_id", r.UserID, "rocketchat.token_env", r.TokenEnv)
		if u, err := url.Parse(r.URL); r.URL != "" && (err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "") {
			bad("rocketchat.url %q must be an http:// or https:// URL", r.URL)
		}
	case "nextcloudtalk":
		n := ch.NextcloudTalk
		required("nextcloudtalk.url", n.URL, "nextcloudtalk.secret_env", n.SecretEnv)
		if u, err := url.Parse(n.URL); n.URL != "" && (err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "") {
			bad("nextcloudtalk.url %q must be an http:// or https:// URL", n.URL)
		}
	case "twitch":
		tw := ch.Twitch
		required("twitch.login", tw.Login, "twitch.token_env", tw.TokenEnv)
		if tw.Login != "" && !twitchLogin.MatchString(tw.Login) {
			bad("twitch.login %q is not a Twitch login", tw.Login)
		}
		if len(tw.Channels) == 0 {
			bad("twitch.channels lists no channel to join")
		}
		for _, c := range tw.Channels {
			if !twitchLogin.MatchString(strings.TrimPrefix(c, "#")) {
				bad("twitch.channels: %q is not a channel login", c)
			}
		}
	}
	return errs
}
