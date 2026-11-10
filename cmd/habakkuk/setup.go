package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/habakkuk-ai/habakkuk/internal/channel/whatsappagent"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
)

// `habakkuk setup` takes a new user from download to a first reply in
// about a minute without editing YAML: pick a model (keys already in the
// environment and local servers are found), pick where to talk (a WhatsApp
// agent is the recommended one), name the agent, then it writes a small
// config.yaml, runs doctor and offers to start serving. Every answer has a
// flag, so it also runs unattended in scripts and CI. On an existing home
// it edits the config in place (after a backup) and keeps what it is not
// asked to change; `habakkuk setup model|channel|name|service` does one
// step on its own.

const setupUsage = `usage: habakkuk setup [model | channel [TYPE] | name | service] [flags]

With no step, runs them all: model, where to talk, names. Re-running edits
the existing config (config.yaml.bak keeps the previous one).

Flags (all optional; -y never prompts and uses flags, the environment, then defaults):
`

// setupOpts are the wizard's flags.
type setupOpts struct {
	yes        bool
	provider   string
	model      string
	baseURL    string
	keyEnv     string
	channel    string
	allow      string
	phone      string
	name       string
	user       string
	workspace  string
	agent      string
	skipVerify bool
	timeout    time.Duration
	serve      bool
	service    bool
	migrate    bool
}

// endpoints are the services setup checks against; tests point them at
// fakes. local maps local-server preset names to their base URLs ("" or a
// missing entry skips the probe).
type endpoints struct {
	whatsappAgent string
	telegram      string
	slack         string
	discord       string
	local         map[string]string
}

func defaultEndpoints() endpoints {
	ep := endpoints{whatsappAgent: whatsappagent.DefaultBase, local: map[string]string{}}
	for _, name := range localPresets {
		ep.local[name] = config.Presets[name].BaseURL
	}
	return ep
}

// setupCmd parses the arguments and runs the wizard on the terminal.
func setupCmd(ctx context.Context, home string, args []string) error {
	s, err := newSetup(ctx, home, args, os.Stdin, os.Stdout)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) && !s.opts.yes {
		s.secret = func() (string, error) {
			state, err := term.GetState(fd)
			if err != nil {
				return "", err
			}
			type res struct {
				b   []byte
				err error
			}
			ch := make(chan res, 1)
			go func() { b, err := term.ReadPassword(fd); ch <- res{b, err} }()
			select {
			case r := <-ch:
				fmt.Fprintln(s.out)
				return string(r.b), r.err
			case <-s.ctx.Done():
				_ = term.Restore(fd, state) // echo back on before exiting
				fmt.Fprintln(s.out)
				return "", s.ctx.Err()
			}
		}
	}
	return s.run()
}

// setup is one run of the wizard.
type setup struct {
	ctx         context.Context
	home        string
	in          *bufio.Reader
	out         io.Writer
	secret      func() (string, error) // hidden line; nil reads a plain one
	interactive bool
	steps       []string // model, channel, name; empty is all of them
	opts        setupOpts
	ep          endpoints
	getenv      func(string) string
	now         func() time.Time
	// runServe and runChat start the agent at the end; tests stub them.
	runServe func(ctx context.Context, home string) error
	runChat  func(ctx context.Context, home string) error
	// goos picks the background service flavour (tests set it).
	goos string
	// userHome is where setup looks for another agent's home to import
	// (tests set it).
	userHome string

	cfgPath  string
	raw      []byte         // config.yaml as it was, nil when new
	cfg      *config.Config // the same, parsed
	agent    string         // the agent being set up
	dotenv   map[string]string
	secrets  map[string]string // .env variables to write
	model    *modelChoice      // nil keeps the current model
	channel  *channelChoice    // nil adds none
	identity *nameChoice       // nil leaves names alone
	listen   string            // the HTTP address picked for a new home
	took     time.Duration     // how long the wizard took, for the summary
	// runCmd runs a command that installs the background service; tests
	// stub it.
	runCmd func(name string, args ...string) error
	// serveWait is how long to wait for a just-installed service to beat.
	serveWait time.Duration
}

func newSetup(ctx context.Context, home string, args []string, in io.Reader, out io.Writer) (*setup, error) {
	s := &setup{ctx: ctx, home: home, in: bufio.NewReader(in), out: out, ep: defaultEndpoints(),
		getenv: os.Getenv, now: time.Now, runServe: serve, goos: runtime.GOOS,
		runChat: func(ctx context.Context, home string) error { return chatCmd(ctx, home, nil) },
		cfgPath: filepath.Join(home, "config.yaml"), secrets: map[string]string{},
		runCmd: runQuiet, serveWait: 15 * time.Second}
	s.userHome, _ = os.UserHomeDir()
	var steps []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		steps, args = append(steps, args[0]), args[1:]
	}
	if len(steps) > 0 {
		switch steps[0] {
		case "model", "name", "service":
			if len(steps) > 1 {
				return nil, fmt.Errorf("habakkuk setup %s takes no argument %q", steps[0], steps[1])
			}
		case "channel":
			if len(steps) > 2 {
				return nil, errors.New("usage: habakkuk setup channel [TYPE]")
			}
			if len(steps) == 2 {
				args = append([]string{"-channel", steps[1]}, args...)
				steps = steps[:1]
			}
		case "help":
			steps = nil
			args = []string{"-h"}
		default:
			return nil, fmt.Errorf("unknown setup step %q (model, channel, name or service)", steps[0])
		}
	}
	s.steps = steps
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(out)
	o := &s.opts
	fs.BoolVar(&o.yes, "y", false, "non-interactive: never prompt")
	fs.BoolVar(&o.yes, "non-interactive", false, "same as -y")
	fs.StringVar(&o.provider, "provider", "", "model provider: a preset (anthropic, openai, gemini, deepseek, openrouter, ollama, ...), chatgpt or copilot (a login), or custom with -base-url")
	fs.StringVar(&o.model, "model", "", "model id (default: the provider's recommended one)")
	fs.StringVar(&o.baseURL, "base-url", "", "API base URL (custom providers, or to override a preset's)")
	fs.StringVar(&o.keyEnv, "key-env", "", "environment variable holding the model key (default: the preset's, e.g. ANTHROPIC_API_KEY)")
	fs.StringVar(&o.channel, "channel", "", "where to talk: "+strings.Join(channelTypes, ", "))
	fs.StringVar(&o.allow, "allow", "", "comma-separated sender ids allowed on the channel (default: found or approve with habakkuk pair)")
	fs.StringVar(&o.phone, "phone", "", "whatsapp: your number with country code, to link with a pairing code instead of a QR")
	fs.StringVar(&o.name, "name", "", `the agent's name (default "Habakkuk")`)
	fs.StringVar(&o.user, "user", "", "your name, saved in the agent's profile of you (USER.md)")
	fs.StringVar(&o.workspace, "workspace", "", "the agent's workspace folder (default ~/habakkuk-workspace)")
	fs.StringVar(&o.agent, "agent", "", "the agent to set up (default habakkuk, or the only one)")
	fs.BoolVar(&o.skipVerify, "skip-verify", false, "do not check keys and tokens with a live call")
	fs.DurationVar(&o.timeout, "timeout", 30*time.Second, "how long each live check may take")
	fs.BoolVar(&o.serve, "serve", false, "start habakkuk serve when done")
	fs.BoolVar(&o.service, "service", false, "write a background service file (systemd or launchd) and print how to enable it")
	fs.BoolVar(&o.migrate, "migrate", false, "with -y on a new home: first import another agent's setup found on this computer (see habakkuk migrate)")
	fs.Usage = func() {
		fmt.Fprint(out, setupUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q; see habakkuk setup -h", fs.Arg(0))
	}
	if o.serve && o.service {
		return nil, errors.New("-serve and -service both start habakkuk; pick one")
	}
	if o.channel != "" && !slices.Contains(channelTypes, o.channel) {
		return nil, fmt.Errorf("-channel %q must be one of %s", o.channel, strings.Join(channelTypes, ", "))
	}
	if o.keyEnv != "" && !envName.MatchString(o.keyEnv) {
		return nil, fmt.Errorf("-key-env %q is not a variable name (A-Z, 0-9 and _)", o.keyEnv)
	}
	if o.timeout <= 0 {
		o.timeout = 30 * time.Second
	}
	s.interactive = !o.yes
	return s, nil
}

func (s *setup) step(name string) bool {
	return len(s.steps) == 0 || slices.Contains(s.steps, name)
}

func (s *setup) run() error {
	start := s.now()
	if s.step("service") && len(s.steps) == 1 {
		return s.serviceStep(true)
	}
	if err := s.load(); err != nil {
		return err
	}
	if s.raw == nil && len(s.steps) == 0 {
		if err := s.offerImport(); err != nil {
			return err
		}
	}
	if s.raw == nil {
		s.say("Habakkuk setup: three short steps, about a minute. Nothing is written until the end.")
	} else {
		s.say("Habakkuk setup: editing %s (backed up to config.yaml.bak before any change).", s.cfgPath)
		if s.interactive {
			s.say("Enter keeps each setting.")
		}
	}
	if s.step("model") {
		if err := s.modelStep(); err != nil {
			return err
		}
	}
	if s.step("channel") {
		if err := s.channelStep(); err != nil {
			return err
		}
	}
	if s.step("name") {
		if err := s.nameStep(); err != nil {
			return err
		}
	}
	if err := s.write(); err != nil {
		return err
	}
	s.say("\nChecking everything with habakkuk doctor:")
	if err := doctor(s.ctx, s.home); err != nil {
		s.say("\nSetup is saved, but doctor found problems; fix them, then run habakkuk setup again.")
		return err
	}
	s.took = s.now().Sub(start)
	return s.finish()
}

// load reads the existing config and .env, and picks the agent.
func (s *setup) load() error {
	s.dotenv = readDotEnv(filepath.Join(s.home, ".env"))
	raw, err := os.ReadFile(s.cfgPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	default:
		cfg, err := config.ParseHome(raw, s.home)
		if err != nil {
			return fmt.Errorf("%s does not load, so setup cannot edit it safely: %w\nFix it, or move it away to start fresh", s.cfgPath, err)
		}
		s.raw, s.cfg = raw, cfg
	}
	s.agent = s.opts.agent
	if s.agent == "" {
		s.agent = "habakkuk"
		if s.cfg != nil {
			if _, ok := s.cfg.Agents["habakkuk"]; !ok && len(s.cfg.Agents) > 0 {
				s.agent = s.cfg.AgentNames()[0]
			}
		}
	}
	if strings.ContainsAny(s.agent, `/\ :`) || s.agent == "." || s.agent == ".." {
		return fmt.Errorf("-agent %q is not a valid agent name", s.agent)
	}
	if s.cfg == nil && !s.step("model") {
		s.steps = append([]string{"model"}, s.steps...) // a new home needs a model first
	}
	return nil
}

// lookup finds a variable in the environment, then in .env.
func (s *setup) lookup(name string) (value, source string) {
	if v := s.getenv(name); v != "" {
		return v, "environment"
	}
	if v := s.dotenv[name]; v != "" {
		return v, ".env"
	}
	return "", ""
}

// keep records a secret for .env unless .env already holds that value.
func (s *setup) keep(name, value string) {
	if value != "" && s.dotenv[name] != value {
		s.secrets[name] = value
	}
}

func (s *setup) say(format string, args ...any) {
	fmt.Fprintf(s.out, format+"\n", args...)
}

func (s *setup) heading(n int, title string) {
	total := 3
	if len(s.steps) > 0 {
		s.say("\n%s", title)
		return
	}
	s.say("\nStep %d of %d: %s", n, total, title)
}

// line asks for a line; def is what Enter gives. Ctrl-C ends the wizard
// even while it waits.
func (s *setup) line(prompt, def string) (string, error) {
	if !s.interactive {
		return def, nil
	}
	if def != "" {
		fmt.Fprintf(s.out, "%s [%s]: ", prompt, def)
	} else {
		fmt.Fprintf(s.out, "%s: ", prompt)
	}
	type res struct {
		s   string
		err error
	}
	ch := make(chan res, 1)
	go func() { l, err := s.in.ReadString('\n'); ch <- res{l, err} }()
	var r res
	select {
	case r = <-ch:
	case <-s.ctx.Done():
		fmt.Fprintln(s.out)
		return "", errors.New("setup cancelled; nothing was written")
	}
	if r.err != nil && (!errors.Is(r.err, io.EOF) || r.s == "") {
		if errors.Is(r.err, io.EOF) {
			return "", errors.New("setup cancelled: input ended (use -y to run without prompts)")
		}
		return "", r.err
	}
	if v := strings.TrimSpace(r.s); v != "" {
		return v, nil
	}
	return def, nil
}

// hidden asks for a secret without echoing it.
func (s *setup) hidden(prompt string) (string, error) {
	if !s.interactive {
		return "", nil
	}
	if s.secret == nil {
		return s.line(prompt, "")
	}
	fmt.Fprintf(s.out, "%s: ", prompt)
	v, err := s.secret()
	if err != nil {
		if s.ctx.Err() != nil {
			return "", errors.New("setup cancelled; nothing was written")
		}
		return "", err
	}
	return strings.TrimSpace(v), nil
}

func (s *setup) yes(prompt string, def bool) (bool, error) {
	if !s.interactive {
		return def, nil
	}
	d := "y/N"
	if def {
		d = "Y/n"
	}
	v, err := s.line(prompt+" ("+d+")", "")
	if err != nil {
		return false, err
	}
	switch strings.ToLower(v) {
	case "":
		return def, nil
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

// menu shows numbered options and returns the index picked (def when
// Enter is pressed or the wizard is not interactive).
func (s *setup) menu(prompt string, options []string, def int) (int, error) {
	if !s.interactive {
		return def, nil
	}
	for i, o := range options {
		fmt.Fprintf(s.out, "  %d) %s\n", i+1, o)
	}
	for {
		v, err := s.line(prompt, fmt.Sprint(def+1))
		if err != nil {
			return 0, err
		}
		var n int
		if _, err := fmt.Sscan(v, &n); err == nil && n >= 1 && n <= len(options) {
			return n - 1, nil
		}
		s.say("  Pick a number from 1 to %d.", len(options))
	}
}

// redact hides secret values that could appear in an error.
func (s *setup) redact(err error, extra ...string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	vals := append(slices.Collect(maps.Values(s.secrets)), extra...)
	for _, v := range vals {
		if len(v) >= 6 {
			msg = strings.ReplaceAll(msg, v, "[key]")
		}
	}
	if msg == err.Error() {
		return err
	}
	return errors.New(msg)
}

// write saves .env, config.yaml (after a backup) and the agent's files.
func (s *setup) write() error {
	out, changed, err := s.render()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.home, 0o700); err != nil {
		return err
	}
	// The home holds chats, memory and keys: only its owner may read it,
	// even when it existed before setup ran.
	if err := os.Chmod(s.home, 0o700); err != nil {
		return err
	}
	if len(s.secrets) > 0 {
		if err := setDotEnv(filepath.Join(s.home, ".env"), s.secrets); err != nil {
			return err
		}
		s.say("\nSaved %s in %s (readable only by you).", strings.Join(slices.Sorted(maps.Keys(s.secrets)), ", "), filepath.Join(s.home, ".env"))
	}
	if changed {
		if s.raw != nil {
			if err := store.WriteFileAtomic(s.cfgPath+".bak", s.raw, 0o600); err != nil {
				return err
			}
		}
		if err := store.WriteFileAtomic(s.cfgPath, out, 0o600); err != nil {
			return err
		}
		if s.raw != nil {
			s.say("Updated %s (the previous one is config.yaml.bak).", s.cfgPath)
		} else {
			s.say("Wrote %s.", s.cfgPath)
		}
	} else if s.raw != nil {
		s.say("config.yaml needs no change.")
	}
	return s.writeAgentFiles()
}

var envName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// readDotEnv reads KEY=VALUE lines like the app does.
func readDotEnv(path string) map[string]string {
	out := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if ok {
			out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return out
}

// setDotEnv sets variables in a .env file, replacing earlier lines for the
// same names and keeping everything else, and leaves it readable only by
// its owner.
func setDotEnv(path string, vars map[string]string) error {
	for k, v := range vars {
		if !envName.MatchString(k) || strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("cannot store %s in .env", k)
		}
	}
	var lines []string
	if data, err := os.ReadFile(path); err == nil {
		if t := strings.TrimRight(string(data), "\n"); t != "" {
			lines = strings.Split(t, "\n")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	done := map[string]bool{}
	var out []string
	for _, line := range lines {
		k, _, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(line), "export "), "=")
		k = strings.TrimSpace(k)
		if v, set := vars[k]; ok && set {
			if !done[k] { // the first line for a name holds the value; later ones go
				out, done[k] = append(out, k+"="+v), true
			}
			continue
		}
		out = append(out, line)
	}
	for _, k := range slices.Sorted(maps.Keys(vars)) {
		if !done[k] {
			out = append(out, k+"="+vars[k])
		}
	}
	return store.WriteFileAtomic(path, []byte(strings.Join(out, "\n")+"\n"), 0o600)
}

// short rounds a duration for people: 340ms, 2.4s.
func short(d time.Duration) time.Duration {
	if d < time.Second {
		return d.Round(time.Millisecond)
	}
	return d.Round(100 * time.Millisecond)
}
