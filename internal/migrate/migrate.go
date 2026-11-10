// Package migrate moves a user's setup from another agent's home into a
// Habakkuk home: agents and their persona files, the user profile, memory
// notes, skills, scheduled tasks, MCP servers, model providers, channels
// and the keys they use. The supported import sources are Hermes Agent
// (hermes.go) and OpenClaw (openclaw.go).
//
// It works in three steps: Read turns the source home into a neutral
// description (Found), NewPlan compares that with the Habakkuk home and
// decides what each item needs (new, already there, conflict), and Apply
// carries the plan out. Nothing is written before Apply, existing agents,
// skills and notes are never replaced without Force, and running it twice
// changes nothing the second time.
//
// The imported text may name files in the source home (a script a schedule
// runs, a folder of reports). The plan lists them, and with Options.Files
// "copy" the import copies the user's own ones into the Habakkuk home and
// rewrites the text to name the copies (files.go, files_copy.go, refs.go).
package migrate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/config"
)

// The import sources.
const (
	Hermes     = "hermes"
	OpenClaw   = "openclaw"
	ClaudeCode = "claude-code"
	Codex      = "codex"
)

// Sources lists the import sources by their --from names: the agents
// first, then the coding tools.
var Sources = []string{Hermes, OpenClaw, ClaudeCode, Codex}

// IsCodingTool reports whether a source is a coding tool (Claude Code,
// Codex) rather than an agent. Its keys are never copied, and an agent
// found beside it is the likelier import.
func IsCodingTool(from string) bool { return from == ClaudeCode || from == Codex }

// Label is a source's name for people.
func Label(from string) string {
	switch from {
	case Hermes:
		return "Hermes Agent"
	case OpenClaw:
		return "OpenClaw"
	case ClaudeCode:
		return "Claude Code"
	case Codex:
		return "Codex"
	}
	return from
}

// Brings says in a few words what an import from a source brings over.
func Brings(from string) string {
	if IsCodingTool(from) {
		return "instructions, MCP servers, skills and memory"
	}
	return "agents, memory, conversations, skills, schedules and keys"
}

// Options say what to import and how.
type Options struct {
	From   string // Hermes or OpenClaw
	Source string // the source home; "" is the source's default home
	// Agent is the Habakkuk agent the source's main agent becomes (default
	// "habakkuk"); other agents keep their own names.
	Agent string
	// Force replaces agents, files, skills, notes, schedules and .env values
	// that already exist and differ.
	Force bool
	// Getenv reads the environment, for the source's own home overrides;
	// nil is os.Getenv.
	Getenv func(string) string
	// UserHome is the user's home directory; "" asks the OS.
	UserHome string
	Now      func() time.Time

	// Files says what happens to the user's own files in the source home
	// that the imported text names: FilesCopy copies them into the Habakkuk
	// home and rewrites the text to name the copies; FilesSkip, or "",
	// leaves them and the text as they are.
	Files string
	// FilesAsk plans a copy for a user who has not been asked yet; the
	// caller asks, and plans again with Files set when the answer is no.
	FilesAsk bool
	// FilesDir is where the copies go; "" is the "files" folder of the
	// Habakkuk home.
	FilesDir string
	// FilesMax is the largest file a copy takes: 0 is DefaultFilesMax,
	// below 0 is no limit.
	FilesMax int64
	// FreeSpace reports the room at a folder; nil asks the file system.
	FreeSpace func(dir string) (uint64, error)

	// History says what happens to past conversations: HistoryCopy, or
	// "", imports them as read-only sessions; HistorySkip leaves them.
	History string
	// Project is a project folder whose own instructions, MCP servers and
	// skills a coding tool's import adds; "" is none.
	Project string
}

func (o *Options) getenv(k string) string {
	if o.Getenv == nil {
		return os.Getenv(k)
	}
	return o.Getenv(k)
}

func (o *Options) userHome() string {
	if o.UserHome != "" {
		return o.UserHome
	}
	h, _ := os.UserHomeDir()
	return h
}

func (o *Options) now() time.Time {
	if o.Now == nil {
		return time.Now()
	}
	return o.Now()
}

func (o *Options) mainAgent() string {
	if o.Agent != "" {
		return o.Agent
	}
	return "habakkuk"
}

// DefaultSource is where a source keeps its home by default, honouring the
// source's own environment overrides.
func DefaultSource(from string, o Options) string {
	switch from {
	case Hermes:
		if d := o.getenv("HERMES_HOME"); d != "" {
			return expandHome(d, o.userHome())
		}
		return filepath.Join(o.userHome(), ".hermes")
	case OpenClaw:
		if d := o.getenv("OPENCLAW_STATE_DIR"); d != "" {
			return expandHome(d, o.userHome())
		}
		if p := o.getenv("OPENCLAW_PROFILE"); p != "" && p != "default" {
			return filepath.Join(o.userHome(), ".openclaw-"+p)
		}
		return filepath.Join(o.userHome(), ".openclaw")
	case ClaudeCode:
		if d := o.getenv("CLAUDE_CONFIG_DIR"); d != "" {
			return expandHome(d, o.userHome())
		}
		return filepath.Join(o.userHome(), ".claude")
	case Codex:
		if d := o.getenv("CODEX_HOME"); d != "" {
			return expandHome(d, o.userHome())
		}
		return filepath.Join(o.userHome(), ".codex")
	}
	return ""
}

// Present reports whether dir looks like a home of that source.
func Present(from, dir string) bool {
	var marks []string
	switch from {
	case Hermes:
		marks = []string{"config.yaml", "SOUL.md", "memories", "profiles"}
	case OpenClaw:
		marks = []string{"openclaw.json", "workspace"}
	case ClaudeCode:
		marks = []string{"CLAUDE.md", "skills", "commands", "settings.json"}
	case Codex:
		marks = []string{"config.toml", "AGENTS.md", "skills", "prompts", "memories"}
	}
	for _, m := range marks {
		if _, err := os.Stat(filepath.Join(dir, m)); err == nil {
			return true
		}
	}
	return false
}

// Detect returns the sources whose default home exists.
func Detect(o Options) []string {
	var out []string
	for _, from := range Sources {
		if Present(from, DefaultSource(from, o)) {
			out = append(out, from)
		}
	}
	return out
}

// Found is what a source home holds, already in Habakkuk's terms.
type Found struct {
	From      string
	Dir       string
	Agents    []*Agent
	Providers []*Provider
	MCP       []*MCPServer
	Channels  []*Channel
	Schedules []*Schedule
	// Secrets are the values to put in $HABAKKUK_HOME/.env, by Habakkuk
	// variable name. They are never printed.
	Secrets map[string]string
	// Missing are variables the imported config names but the source did
	// not hold a value for (they were set in the shell, say).
	Missing []string
	// Leftovers cannot carry over; each says what to do instead.
	Leftovers []Leftover
	// Skipped are things left out on purpose, for the report.
	Skipped []string
	// History are the past conversations, to import as read-only sessions.
	History []*Conversation
	// OtherEnv are the source .env's other variable names, not copied.
	OtherEnv []string

	// usedEnv holds every source variable some agent's import used, across
	// all the homes read (one per profile), so a key the main home copied
	// is not listed as left behind because a profile's .env also has it.
	usedEnv map[string]bool
	// nested are homes kept inside Dir (a profile's home), as slash paths
	// relative to it; a file reference into one names its own entries.
	nested []string
	// workspaces are the agents' workspace folders the import leaves where
	// they are: folder -> agent.
	workspaces map[string]string
	// temp are folders the read made (skills written from slash
	// commands); Close removes them.
	temp []string
}

// Close removes what reading the source left on disk: the skills it wrote
// from slash commands, in a temporary folder.
func (f *Found) Close() error {
	var errs []error
	for _, d := range f.temp {
		errs = append(errs, os.RemoveAll(d))
	}
	f.temp = nil
	return errors.Join(errs...)
}

// otherEnv offers the variables of one source .env that this home's import
// did not use; finish keeps those no other home used either.
func (f *Found) otherEnv(env map[string]string, used map[string]bool, ownPrefix string) {
	if f.usedEnv == nil {
		f.usedEnv = map[string]bool{}
	}
	for k := range used {
		if used[k] {
			f.usedEnv[k] = true
		}
	}
	for k := range env {
		if !used[k] && !strings.HasPrefix(k, ownPrefix) && !slices.Contains(f.OtherEnv, k) {
			f.OtherEnv = append(f.OtherEnv, k)
		}
	}
}

// Agent is one agent to create or fill.
type Agent struct {
	From      string // its name in the source
	Name      string // the Habakkuk agent
	Main      bool
	Model     string // "<provider>/<model>"; "" when none was found
	Fallbacks []string
	Workspace string
	MCP       []string          // MCP servers it uses, for its toolsets
	Files     map[string]string // SOUL.md, AGENTS.md: their text
	Identity  *agent.Identity
	Profile   string // the user profile, as "## Section" blocks
	Notes     []Note
	Skills    []Skill
}

// Note is one memory note.
type Note struct {
	Title string
	Body  string
	Tags  []string
}

// Skill is one SKILL.md folder to copy.
type Skill struct {
	Name   string
	Dir    string
	Shared bool // into $HABAKKUK_HOME/skills rather than the agent's own folder
}

// Provider is one model provider for config.yaml.
type Provider struct {
	Name    string
	Preset  string
	Type    string
	BaseURL string
	KeysEnv []string
	Auth    string // a subscription login: chatgpt or copilot
}

// MCPServer is one MCP server for config.yaml.
type MCPServer struct {
	Name    string
	Command string
	Args    []string
	Env     map[string]string // child variable -> variable holding its value
	URL     string
	Headers map[string]string // header -> variable or "Bearer ${VAR}"
	OAuth   bool
}

// Channel is one channel account for config.yaml.
type Channel struct {
	Name         string
	Type         string
	Agent        string
	TokenEnv     string
	AppTokenEnv  string
	BotTokensEnv []string
	AllowFrom    []string
	Account      string // signal: number; matrix: user id
	Homeserver   string
	APIURL       string
	Routes       map[string]string
	Link         bool // a linked-device session that must be linked again
}

// Schedule is one scheduled task, imported paused.
type Schedule struct {
	Agent    string
	Name     string
	Spec     string
	Prompt   string
	Command  string
	OnOutput string
	Workdir  string
	To       *Address
}

// Address is where a schedule delivers.
type Address struct {
	Account      string `json:"account"`
	Conversation string `json:"conversation"`
	Thread       string `json:"thread,omitempty"`
}

// Leftover is something that cannot carry over.
type Leftover struct {
	What    string
	Instead string
}

// Read reads a source home.
func Read(o Options) (*Found, error) {
	dir := o.Source
	if dir == "" {
		dir = DefaultSource(o.From, o)
	}
	dir, err := filepath.Abs(expandHome(dir, o.userHome()))
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("no %s home at %s (pass --source DIR)", Label(o.From), dir)
	case err != nil:
		return nil, err
	case !info.IsDir():
		return nil, fmt.Errorf("%s is not a folder", dir)
	}
	var f *Found
	switch o.From {
	case Hermes:
		f, err = readHermes(dir, o)
	case OpenClaw:
		f, err = readOpenClaw(dir, o)
	case ClaudeCode:
		f, err = readClaudeCode(dir, o)
	case Codex:
		f, err = readCodex(dir, o)
	default:
		return nil, fmt.Errorf("--from %q: want %s", o.From, strings.Join(Sources, " or "))
	}
	if err != nil {
		return nil, err
	}
	f.From, f.Dir = o.From, dir
	if IsCodingTool(o.From) {
		// Keys are never taken from a coding tool: each one its config
		// names is listed for the user to set.
		f.Secrets = nil
		for _, p := range f.Providers {
			if pr, ok := config.Presets[p.Preset]; ok && pr.KeyEnv != "" && !pr.KeyOptional {
				f.Missing = append(f.Missing, pr.KeyEnv)
			}
		}
	}
	switch {
	case IsCodingTool(o.From):
		f.leftover("past coding sessions", "they stay in "+dir+"; hk migrate does not bring them over yet")
	case o.History == HistorySkip:
		f.leftover("conversation history", "left in "+dir+" (--history skip); run hk migrate again without it to bring it over")
	default:
		f.readHistory()
	}
	f.finish()
	return f, nil
}

// finish drops what nothing uses and sorts the lists.
func (f *Found) finish() {
	used := map[string]bool{}
	for _, a := range f.Agents {
		for _, m := range append([]string{a.Model}, a.Fallbacks...) {
			if p, _, ok := strings.Cut(m, "/"); ok {
				used[p] = true
			}
		}
	}
	f.Providers = slices.DeleteFunc(f.Providers, func(p *Provider) bool { return !used[p.Name] })
	slices.SortFunc(f.Providers, func(a, b *Provider) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(f.MCP, func(a, b *MCPServer) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(f.Channels, func(a, b *Channel) int { return strings.Compare(a.Name, b.Name) })
	// Variables the config names but the source had no value for.
	need := map[string]bool{}
	for _, p := range f.Providers {
		for _, k := range p.KeysEnv {
			if !config.Presets[p.Preset].KeyOptional {
				need[k] = true
			}
		}
	}
	for _, c := range f.Channels {
		for _, k := range append([]string{c.TokenEnv, c.AppTokenEnv}, c.BotTokensEnv...) {
			if k != "" {
				need[k] = true
			}
		}
	}
	for _, m := range f.MCP {
		for _, v := range m.Env {
			need[v] = true
		}
		for _, v := range m.Headers {
			for _, k := range varRefs(v) {
				need[k] = true
			}
		}
	}
	for k := range need {
		if _, ok := f.Secrets[k]; !ok {
			f.Missing = append(f.Missing, k)
		}
	}
	slices.Sort(f.Missing)
	f.Missing = slices.Compact(f.Missing)
	f.OtherEnv = slices.DeleteFunc(f.OtherEnv, func(k string) bool {
		_, copied := f.Secrets[k]
		return f.usedEnv[k] || copied
	})
	slices.Sort(f.OtherEnv)
}

// leftover records something that cannot carry over, once.
func (f *Found) leftover(what, instead string) {
	l := Leftover{What: what, Instead: instead}
	if !slices.Contains(f.Leftovers, l) {
		f.Leftovers = append(f.Leftovers, l)
	}
}

func (f *Found) skip(format string, args ...any) {
	f.Skipped = append(f.Skipped, fmt.Sprintf(format, args...))
}

// secret records a value for a Habakkuk variable. An empty value is
// ignored; a second, different value for the same name keeps the first.
func (f *Found) secret(name, value string) {
	value = strings.TrimSpace(value)
	if name == "" || value == "" {
		return
	}
	if f.Secrets == nil {
		f.Secrets = map[string]string{}
	}
	if _, ok := f.Secrets[name]; !ok {
		f.Secrets[name] = value
	}
}

func (f *Found) provider(p *Provider) {
	for _, q := range f.Providers {
		if q.Name == p.Name {
			return
		}
	}
	f.Providers = append(f.Providers, p)
}

// mcp adds a server, sharing one with the same name and definition and
// renaming one that clashes; it returns the name used.
func (f *Found) mcp(m *MCPServer, owner string) string {
	for _, q := range f.MCP {
		if q.Name == m.Name {
			if sameMCP(q, m) {
				return q.Name
			}
			m.Name = cleanName(m.Name+"-"+owner, 32)
			return f.mcp(m, owner)
		}
	}
	f.MCP = append(f.MCP, m)
	return m.Name
}

func sameMCP(a, b *MCPServer) bool {
	return a.Command == b.Command && slices.Equal(a.Args, b.Args) && a.URL == b.URL && a.OAuth == b.OAuth &&
		fmt.Sprint(a.Env) == fmt.Sprint(b.Env) && fmt.Sprint(a.Headers) == fmt.Sprint(b.Headers)
}

var reNameJunk = regexp.MustCompile(`[^a-z0-9_-]+`)

// cleanName turns a source name into a Habakkuk name: lowercase letters,
// digits, "-" and "_", at most n characters.
func cleanName(s string, n int) string {
	s = strings.Trim(reNameJunk.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-"), "-_")
	if len(s) > n {
		s = strings.TrimRight(s[:n], "-_")
	}
	return s
}

// envSuffix turns a name into a variable-name suffix: "work bot" -> WORK_BOT.
func envSuffix(s string) string {
	return strings.Trim(regexp.MustCompile(`[^A-Z0-9]+`).ReplaceAllString(strings.ToUpper(s), "_"), "_")
}

func expandHome(p, home string) string {
	if p == "~" {
		return home
	}
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		return filepath.Join(home, rest)
	}
	return p
}

// readText reads a file as trimmed text; "" when it is missing.
func readText(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(strings.ReplaceAll(string(data), "\r\n", "\n")), nil
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
