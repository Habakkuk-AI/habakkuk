package whatsapp

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

// sendServers are the JID servers whatsmeow can send to.
var sendServers = []string{types.DefaultUserServer, types.HiddenUserServer, types.GroupServer,
	types.BroadcastServer, types.NewsletterServer, types.BotServer}

// parseTarget reads a conversation to send to. A phone number, with or
// without + and separators, becomes <digits>@s.whatsapp.net; a full JID
// (<digits>@lid, a group, ...) passes through. A target that can never be
// delivered is a channel.Undeliverable error, so its delivery stops at once
// instead of being retried.
func parseTarget(conv string) (types.JID, error) {
	s := strings.TrimSpace(conv)
	bad := func(why string) (types.JID, error) {
		return types.JID{}, channel.Undeliverable{Err: fmt.Errorf("whatsapp: cannot send to %q: %s", conv, why)}
	}
	if !strings.Contains(s, "@") {
		digits := strings.Map(func(r rune) rune {
			if strings.ContainsRune("+ -()", r) {
				return -1
			}
			return r
		}, s)
		if digits == "" || strings.Trim(digits, "0123456789") != "" {
			return bad("want a phone number (+15551234567) or a WhatsApp id such as 15551234567@s.whatsapp.net or 98765432101234@lid")
		}
		return types.NewJID(digits, types.DefaultUserServer), nil
	}
	jid, err := types.ParseJID(s)
	if err != nil {
		return bad(err.Error())
	}
	if jid.Server == types.LegacyUserServer {
		jid.Server = types.DefaultUserServer
	}
	if jid.User == "" || !slices.Contains(sendServers, jid.Server) {
		return bad("unknown WhatsApp server " + jid.Server)
	}
	return jid.ToNonAD(), nil
}

// NormalizeTarget returns a conversation in the form inbound messages use,
// so `hk send`, schedules and home chats can name a person by phone
// number (channel.TargetNormalizer).
func (a *Adapter) NormalizeTarget(conv string) (string, error) {
	jid, err := parseTarget(conv)
	if err != nil {
		return "", err
	}
	return jid.String(), nil
}

// sendErr marks failures that retrying cannot fix as undeliverable.
func sendErr(err error) error {
	if errors.Is(err, whatsmeow.ErrUnknownServer) || errors.Is(err, whatsmeow.ErrRecipientADJID) {
		return channel.Undeliverable{Err: err}
	}
	return err
}
