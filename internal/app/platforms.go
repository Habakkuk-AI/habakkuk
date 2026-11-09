package app

import (
	"path/filepath"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/channel/bluebubbles"
	"github.com/habakkuk-ai/habakkuk/internal/channel/email"
	"github.com/habakkuk-ai/habakkuk/internal/channel/googlechat"
	"github.com/habakkuk-ai/habakkuk/internal/channel/irc"
	"github.com/habakkuk-ai/habakkuk/internal/channel/line"
	"github.com/habakkuk-ai/habakkuk/internal/channel/mattermost"
	"github.com/habakkuk-ai/habakkuk/internal/channel/msteams"
	"github.com/habakkuk-ai/habakkuk/internal/channel/ntfy"
	"github.com/habakkuk-ai/habakkuk/internal/channel/sms"
	"github.com/habakkuk-ai/habakkuk/internal/config"
)

// platformAdapter builds the adapter for a channel type configured in its
// own block (email, sms, ...) and mounts the ones that receive webhooks on
// the HTTP server. It returns nil for types openChannels builds itself.
func (a *App) platformAdapter(home, name string, ch *config.Channel) channel.Adapter {
	state := func(kind string) string { return filepath.Join(home, "channels", kind+"-"+name+".json") }
	var ad channel.Adapter
	switch ch.Type {
	case "email":
		e := ch.Email
		ad = email.New(email.Config{
			Address: e.Address, Name: e.Name, IMAP: e.IMAP, SMTP: e.SMTP, IMAPTLS: e.IMAPTLS, SMTPTLS: e.SMTPTLS,
			Username: e.Username, Password: a.Secret(e.PasswordEnv), Folder: e.Folder, Poll: e.Poll,
			AllowUnauthenticated: e.AllowUnauthenticated, StatePath: state("email"),
		}, a.Log.With("channel", name))
	case "sms":
		s := ch.SMS
		ad = sms.New(s.AccountSID, a.Secret(s.AuthTokenEnv), s.From, s.MessagingServiceSID, a.webhookURL(name), ch.APIURL)
	case "msteams":
		t := ch.Teams
		ad = msteams.New(t.AppID, a.Secret(t.AppPasswordEnv), t.TenantID, state("msteams"), a.Log.With("channel", name))
	case "googlechat":
		g := ch.GoogleChat
		ad = googlechat.New(g.Audience, a.Secret(g.CredentialsEnv), ch.APIURL, a.Log.With("channel", name))
	case "bluebubbles":
		b := ch.BlueBubbles
		ad = bluebubbles.New(b.URL, a.Secret(b.PasswordEnv), a.Secret(b.WebhookSecretEnv), b.PrivateAPI, b.Mention).WithTyping()
	case "line":
		l := ch.LINE
		ad = line.New(a.Secret(l.ChannelSecretEnv), a.Secret(l.AccessTokenEnv), a.webhookURL(name), ch.APIURL)
	case "mattermost":
		ad = mattermost.New(ch.Mattermost.URL, a.Secret(ch.Mattermost.TokenEnv))
	case "irc":
		i := ch.IRC
		ad = irc.New(i.Server, *i.TLS, i.Nick, i.Username, i.SASLUser, a.Secret(i.PasswordEnv), i.Channels)
	case "ntfy":
		n := ch.Ntfy
		ad = ntfy.New(n.URL, n.Topic, n.Subscribe, a.Secret(n.TokenEnv), state("ntfy"))
	default:
		if ad = a.platformAdapter2(home, name, ch); ad == nil {
			return nil
		}
	}
	if ep, ok := ad.(channel.Endpoint); ok && a.Web != nil {
		a.Web.Mount(name, ep)
	}
	return ad
}

// webhookURL is where a platform reaches a channel: <public_url>/channels/<name>.
func (a *App) webhookURL(name string) string {
	if a.Cfg.HTTP == nil || a.Cfg.HTTP.PublicURL == "" {
		return ""
	}
	return a.Cfg.HTTP.PublicURL + "/channels/" + name
}
