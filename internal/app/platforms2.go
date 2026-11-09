package app

import (
	"path/filepath"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/channel/dingtalk"
	"github.com/habakkuk-ai/habakkuk/internal/channel/feishu"
	"github.com/habakkuk-ai/habakkuk/internal/channel/messenger"
	"github.com/habakkuk-ai/habakkuk/internal/channel/nextcloudtalk"
	"github.com/habakkuk-ai/habakkuk/internal/channel/rocketchat"
	"github.com/habakkuk-ai/habakkuk/internal/channel/twitch"
	"github.com/habakkuk-ai/habakkuk/internal/channel/wecom"
	"github.com/habakkuk-ai/habakkuk/internal/channel/whatsappcloud"
	"github.com/habakkuk-ai/habakkuk/internal/channel/zulip"
	"github.com/habakkuk-ai/habakkuk/internal/config"
)

// platformAdapter2 builds the adapters for the types in config's
// platforms2.go. platformAdapter calls it for types it does not know and
// mounts the result; it returns nil for types it does not know either.
func (a *App) platformAdapter2(home, name string, ch *config.Channel) channel.Adapter {
	switch ch.Type {
	case "whatsappcloud":
		w := ch.WhatsAppCloud
		return whatsappcloud.New(whatsappcloud.Config{
			PhoneNumberID: w.PhoneNumberID, Token: a.Secret(w.AccessTokenEnv), AppSecret: a.Secret(w.AppSecretEnv),
			VerifyToken: a.Secret(w.VerifyTokenEnv), APIURL: ch.APIURL, Template: w.Template,
			TemplateLanguage: w.TemplateLanguage, TemplateText: w.TemplateText,
			StatePath: filepath.Join(home, "channels", "whatsappcloud-"+name+".json"),
		})
	case "messenger", "instagram":
		m := ch.Messenger
		if ch.Type == "instagram" {
			m = ch.Instagram
		}
		return messenger.New(messenger.Config{
			Instagram: ch.Type == "instagram", PageID: m.PageID, Token: a.Secret(m.AccessTokenEnv),
			AppSecret: a.Secret(m.AppSecretEnv), VerifyToken: a.Secret(m.VerifyTokenEnv), APIURL: ch.APIURL,
			WebhookURL: a.webhookURL(name),
		})
	case "feishu":
		f := ch.Feishu
		base := ch.APIURL
		if base == "" {
			base = feishu.FeishuURL
			if f.Domain == "lark" {
				base = feishu.LarkURL
			}
		}
		return feishu.New(feishu.Config{
			AppID: f.AppID, AppSecret: a.Secret(f.AppSecretEnv), VerificationToken: a.Secret(f.VerificationTokenEnv),
			EncryptKey: a.Secret(f.EncryptKeyEnv), APIURL: base, ReplyInThread: f.ReplyInThread,
		})
	case "dingtalk":
		d := ch.DingTalk
		return dingtalk.New(dingtalk.Config{ClientID: d.ClientID, ClientSecret: a.Secret(d.ClientSecretEnv),
			RobotCode: d.RobotCode, APIURL: ch.APIURL})
	case "wecom":
		w := ch.WeCom
		return wecom.New(wecom.Config{CorpID: w.CorpID, AgentID: w.AgentID, Secret: a.Secret(w.SecretEnv),
			Token: a.Secret(w.TokenEnv), EncodingAESKey: a.Secret(w.AESKeyEnv), Markdown: w.Markdown, APIURL: ch.APIURL})
	case "zulip":
		z := ch.Zulip
		return zulip.New(z.URL, z.Email, a.Secret(z.APIKeyEnv))
	case "rocketchat":
		r := ch.RocketChat
		return rocketchat.New(r.URL, r.UserID, a.Secret(r.TokenEnv))
	case "nextcloudtalk":
		n := ch.NextcloudTalk
		return nextcloudtalk.New(n.URL, a.Secret(n.SecretEnv), n.Mention)
	case "twitch":
		tw := ch.Twitch
		return twitch.New(tw.Server, tw.Login, a.Secret(tw.TokenEnv), tw.Channels)
	}
	return nil
}
