package cutover

import (
	"fmt"
	"slices"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/migrate"
)

// Shared is a channel both agents would use: the same bot token, which
// only one program at a time can poll, or a WhatsApp number still linked
// in the old agent.
type Shared struct {
	Channel string // the Habakkuk channel
	Type    string
	Why     string
}

func (s Shared) String() string { return s.Channel + " (" + s.Type + "): " + s.Why }

// SharedChannels lists the Habakkuk channels the old agent would use too.
// found is the source as hk migrate reads it; secret reads a Habakkuk
// variable (the environment, then $HABAKKUK_HOME/.env).
func SharedChannels(found *migrate.Found, cfg *config.Config, secret func(string) string) []Shared {
	if found == nil || cfg == nil {
		return nil
	}
	label := migrate.Label(found.From)
	var out []Shared
	names := slices.Sorted(func(yield func(string) bool) {
		for n := range cfg.Channels {
			if !yield(n) {
				return
			}
		}
	})
	for _, name := range names {
		ch := cfg.Channels[name]
		if ch.Type == "whatsapp" {
			if sessions := waSessions(found); len(sessions) > 0 {
				out = append(out, Shared{Channel: name, Type: ch.Type,
					Why: fmt.Sprintf("%s still has a linked WhatsApp session (%s); while it runs, its bridge answers on that number too", label, strings.Join(sessions, ", "))})
			}
			continue
		}
		mine := map[string]bool{}
		for _, v := range ch.SecretVars() {
			if v == "" {
				continue
			}
			if val := strings.TrimSpace(secret(v)); val != "" {
				mine[val] = true
			}
		}
		if len(mine) == 0 {
			continue
		}
		for _, c := range found.Channels {
			if c.Type != ch.Type {
				continue
			}
			same := false
			for _, v := range append([]string{c.TokenEnv, c.AppTokenEnv}, c.BotTokensEnv...) {
				if val := strings.TrimSpace(found.Secrets[v]); val != "" && mine[val] {
					same = true
				}
			}
			if same {
				out = append(out, Shared{Channel: name, Type: ch.Type,
					Why: fmt.Sprintf("the same %s token as %s's %s channel; only one program at a time can poll it", typeLabel(ch.Type), label, c.Type)})
				break
			}
		}
	}
	return out
}

// waSessions are the old agent's WhatsApp linked-device sessions that
// still hold a login.
func waSessions(found *migrate.Found) []string {
	sp := specs[found.From]
	if sp == nil || sp.waSessions == nil {
		return nil
	}
	return sp.waSessions(found.Dir)
}

func typeLabel(t string) string {
	switch t {
	case "telegram":
		return "Telegram bot"
	case "discord":
		return "Discord bot"
	case "slack":
		return "Slack app"
	case "matrix":
		return "Matrix"
	}
	return t
}
