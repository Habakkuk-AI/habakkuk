package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/channel/discord"
	"github.com/habakkuk-ai/habakkuk/internal/channel/slack"
	"github.com/habakkuk-ai/habakkuk/internal/channel/telegram"
	"github.com/habakkuk-ai/habakkuk/internal/channel/whatsappagent"
	"github.com/habakkuk-ai/habakkuk/internal/config"
)

// channelTypes are the places setup can connect, in menu order.
var channelTypes = []string{"whatsappagent", "telegram", "whatsapp", "web", "slack", "discord", "terminal"}

var channelMenu = map[string]string{
	"whatsappagent": "WhatsApp agent: its own chat in your WhatsApp, no phone linking, no server  <- recommended",
	"telegram":      "Telegram bot",
	"whatsapp":      "WhatsApp linked device: answers the messages a number receives (QR or pairing code)",
	"web":           "Web chat in your browser, on this computer",
	"slack":         "Slack app",
	"discord":       "Discord bot",
	"terminal":      "Just the terminal (hk chat)",
}

// defaultAccount is the account name a new channel of a type gets.
var defaultAccount = map[string]string{"whatsappagent": "wagent", "telegram": "telegram", "whatsapp": "whatsapp",
	"slack": "slack", "discord": "discord", "web": config.WebAccount}

// channelChoice is the channel account setup adds or updates.
type channelChoice struct {
	typ       string
	name      string // account name under channels:
	exists    bool   // the account is in config.yaml already
	tokenEnv  string // telegram, discord, whatsappagent
	appEnv    string // slack
	botEnv    string // slack
	apiURL    string // written when not the platform's default
	allow     []string
	allowNote string // the comment next to allow_from
	found     bool   // allow comes from a message the user already sent
	bot       string // the bot's username, for the next-step line
	phone     string // whatsapp: link with a pairing code
	link      bool   // whatsapp: run the link flow after writing
}

func (s *setup) channelStep() error {
	s.heading(2, "Pick where to talk")
	existing := s.existingChannels()
	typ := s.opts.channel
	if typ == "" {
		if !s.interactive {
			if len(existing) > 0 {
				s.say("  Keeping %s.", strings.Join(existing, ", "))
			} else {
				s.say("  Just the terminal: talk with hk chat (add a channel later with hk setup channel).")
			}
			return nil
		}
		var labels []string
		var types []string
		if len(existing) > 0 {
			labels, types = append(labels, "Keep "+strings.Join(existing, ", ")), append(types, "")
		}
		for _, t := range channelTypes {
			l := channelMenu[t]
			if len(existing) > 0 {
				l = strings.TrimSuffix(l, "  <- recommended")
				if t != "terminal" {
					l = "Add: " + l
				}
			}
			labels, types = append(labels, l), append(types, t)
		}
		i, err := s.menu("Choose", labels, 0)
		if err != nil {
			return err
		}
		if typ = types[i]; typ == "" {
			return nil
		}
	}
	if typ == "terminal" {
		s.say("  Talk with hk chat. Add a channel any time with hk setup channel.")
		return nil
	}
	c := s.channelFor(typ)
	var err error
	switch typ {
	case "whatsappagent":
		err = s.setupWhatsAppAgent(c)
	case "telegram":
		err = s.setupTelegram(c)
	case "whatsapp":
		err = s.setupWhatsApp(c)
	case "slack":
		err = s.setupSlack(c)
	case "discord":
		err = s.setupDiscord(c)
	case "web":
		s.say("  The chat page will be at http://%s/ while hk serve runs (only this computer can reach it).", s.webListen())
	}
	if err != nil {
		return err
	}
	if s.opts.allow != "" {
		c.allow, c.allowNote, c.found = splitList(s.opts.allow), "", false
	}
	s.channel = c
	return nil
}

// existingChannels names the configured accounts, with their types.
func (s *setup) existingChannels() []string {
	if s.cfg == nil {
		return nil
	}
	var out []string
	for _, name := range slices.Sorted(maps.Keys(s.cfg.Channels)) {
		if ch := s.cfg.Channels[name]; ch != nil {
			if ch.Type == "web" && name == config.WebAccount {
				out = append(out, "web chat")
				continue
			}
			out = append(out, name+" ("+ch.Type+")")
		}
	}
	return out
}

// channelFor returns the account to set up: the existing one of that type
// (so a re-run updates it), or a new one.
func (s *setup) channelFor(typ string) *channelChoice {
	c := &channelChoice{typ: typ, name: defaultAccount[typ]}
	switch typ {
	case "whatsappagent":
		c.tokenEnv = "WHATSAPP_AGENT_KEY"
		if s.ep.whatsappAgent != whatsappagent.DefaultBase {
			c.apiURL = s.ep.whatsappAgent
		}
	case "telegram":
		c.tokenEnv = "TELEGRAM_BOT_TOKEN"
		c.apiURL = s.ep.telegram
	case "discord":
		c.tokenEnv = "DISCORD_BOT_TOKEN"
		c.apiURL = s.ep.discord
	case "slack":
		c.appEnv, c.botEnv = "SLACK_APP_TOKEN", "SLACK_BOT_TOKEN"
		c.apiURL = s.ep.slack
	}
	if s.cfg == nil {
		return c
	}
	for _, name := range slices.Sorted(maps.Keys(s.cfg.Channels)) {
		ch := s.cfg.Channels[name]
		if ch == nil || ch.Type != typ {
			continue
		}
		c.name, c.exists = name, true
		if ch.TokenEnv != "" {
			c.tokenEnv = ch.TokenEnv
		}
		if ch.AppTokenEnv != "" {
			c.appEnv = ch.AppTokenEnv
		}
		if len(ch.BotTokensEnv) > 0 {
			c.botEnv = ch.BotTokensEnv[0]
		}
		if ch.APIURL != "" {
			c.apiURL = ch.APIURL
		}
		c.allow = ch.AllowFrom
		return c
	}
	for taken := 2; s.cfg.Channels[c.name] != nil; taken++ { // a name used by another type
		c.name = fmt.Sprintf("%s%d", defaultAccount[typ], taken)
	}
	return c
}

// token asks for a channel secret (hidden), falling back to the one in
// the environment or .env. unchanged reports that it is the value an
// existing account already uses, which is not checked again: a running
// hk serve may be polling with it.
func (s *setup) token(c *channelChoice, env, prompt string) (value string, unchanged bool, err error) {
	cur, src := s.lookup(env)
	if !s.interactive {
		if cur == "" {
			return "", false, fmt.Errorf("set %s in the environment or in %s/.env (tokens are never passed as flags)", env, s.home)
		}
		return cur, c.exists, nil
	}
	if cur != "" {
		prompt += "; Enter keeps the one in " + src
	}
	for {
		v, err := s.hidden(prompt)
		if err != nil {
			return "", false, err
		}
		if v == "" {
			v = cur
		}
		if v != "" {
			return v, c.exists && v == cur, nil
		}
		s.say("  Paste it, or press Ctrl-C to stop.")
	}
}

// checked runs a live check unless -skip-verify, printing the outcome.
func (s *setup) checked(what string, secret string, check func(ctx context.Context) (string, error)) error {
	if s.opts.skipVerify {
		s.say("  Not checked (-skip-verify).")
		return nil
	}
	fmt.Fprintf(s.out, "  Checking %s ... ", what)
	ctx, cancel := context.WithTimeout(s.ctx, s.opts.timeout)
	defer cancel()
	msg, err := check(ctx)
	if err != nil {
		fmt.Fprintln(s.out, "failed")
		if errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("no answer within %s: check your connection, or raise -timeout", s.opts.timeout)
		}
		return s.redact(err, secret)
	}
	fmt.Fprintln(s.out, msg)
	return nil
}

// retry runs ask-and-check until the check passes (three tries), or once
// when not interactive.
func (s *setup) retry(step func() error) error {
	for attempt := 1; ; attempt++ {
		err := step()
		if err == nil || !s.interactive || attempt == 3 || s.ctx.Err() != nil {
			return err
		}
		s.say("  %v", err)
		s.say("  Let's try again.")
	}
}

func (s *setup) setupWhatsAppAgent(c *channelChoice) error {
	s.say("  1. In WhatsApp, open Settings > Agents > Create agent, and send your new agent a message (\"hi\").")
	s.say("  2. In the agent's chat, open Chat info > API key, and copy the key.")
	var senders []string
	err := s.retry(func() error {
		key, unchanged, err := s.token(c, c.tokenEnv, "Paste the agent's API key (hidden)")
		if err != nil {
			return err
		}
		if unchanged {
			s.say("  Same key as before; not checked again (hk serve may be using it).")
			return nil
		}
		ad := whatsappagent.New(whatsappagent.Config{Token: key, APIURL: s.ep.whatsappAgent})
		poll := func(ctx context.Context) (string, error) {
			var err error
			senders, err = ad.Check(ctx)
			switch {
			case errors.Is(err, whatsappagent.ErrKeyInUse):
				return "ok (the key works; something is already polling with it, probably hk serve)", nil
			case err != nil:
				return "", err
			case len(senders) > 0:
				return "ok, and your message is there", nil
			}
			return "ok", nil
		}
		if err := s.checked("the key with WhatsApp", key, poll); err != nil {
			return err
		}
		s.keep(c.tokenEnv, key)
		if len(senders) == 0 && s.interactive && !s.opts.skipVerify && s.opts.allow == "" && len(c.allow) == 0 {
			if _, err := s.line("  No message from you yet. Send your agent \"hi\" in WhatsApp, then press Enter", ""); err != nil {
				return err
			}
			if err := s.checked("for your message", key, poll); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	switch {
	case len(senders) > 0:
		c.allow, c.found = mergeIDs(c.allow, senders), true
		c.allowNote = "you: found your message, so only you are let in"
	case len(c.allow) == 0:
		c.allow = []string{"*"}
		c.allowNote = "WhatsApp lets only the agent's creator message it; put your id here to be stricter"
	}
	return nil
}

func (s *setup) setupTelegram(c *channelChoice) error {
	s.say("  1. In Telegram, message @BotFather: /newbot, pick a name, and copy the token it gives you.")
	s.say("  2. Open your new bot and press Start, so setup can let you in.")
	var senders []telegram.Sender
	err := s.retry(func() error {
		token, unchanged, err := s.token(c, c.tokenEnv, "Paste the bot token (hidden)")
		if err != nil {
			return err
		}
		if unchanged {
			s.say("  Same token as before; not checked again (hk serve may be using it).")
			return nil
		}
		tg := telegram.New(token, s.ep.telegram)
		err = s.checked("the token with Telegram", token, func(ctx context.Context) (string, error) {
			name, found, err := tg.Check(ctx)
			if err != nil {
				return "", fatalHint(err, "Telegram refused the token; copy it again from @BotFather")
			}
			senders, c.bot = found, name
			return "ok, the bot is @" + name, nil
		})
		if err == nil {
			s.keep(c.tokenEnv, token)
		}
		return err
	})
	if err != nil {
		return err
	}
	if s.opts.allow != "" || len(senders) == 0 {
		if len(c.allow) == 0 {
			c.allowNote = "empty: whoever writes gets a pairing code; approve with hk pair CODE"
		}
		return nil
	}
	var names []string
	var ids []string
	for _, p := range senders {
		names, ids = append(names, p.Name), append(ids, p.ID)
	}
	ok := false
	if s.interactive {
		var err error
		ok, err = s.yes(fmt.Sprintf("  %s wrote to the bot. Let them talk to your agent?", strings.Join(names, ", ")), true)
		if err != nil {
			return err
		}
	}
	if ok { // a bot can be found by anyone, so only a person at the keyboard decides
		c.allow, c.found = mergeIDs(c.allow, ids), true
		c.allowNote = strings.Join(names, ", ")
	}
	return nil
}

func (s *setup) setupWhatsApp(c *channelChoice) error {
	s.say("  Habakkuk becomes a linked device of a WhatsApp number and answers the messages that number")
	s.say("  receives. A second number is best: messages you send from it are not answered.")
	c.phone = s.opts.phone
	if c.phone == "" && s.interactive {
		v, err := s.line("  That number with country code, to link with a pairing code (Enter shows a QR code instead)", "")
		if err != nil {
			return err
		}
		c.phone = v
	}
	c.link = s.interactive || c.phone != ""
	if s.opts.allow == "" && s.interactive && len(c.allow) == 0 {
		v, err := s.line("  Numbers allowed to talk to it, comma-separated (Enter: approve people with hk pair)", "")
		if err != nil {
			return err
		}
		c.allow = splitList(v)
	}
	if len(c.allow) == 0 {
		c.allowNote = "empty: whoever writes gets a pairing code; approve with hk pair CODE"
	}
	return nil
}

func (s *setup) setupSlack(c *channelChoice) error {
	s.say("  1. Create an app at https://api.slack.com/apps, turn on Socket Mode, and subscribe to message events.")
	s.say("  2. Copy the app-level token (xapp-, scope connections:write) and, after installing it, the bot token (xoxb-).")
	err := s.retry(func() error {
		app, sameApp, err := s.token(c, c.appEnv, "App-level token (hidden)")
		if err != nil {
			return err
		}
		bot, sameBot, err := s.token(c, c.botEnv, "Bot token (hidden)")
		if err != nil {
			return err
		}
		if sameApp && sameBot {
			s.say("  Same tokens as before; not checked again (hk serve may be using them).")
			return nil
		}
		ad := slack.New(app, []string{bot}, s.ep.slack)
		err = s.checked("the tokens with Slack", app, func(ctx context.Context) (string, error) {
			teams, err := ad.Check(ctx)
			if err != nil {
				return "", s.redact(fatalHint(err, "Slack refused a token"), bot)
			}
			return "ok, workspace " + strings.Join(teams, ", "), nil
		})
		if err == nil {
			s.keep(c.appEnv, app)
			s.keep(c.botEnv, bot)
		}
		return err
	})
	if err == nil && len(c.allow) == 0 {
		c.allowNote = "empty: whoever writes gets a pairing code; approve with hk pair CODE"
	}
	return err
}

func (s *setup) setupDiscord(c *channelChoice) error {
	s.say("  1. At https://discord.com/developers/applications, create an application, add a bot, and turn on")
	s.say("     the Message Content intent. 2. Copy the bot's token, and invite it to your server (OAuth2 > URL Generator, scope bot).")
	err := s.retry(func() error {
		token, unchanged, err := s.token(c, c.tokenEnv, "Paste the bot token (hidden)")
		if err != nil {
			return err
		}
		if unchanged {
			s.say("  Same token as before; not checked again (hk serve may be using it).")
			return nil
		}
		ad := discord.New(token, s.ep.discord)
		err = s.checked("the token with Discord", token, func(ctx context.Context) (string, error) {
			name, err := ad.Check(ctx)
			if err != nil {
				return "", fatalHint(err, "Discord refused the token; reset and copy it again under Bot")
			}
			c.bot = name
			return "ok, the bot is " + name, nil
		})
		if err == nil {
			s.keep(c.tokenEnv, token)
		}
		return err
	})
	if err == nil && len(c.allow) == 0 {
		c.allowNote = "empty: whoever writes gets a pairing code; approve with hk pair CODE"
	}
	return err
}

// fatalHint explains a refused credential.
func fatalHint(err error, hint string) error {
	var f channel.Fatal
	if errors.As(err, &f) {
		return fmt.Errorf("%s (%v)", hint, f.Err)
	}
	return err
}

// webListen is the HTTP address: the configured one, else the default
// port, or the next free one when something else already has it.
func (s *setup) webListen() string {
	if s.cfg != nil && s.cfg.HTTP != nil && s.cfg.HTTP.Listen != "" {
		return s.cfg.HTTP.Listen
	}
	if s.listen == "" {
		s.listen = freeListen(config.DefaultListen, 16)
	}
	return s.listen
}

// freeListen returns addr, or the first of the next tries ports after it
// that nothing listens on; addr when none is free.
func freeListen(addr string, tries int) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	first, err := strconv.Atoi(port)
	if err != nil {
		return addr
	}
	for p := first; p < first+tries; p++ {
		a := net.JoinHostPort(host, strconv.Itoa(p))
		if ln, err := net.Listen("tcp", a); err == nil {
			_ = ln.Close()
			return a
		}
	}
	return addr
}

func splitList(v string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' }) {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func mergeIDs(have, add []string) []string {
	out := slices.Clone(have)
	for _, id := range add {
		if !slices.Contains(out, id) && !slices.Contains(out, "*") {
			out = append(out, id)
		}
	}
	return out
}

// setDisplay is `hk setup channel NAME -progress ... -approvals-detail
// ...`: it changes what one existing channel shows in its chats and nothing
// else. NAME is the account's name under channels:, or its type when only
// one account has that type.
func (s *setup) setDisplay() error {
	cfg, err := config.Load(s.home)
	if err != nil {
		return err
	}
	account, err := displayAccount(cfg, s.channelArg)
	if err != nil {
		return err
	}
	before, _ := os.ReadFile(s.cfgPath)
	ch, err := config.SetChannelDisplay(s.home, account, s.opts.progress, s.opts.approvalsDetail)
	if err != nil {
		return err
	}
	if after, _ := os.ReadFile(s.cfgPath); string(after) != string(before) {
		s.say("Updated %s (the previous one is config.yaml.bak).", s.cfgPath)
	} else {
		s.say("config.yaml needs no change.")
	}
	shows := map[string]string{config.ProgressPlain: "a short status line", config.ProgressDetailed: "every step", config.ProgressOff: "nothing",
		config.ApprovalsSummary: "a plain summary", config.ApprovalsFull: "the exact command as well"}
	s.say("%s: while the agent works the chat shows %s; approval requests show %s.", account, shows[ch.ProgressMode()], shows[ch.ApprovalsDetailMode()])
	s.say("A running hk serve picks this up in a few seconds. No restart is needed.")
	return nil
}

// displayAccount resolves the word after "setup channel" to a messaging
// account in the config.
func displayAccount(cfg *config.Config, arg string) (string, error) {
	var names, ofType []string
	for name, ch := range cfg.Channels {
		if !ch.MessagingChannel() {
			continue
		}
		names = append(names, name)
		if ch.Type == arg {
			ofType = append(ofType, name)
		}
	}
	slices.Sort(names)
	if slices.Contains(names, arg) {
		return arg, nil
	}
	switch {
	case len(ofType) == 1:
		return ofType[0], nil
	case len(ofType) > 1:
		slices.Sort(ofType)
		return "", fmt.Errorf("more than one %s channel is set up (%s); name the one to change", arg, strings.Join(ofType, ", "))
	case len(names) == 0:
		return "", errors.New("no chat channel is set up yet; add one with hk setup channel")
	}
	return "", fmt.Errorf("no channel %q in config.yaml; the channels are: %s", arg, strings.Join(names, ", "))
}
