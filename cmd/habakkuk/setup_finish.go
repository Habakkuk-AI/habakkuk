package main

import (
	"errors"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
)

// nameChoice is what the agent is called and what it calls the user.
type nameChoice struct {
	name string // "" leaves the identity alone
	user string // "" leaves USER.md alone
}

func (s *setup) agentDir() string { return filepath.Join(s.home, "agents", s.agent) }

func (s *setup) nameStep() error {
	s.heading(3, "Names")
	cur, hasID := agent.ReadIdentity(s.agentDir())
	def := "Habakkuk"
	if hasID {
		def = cur.Name
	}
	name := s.opts.name
	for {
		if name == "" {
			v, err := s.line("  What should your agent be called?", def)
			if err != nil {
				return err
			}
			name = v
		}
		if err := checkAgentName(name); err != nil {
			if !s.interactive {
				return fmt.Errorf("-name: %w", err)
			}
			s.say("  %v", err)
			name = ""
			continue
		}
		break
	}
	curUser := profileName(s.agentDir())
	user := s.opts.user
	if user == "" {
		v, err := s.line("  And what should it call you? (optional)", curUser)
		if err != nil {
			return err
		}
		user = v
	}
	if strings.ContainsAny(user, "\r\n") || utf8.RuneCountInString(user) > 60 {
		return errors.New("your name must be one line of at most 60 characters")
	}
	c := &nameChoice{}
	switch {
	case hasID && cur.Name != name:
		c.name = name
	case !hasID && !strings.EqualFold(name, s.agent): // the agent already goes by its own name
		c.name = name
	}
	if user != curUser {
		c.user = strings.TrimSpace(user)
	}
	s.identity = c
	if !s.interactive {
		who := ""
		if user != "" {
			who = "; it will call you " + user
		}
		s.say("  Your agent is %s%s.", name, who)
	}
	return nil
}

// checkAgentName applies the identity's rules before anything is written.
func checkAgentName(name string) error {
	switch {
	case strings.TrimSpace(name) == "":
		return errors.New("the name is empty")
	case utf8.RuneCountInString(name) > 40:
		return errors.New("the name is longer than 40 characters")
	case strings.ContainsAny(name, "\n\r:#"):
		return errors.New("the name must be one line without ':' or '#'")
	}
	return nil
}

var reProfileName = regexp.MustCompile(`(?m)^- Name: *(.*)$`)

// profileName is the user's name in USER.md, "" when there is none.
func profileName(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "USER.md"))
	if err != nil {
		return ""
	}
	if m := reProfileName.FindSubmatch(data); m != nil {
		return strings.TrimSpace(string(m[1]))
	}
	return ""
}

// setProfileName writes "- Name: X" under ## Basics in USER.md, the
// section the agent's own profile tool uses for it.
func setProfileName(dir, name string) error {
	path := filepath.Join(dir, "USER.md")
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	line := "- Name: " + name
	text := strings.TrimSpace(string(data))
	switch {
	case reProfileName.MatchString(text):
		text = reProfileName.ReplaceAllLiteralString(text, line)
	case slices.Contains(strings.Split(text, "\n"), "## Basics"):
		lines := strings.Split(text, "\n")
		i := slices.Index(lines, "## Basics") + 1
		for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
			i++
		}
		text = strings.Join(slices.Insert(lines, i, line), "\n")
	case text == "":
		text = "## Basics\n\n" + line
	default:
		text = "## Basics\n\n" + line + "\n\n" + text
	}
	return store.WriteFileAtomic(path, []byte(strings.TrimSpace(text)+"\n"), 0o600)
}

// writeAgentFiles writes SOUL.md (when missing), the identity and the
// user's name. An agent that has not met its user yet keeps its welcome,
// which then introduces it by the name chosen here.
func (s *setup) writeAgentFiles() error {
	dir := s.agentDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fresh := agent.NeedsOnboarding(dir)
	soul := filepath.Join(dir, "SOUL.md")
	if _, err := os.Stat(soul); errors.Is(err, os.ErrNotExist) {
		if err := store.WriteFileAtomic(soul, []byte(defaultSoul), 0o600); err != nil {
			return err
		}
	}
	c := s.identity
	if c == nil || c.name == "" && c.user == "" {
		return nil
	}
	if c.name != "" {
		id, _ := agent.ReadIdentity(dir)
		id.Name = c.name
		if err := agent.WriteIdentity(dir, id); err != nil {
			return fmt.Errorf("agent name: %w", err)
		}
	}
	if c.user != "" {
		if err := setProfileName(dir, c.user); err != nil {
			return err
		}
	}
	if fresh {
		return agent.KeepWelcome(dir)
	}
	return nil
}

// The name lives in IDENTITY.md, so SOUL.md does not repeat it.
const defaultSoul = "You are a sharp, warm and dependable personal AI agent. You act, verify, and report plainly.\n"

// agentName is what the agent goes by, for the closing lines.
func (s *setup) agentName() string {
	if s.identity != nil && s.identity.name != "" {
		return s.identity.name
	}
	if id, ok := agent.ReadIdentity(s.agentDir()); ok {
		return id.Name
	}
	return "Habakkuk"
}

// talk is the channel the closing lines are about: the one just set up,
// else the first configured one, else the terminal.
func (s *setup) talk() *channelChoice {
	if s.channel != nil {
		return s.channel
	}
	if s.cfg != nil {
		for _, typ := range channelTypes {
			for _, ch := range s.cfg.Channels {
				if ch != nil && ch.Type == typ {
					return &channelChoice{typ: typ, allow: ch.AllowFrom, exists: true}
				}
			}
		}
	}
	return &channelChoice{typ: "terminal"}
}

// nextStep says how to reach the agent once habakkuk serve runs.
func (s *setup) nextStep(c *channelChoice) string {
	var next string
	switch c.typ {
	case "whatsappagent":
		next = "send a message to your agent in WhatsApp"
		if c.found {
			next = "your agent answers the message you sent it in WhatsApp"
		}
	case "telegram":
		next = "send a message to your bot in Telegram"
		if c.bot != "" {
			next = "send a message to @" + c.bot + " in Telegram"
		}
	case "whatsapp":
		next = "send a message to the linked WhatsApp number"
	case "web":
		next = "open http://" + s.webListen() + "/ in your browser"
	case "slack":
		next = "send the bot a direct message in Slack"
	case "discord":
		next = "send the bot a direct message in Discord"
	}
	if len(c.allow) == 0 && c.typ != "web" && c.typ != "terminal" {
		next += " (the first message gets a pairing code: approve it with habakkuk pair CODE)"
	}
	return next
}

// finish links WhatsApp if needed, sums up what is now set up with the
// first things to try, then offers to keep the agent running (or to chat,
// for the terminal).
func (s *setup) finish() error {
	c := s.talk()
	if c.link {
		args := []string{"link", "-account", c.name}
		if c.phone != "" {
			args = append(args, "-phone", c.phone)
		}
		s.say("\nLinking WhatsApp: on your phone, open Settings > Linked devices > Link a device.")
		if err := whatsappCmd(s.ctx, s.home, args); err != nil {
			s.say("Linking did not finish: %v\nRun habakkuk whatsapp link when you are ready.", err)
		}
	}
	name := s.agentName()
	s.summary(c)
	if c.typ == "terminal" {
		if s.opts.serve {
			s.say("\nStarting habakkuk serve (Ctrl-C stops it); talk to %s with habakkuk chat in another terminal.", name)
			return s.runServe(s.ctx, s.home)
		}
		if !s.interactive { // a script: never open a chat it cannot type in
			return nil
		}
		if ok, err := s.yes("\nStart a chat now?", true); err != nil || !ok {
			return err
		}
		return s.runChat(s.ctx, s.home)
	}
	next := s.nextStep(c)
	if s.opts.service || s.interactive && (s.goos == "linux" || s.goos == "darwin") {
		want := s.opts.service
		if !want {
			var err error
			if want, err = s.yes("\nKeep "+name+" running in the background, now and after a restart?", true); err != nil {
				return err
			}
		}
		if want {
			if err := s.serviceStep(false); err != nil {
				return err
			}
			if s.interactive && s.installService() {
				s.say("\n%s is running in the background. Now %s.", name, next)
				return nil
			}
			s.say("Once it runs, %s.", next)
			return nil
		}
	}
	start := s.opts.serve
	if !start && s.interactive {
		var err error
		if start, err = s.yes("Start habakkuk serve in this terminal now?", true); err != nil {
			return err
		}
	}
	if start {
		s.say("\n%s now. (habakkuk serve is running; Ctrl-C stops it.)", capitalize(next))
		return s.runServe(s.ctx, s.home)
	}
	s.say("\nNext: run `habakkuk serve`, then %s.", next)
	return nil
}

// channelLabels name where the user talks, for the summary.
var channelLabels = map[string]string{
	"whatsappagent": "your WhatsApp agent",
	"telegram":      "Telegram",
	"whatsapp":      "WhatsApp (linked device)",
	"web":           "the web chat in your browser",
	"slack":         "Slack",
	"discord":       "Discord",
	"terminal":      "this terminal, with habakkuk chat",
}

// starterPrompts are things a fresh agent does well with no extra setup:
// a recurring brief (a schedule) and a memory. The dashboard's Chat page
// suggests the same ones.
var starterPrompts = []string{
	"Every weekday at 8, send me a short brief of my day.",
	"Remember that I like short, direct answers.",
}

// summary is the closing screen: what is set up, where things live, and
// the first things to try.
func (s *setup) summary(c *channelChoice) {
	name := s.agentName()
	cfg, err := config.Load(s.home)
	if err != nil {
		return // doctor has passed, so this does not happen; say nothing rather than guess
	}
	s.say("\n%s is ready. Setup took %s.\n", name, short(s.took))
	row := func(k, v string) { s.say("  %-10s %s", k, v) }
	if a := cfg.Agents[s.agent]; a != nil {
		m := a.Model
		if a.FastModel != "" {
			m += ", and " + a.FastModel + " for quick messages (cheaper)"
		}
		row("Model", m)
	}
	talk := channelLabels[c.typ]
	switch {
	case c.typ == "terminal" || c.typ == "web":
	case len(c.allow) == 1:
		talk += "; only you can message it"
	case len(c.allow) > 1:
		talk += fmt.Sprintf("; %d people can message it", len(c.allow))
	default:
		talk += "; you approve each new person with habakkuk pair"
	}
	if c.typ == "terminal" && cfg.HTTP != nil {
		talk += ", or Chat in the dashboard"
	}
	row("Talk", talk)
	if cfg.Memory != nil && cfg.Memory.Review {
		row("Memory", "on: "+name+" keeps what matters from each chat")
	} else {
		row("Memory", "notes only (turn on review: memory.review in config.yaml)")
	}
	if cfg.HTTP != nil {
		row("Dashboard", "http://"+cfg.HTTP.Listen+"/app/ while habakkuk serve runs")
	}
	row("Settings", s.cfgPath+" (keys in .env beside it)")

	s.say("\nTry these first:")
	first := capitalize(s.nextStep(c)) + "."
	if c.typ == "terminal" {
		first = "Run habakkuk chat and say hi."
	}
	s.say("  1. %s", first)
	s.say("  2. Get a morning brief: %q", starterPrompts[0])
	s.say("  3. Teach it about you: %q", starterPrompts[1])
	s.say("\nSchedules, channels and memory reviews run while habakkuk serve does.")
}

// installService copies the service file where the system looks for it
// and starts it, then waits for its first heartbeat. It reports whether
// the agent is now running; on false the commands to do it by hand have
// already been printed.
func (s *setup) installService() bool {
	home, err := filepath.Abs(s.home)
	if err != nil {
		return false
	}
	var src, dest string
	var cmds [][]string
	switch s.goos {
	case "darwin":
		src = filepath.Join(home, launchdLabel+".plist")
		dest = filepath.Join(s.userHome, "Library", "LaunchAgents", launchdLabel+".plist")
		domain := fmt.Sprintf("gui/%d", os.Getuid())
		cmds = [][]string{
			{"launchctl", "bootout", domain + "/" + launchdLabel}, // an older copy; fails harmlessly when none
			{"launchctl", "bootstrap", domain, dest},
		}
	case "linux":
		src = filepath.Join(home, "habakkuk.service")
		dest = filepath.Join(s.userHome, ".config", "systemd", "user", "habakkuk.service")
		cmds = [][]string{
			{"systemctl", "--user", "daemon-reload"},
			{"systemctl", "--user", "enable", "habakkuk"},
			{"systemctl", "--user", "restart", "habakkuk"},
		}
	default:
		return false
	}
	data, err := os.ReadFile(src)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(dest), 0o700)
	}
	if err == nil {
		err = store.WriteFileAtomic(dest, data, 0o600)
	}
	for i, c := range cmds {
		if err != nil {
			break
		}
		if e := s.runCmd(c[0], c[1:]...); e != nil && (s.goos != "darwin" || i > 0) { // launchctl bootout fails when nothing is loaded
			err = e
		}
	}
	if err != nil {
		s.say("  Could not start it for you: %v\n  Run the commands above instead.", err)
		return false
	}
	deadline := time.Now().Add(s.serveWait)
	for {
		if strings.HasPrefix(serveState(s.home, time.Now()), "running") {
			return true
		}
		if time.Now().After(deadline) {
			s.say("  The service is installed but has not started yet; habakkuk logs -f shows why.")
			return false
		}
		select {
		case <-s.ctx.Done():
			return false
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// runQuiet runs a command and returns its output as the error when it
// fails.
func runQuiet(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
		}
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

const launchdLabel = "ai.habakkuk.agent"

// serviceStep writes a systemd user unit (Linux) or a launchd agent
// (macOS) that runs habakkuk serve, into $HABAKKUK_HOME, and prints the
// commands that install and start it. It installs nothing itself.
func (s *setup) serviceStep(heading bool) error {
	if heading {
		s.say("Background service")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	home, err := filepath.Abs(s.home)
	if err != nil {
		return err
	}
	if strings.Contains(exe, "go-build") {
		s.say("  Note: %s is a temporary build (go run); install habakkuk and run this again so the service finds it.", exe)
	}
	path := s.getenv("PATH")
	var file, text string
	var cmds []string
	switch s.goos {
	case "linux":
		file = filepath.Join(home, "habakkuk.service")
		text = systemdUnit(exe, home, path)
		cmds = []string{
			"mkdir -p ~/.config/systemd/user && cp " + shellQuote(file) + " ~/.config/systemd/user/habakkuk.service",
			"systemctl --user daemon-reload && systemctl --user enable --now habakkuk",
			`loginctl enable-linger "$USER"    # keeps it running while you are logged out`,
		}
	case "darwin":
		file = filepath.Join(home, launchdLabel+".plist")
		text = launchdPlist(exe, home, path)
		dest := "~/Library/LaunchAgents/" + launchdLabel + ".plist"
		cmds = []string{
			"mkdir -p ~/Library/LaunchAgents && cp " + shellQuote(file) + " " + dest,
			"launchctl bootstrap gui/$(id -u) " + dest,
		}
	default:
		return fmt.Errorf("background services are set up for Linux (systemd) and macOS (launchd); on %s, run habakkuk serve with your system's service manager", s.goos)
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	if err := store.WriteFileAtomic(file, []byte(text), 0o600); err != nil {
		return err
	}
	s.say("Wrote %s. To install and start it, run:\n", file)
	for _, c := range cmds {
		s.say("  %s", c)
	}
	s.say("\nIts log: habakkuk logs -f. Stop habakkuk serve in other terminals first: only one may poll each channel.")
	return nil
}

func systemdUnit(exe, home, path string) string {
	q := func(v string) string {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%").Replace(v) + `"`
	}
	var b strings.Builder
	b.WriteString("# Written by habakkuk setup. Install: see the commands it printed.\n")
	b.WriteString("[Unit]\nDescription=Habakkuk personal AI agent\nWants=network-online.target\nAfter=network-online.target\n\n")
	b.WriteString("[Service]\n")
	fmt.Fprintf(&b, "ExecStart=%s serve\n", q(exe))
	fmt.Fprintf(&b, "Environment=%s\n", q("HABAKKUK_HOME="+home))
	if path != "" {
		fmt.Fprintf(&b, "Environment=%s\n", q("PATH="+path))
	}
	b.WriteString("Restart=on-failure\nRestartSec=5\n\n[Install]\nWantedBy=default.target\n")
	return b.String()
}

func launchdPlist(exe, home, path string) string {
	e := html.EscapeString
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- Written by habakkuk setup. Install: see the commands it printed. -->
<plist version="1.0">
<dict>
  <key>Label</key><string>` + launchdLabel + `</string>
  <key>ProgramArguments</key>
  <array><string>` + e(exe) + `</string><string>serve</string></array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>HABAKKUK_HOME</key><string>` + e(home) + `</string>
`)
	if path != "" {
		b.WriteString("    <key>PATH</key><string>" + e(path) + "</string>\n")
	}
	b.WriteString(`  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>StandardOutPath</key><string>` + e(filepath.Join(home, "serve.out.log")) + `</string>
  <key>StandardErrorPath</key><string>` + e(filepath.Join(home, "serve.err.log")) + `</string>
</dict>
</plist>
`)
	return b.String()
}

func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " '\"\\$`!*?&;|<>(){}[]#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
