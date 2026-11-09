package config

import (
	"slices"
	"strings"
	"testing"
)

const morePlatforms = `
  wacloud:
    type: whatsappcloud
    whatsappcloud: {phone_number_id: "1234567890", access_token_env: WA_TOKEN, app_secret_env: META_SECRET, verify_token_env: META_VERIFY, template: come_back}
  fb:
    type: messenger
    messenger: {page_id: "42", access_token_env: FB_TOKEN, app_secret_env: META_SECRET, verify_token_env: META_VERIFY}
  ig:
    type: instagram
    instagram: {access_token_env: FB_TOKEN, app_secret_env: META_SECRET, verify_token_env: META_VERIFY}
  lark:
    type: feishu
    feishu: {app_id: cli_1, app_secret_env: LARK_SECRET, verification_token_env: LARK_VTOKEN, domain: lark}
  feishu:
    type: feishu
    feishu: {app_id: cli_2, app_secret_env: FS_SECRET, verification_token_env: FS_VTOKEN, encrypt_key_env: FS_KEY}
  ding:
    type: dingtalk
    dingtalk: {client_id: ding1, client_secret_env: DING_SECRET}
  wecom:
    type: wecom
    wecom: {corp_id: ww1, agent_id: 1000002, secret_env: WECOM_SECRET, token_env: WECOM_TOKEN, aes_key_env: WECOM_KEY}
  zulip:
    type: zulip
    zulip: {url: "https://example.zulipchat.com", email: bot@example.com, api_key_env: ZULIP_KEY}
  rc:
    type: rocketchat
    rocketchat: {url: "https://chat.example.com", user_id: u1, token_env: RC_TOKEN}
  talk:
    type: nextcloudtalk
    nextcloudtalk: {url: "https://cloud.example.com", secret_env: TALK_SECRET}
  tw:
    type: twitch
    twitch: {login: habakkuk_bot, token_env: TW_TOKEN, channels: ["#mystream", other]}
`

func TestPlatforms2Channels(t *testing.T) {
	cfg, err := Parse([]byte(withChannels(morePlatforms, httpSection)))
	if err != nil {
		t.Fatal(err)
	}
	if w := cfg.Channels["wacloud"].WhatsAppCloud; w.TemplateLanguage != "en_US" || w.Template != "come_back" {
		t.Fatalf("whatsappcloud defaults: %+v", w)
	}
	if cfg.Channels["feishu"].Feishu.Domain != "feishu" || cfg.Channels["lark"].Feishu.Domain != "lark" {
		t.Fatal("feishu domain")
	}
	if cfg.Channels["talk"].NextcloudTalk.Mention != "Habakkuk" {
		t.Fatal("nextcloudtalk mention default")
	}
	if cfg.Channels["ding"].DingTalk.RobotCode != "ding1" {
		t.Fatal("dingtalk robot code default")
	}
	for name, want := range map[string][]string{
		"wacloud": {"WA_TOKEN", "META_SECRET", "META_VERIFY"},
		"ig":      {"FB_TOKEN", "META_SECRET", "META_VERIFY"},
		"feishu":  {"FS_SECRET", "FS_VTOKEN", "FS_KEY"},
		"lark":    {"LARK_SECRET", "LARK_VTOKEN"},
		"ding":    {"DING_SECRET"},
		"wecom":   {"WECOM_SECRET", "WECOM_TOKEN", "WECOM_KEY"},
		"zulip":   {"ZULIP_KEY"},
		"rc":      {"RC_TOKEN"},
		"talk":    {"TALK_SECRET"},
		"tw":      {"TW_TOKEN"},
	} {
		if got := cfg.Channels[name].SecretVars(); !slices.Equal(got, want) {
			t.Errorf("%s secret vars = %v, want %v", name, got, want)
		}
	}
	if names := cfg.SecretEnvNames(); !slices.Contains(names, "WECOM_KEY") || !slices.Contains(names, "WA_TOKEN") {
		t.Fatalf("secret names = %v", names)
	}
	for _, typ := range []string{"whatsappcloud", "messenger", "instagram", "feishu", "dingtalk", "wecom", "nextcloudtalk"} {
		if !WebhookTypes[typ] {
			t.Errorf("%s is not a webhook type", typ)
		}
	}
}

func TestPlatforms2Validation(t *testing.T) {
	for _, tc := range []struct{ channels, http, want string }{
		{"\n  wa:\n    type: whatsappcloud\n", httpSection, `needs its settings in an "whatsappcloud:" block`},
		{"\n  wa:\n    type: whatsappcloud\n    whatsappcloud: {phone_number_id: \"1\", access_token_env: T, app_secret_env: S, verify_token_env: V}\n", "", "needs the http section"},
		{"\n  wa:\n    type: whatsappcloud\n    whatsappcloud: {phone_number_id: \"+15551234567\", access_token_env: T, app_secret_env: S, verify_token_env: V}\n", httpSection, "not the phone number"},
		{"\n  wa:\n    type: whatsappcloud\n    whatsappcloud: {phone_number_id: \"1\", access_token_env: T, app_secret_env: S}\n", httpSection, "whatsappcloud.verify_token_env is required"},
		{"\n  wa:\n    type: whatsappcloud\n    whatsappcloud: {phone_number_id: \"1\", access_token_env: T, app_secret_env: S, verify_token_env: V, template_text: true}\n", httpSection, "template_text needs"},
		{"\n  wa:\n    type: whatsappcloud\n    whatsappcloud: {phone_number_id: \"1\", access_token_env: T, app_secret_env: S, verify_token_env: V, template: Come Back}\n", httpSection, "template name"},
		{"\n  ig:\n    type: instagram\n    messenger: {access_token_env: T, app_secret_env: S, verify_token_env: V}\n", httpSection, `an "messenger:" block does not belong`},
		{"\n  fs:\n    type: feishu\n    feishu: {app_id: a, app_secret_env: S, verification_token_env: V, domain: china}\n", httpSection, "feishu.domain"},
		{"\n  fs:\n    type: feishu\n    feishu: {app_secret_env: S, verification_token_env: V}\n", httpSection, "feishu.app_id is required"},
		{"\n  dt:\n    type: dingtalk\n    dingtalk: {client_id: d}\n", httpSection, "dingtalk.client_secret_env is required"},
		{"\n  wc:\n    type: wecom\n    wecom: {corp_id: c, secret_env: S, token_env: T, aes_key_env: K}\n", httpSection, "wecom.agent_id is required"},
		{"\n  z:\n    type: zulip\n    zulip: {url: \"chat.example.com\", email: b@x, api_key_env: K}\n", "", "zulip.url"},
		{"\n  rc:\n    type: rocketchat\n    rocketchat: {url: \"https://chat.example.com\", token_env: T}\n", "", "rocketchat.user_id is required"},
		{"\n  tw:\n    type: twitch\n    twitch: {login: habakkuk_bot, token_env: T}\n", "", "twitch.channels lists no channel"},
		{"\n  tw:\n    type: twitch\n    twitch: {login: \"habakkuk bot\", token_env: T, channels: [a]}\n", "", "not a Twitch login"},
		{"\n  tg:\n    type: telegram\n    token_env: T\n    wecom: {corp_id: c}\n", "", `an "wecom:" block does not belong`},
	} {
		_, err := Parse([]byte(withChannels(tc.channels, tc.http)))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("channels %q: err = %v, want %q", tc.channels, err, tc.want)
		}
	}
}

// The template's commented examples for these types parse once
// uncommented (TestTemplateChannelExamples parses them all).
func TestTemplatePlatforms2Examples(t *testing.T) {
	for _, name := range []string{"wacloud", "fbpage", "insta", "feishu", "dingtalk", "wecom", "zulip", "rocketchat", "talk", "twitch"} {
		if !strings.Contains(Template, "\n#   "+name+":\n") {
			t.Errorf("template has no example channel %s", name)
		}
	}
}
