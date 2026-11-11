package tools

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/habakkuk-ai/habakkuk/internal/core/platform"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// Approval rules ("always allow"). A rule answers yes in place of a
// person, for one call the person has already said yes to:
//
//   - an exec rule: one simple command (a program and plain words: no
//     shell syntax, nothing the shell would expand, no cd), exactly, run
//     in one folder exactly (the workspace unless the rule names another).
//     Quoting may differ ("echo 'a b'" is "echo "a b""), nothing else may.
//     Wildcard rules may come later, with a reviewed list of programs;
//   - a file tool (write_file, edit_file, apply_patch) writing inside a
//     folder.
//
// Rules come from the config (agents.<name>.allow), which apply wherever
// rules apply, and from "Always allow" answers, saved in the agent's folder,
// which apply only where they were given: in that chat, or on this machine
// (Policy.Place). They answer only for a turn the owner typed just now, in
// a direct chat or on this machine, untainted, with no chat notes in its
// message, in a session nothing tainted entered since the owner's previous
// message (agent.limitRules). A rule never answers:
//
//   - for any other turn: a schedule's run, a /goal follow-up, a board
//     card's run, a sub-agent's task, a background result or notice, an
//     API, MCP, editor or A2A request, a group's, a guest's or a webhook's
//     turn (Env.RulesOff, or Policy.Place "");
//   - for a turn that has read outside content (it is tainted), or whose
//     session or message took some in (Env.RulesPaused);
//   - for a call that always needs a person: a ConfirmTool's, or a write
//     to the agent's instruction files or to its saved rules, by name or
//     through a link, or a command in or into Habakkuk's own folders;
//   - for a command on the hardline blocklist, which never runs at all.

// Rule is one "always allow".
type Rule struct {
	ID string `json:"id"`
	// Tool is exec, or the file tool the rule covers.
	Tool string `json:"tool"`
	// Exact is the command line an exec rule allows.
	Exact string `json:"exact,omitempty"`
	// In is the folder an exec rule's command must run in, exactly, as its
	// real path (it is compared as stored); "" is the agent's workspace.
	In string `json:"in,omitempty"`
	// Program is where the command's program was when the rule was saved
	// (a resolved, absolute path); a call whose program resolves elsewhere
	// is not covered. "" compares the program as written.
	Program string `json:"program,omitempty"`
	// Path is the folder a file tool's rule lets it write in, or below.
	Path string `json:"path,omitempty"`
	// Place is where the rule applies: "" (a config rule) wherever rules
	// apply, PlaceLocal on this machine, or one ChatPlace.
	Place string `json:"place,omitempty"`
	// SavedBy names who answered "Always allow" ("7 (Sam)").
	SavedBy string `json:"saved_by,omitempty"`
	// Added is when the rule was saved; Source is where it came from
	// ("chat", "local", "config").
	Added  time.Time `json:"added,omitzero"`
	Source string    `json:"source,omitempty"`
}

// RuleTools are the file tools a rule can cover, besides exec.
var RuleTools = []string{"write_file", "edit_file", "apply_patch"}

// windowsHost is set on Windows, where commands on this machine run in
// PowerShell unless HABAKKUK_SHELL says otherwise.
var windowsHost = runtime.GOOS == "windows"

// shellName names the shell commands run in on this machine
// (platform.ScriptShellName); a variable so tests can stand in another.
var shellName = platform.ScriptShellName

// posixShell reports whether exec rules can be judged for this agent: its
// commands run on this machine (not in a Docker or SSH sandbox, whose
// programs this machine can't pin), on macOS or Linux, in bash, sh or dash
// (not zsh, which expands a leading =, nor fish, PowerShell or anything
// HABAKKUK_SHELL names). Elsewhere no exec rule is offered or applies;
// file rules are unaffected.
func posixShell(env *Env) bool {
	if env.Backend != nil || windowsHost {
		return false
	}
	switch shellName() {
	case "bash", "sh", "dash":
		return true
	}
	return false
}

// maxOffered is the longest command or folder "Always allow" is offered
// for: longer ones do not fit clearly in a chat prompt.
const maxOffered = 300

// Validate checks that the rule says one clear thing.
func (r Rule) Validate() error {
	switch {
	case r.Tool == "exec":
		if strings.TrimSpace(r.Exact) == "" {
			return errors.New("an exec rule needs exact: the whole command line")
		}
		if _, ok := simpleCommand(r.Exact); !ok {
			return fmt.Errorf("exact %q must be one simple command: a program and plain words, with no ; & | < > ( ) $ ` { } * ? [ ], no ~ except a leading ~/, and no cd", r.Exact)
		}
		if r.Path != "" {
			return errors.New("path is for file tools; an exec rule takes in")
		}
		if r.In != "" && !absOrHome(r.In) {
			return fmt.Errorf("in %q must be an absolute path or start with ~/", r.In)
		}
	case slices.Contains(RuleTools, r.Tool):
		if r.Exact != "" || r.In != "" || r.Program != "" {
			return fmt.Errorf("a %s rule takes only path", r.Tool)
		}
		if !absOrHome(r.Path) {
			return fmt.Errorf("path %q must be an absolute path or start with ~/", r.Path)
		}
		if broadFolder(expandHome(r.Path)) {
			return fmt.Errorf("path %q is too broad: name a folder, not / or your home folder", r.Path)
		}
	default:
		return fmt.Errorf("tool %q: a rule covers exec, write_file, edit_file or apply_patch", r.Tool)
	}
	return nil
}

// String says in a few words what the rule covers.
func (r Rule) String() string {
	if r.Tool == "exec" {
		return fmt.Sprintf("exec exactly `%s` in %s", r.Exact, r.folder())
	}
	return fmt.Sprintf("%s in %s", r.Tool, r.Path)
}

// Where says where the rule applies.
func (r Rule) Where() string {
	switch {
	case r.Place == "":
		return "everywhere rules apply"
	case r.Place == PlaceLocal:
		return "on this machine"
	}
	return "in chat " + strings.TrimPrefix(r.Place, "chat:")
}

func (r Rule) folder() string {
	if r.In == "" {
		return "the workspace"
	}
	return r.In
}

// same reports whether two rules cover the same calls in the same place.
func (r Rule) same(o Rule) bool {
	return r.Tool == o.Tool && r.Exact == o.Exact && r.Program == o.Program && r.Place == o.Place &&
		filepath.Clean(expandHome(r.In)) == filepath.Clean(expandHome(o.In)) &&
		filepath.Clean(expandHome(r.Path)) == filepath.Clean(expandHome(o.Path))
}

// Note is what the transcript says when the rule allowed a call.
func (r Rule) Note() string {
	return "[allowed by a saved approval rule (" + r.ID + "): " + r.String() + "]"
}

// RuleSet is an agent's approval rules.
type RuleSet interface {
	// Rules lists the rules: the config's, then the saved ones.
	Rules() []Rule
	// Save stores a rule and returns it with its ID; a rule that is
	// already there is returned as it is.
	Save(r Rule) (Rule, error)
	// File is where the saved rules are kept. Writing it always needs a
	// person.
	File() string
}

// RuleFile is a RuleSet kept in a JSON file, plus the rules from the
// config. It reads the file on every call, so `hk approvals rm` takes
// effect in a running server.
type RuleFile struct {
	Path   string
	Config []Rule
}

// RulesFileName is the file in an agent's folder that holds its saved
// rules.
const RulesFileName = "approvals.json"

type ruleDoc struct {
	Version int    `json:"version"`
	Rules   []Rule `json:"rules"`
}

// ruleLocks serialize changes to each rules file within this process;
// lock takes the file lock too, so `hk approvals rm` and a running server
// never undo each other's change.
var ruleLocks sync.Map // path -> *sync.Mutex

func (f *RuleFile) lock() (func(), error) {
	m, _ := ruleLocks.LoadOrStore(filepath.Clean(f.Path), &sync.Mutex{})
	m.(*sync.Mutex).Lock()
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		m.(*sync.Mutex).Unlock()
		return nil, err
	}
	lf, err := os.OpenFile(f.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		m.(*sync.Mutex).Unlock()
		return nil, err
	}
	if err := platform.Lock(lf); err != nil {
		lf.Close()
		m.(*sync.Mutex).Unlock()
		return nil, err
	}
	return func() {
		_ = platform.Unlock(lf)
		lf.Close()
		m.(*sync.Mutex).Unlock()
	}, nil
}

func (f *RuleFile) File() string { return f.Path }

// Saved reads the saved rules.
func (f *RuleFile) Saved() ([]Rule, error) {
	return f.read()
}

func (f *RuleFile) read() ([]Rule, error) {
	data, err := os.ReadFile(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc ruleDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", f.Path, err)
	}
	out := doc.Rules[:0]
	for _, r := range doc.Rules {
		if r.Place == "" {
			r.Place = PlaceLocal // only config rules apply everywhere
		}
		if r.Validate() == nil { // a hand edit that broke a rule drops it, not the rest
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *RuleFile) write(rules []Rule) error {
	data, err := json.MarshalIndent(ruleDoc{Version: 1, Rules: rules}, "", "  ")
	if err != nil {
		return err
	}
	return store.WriteFileAtomic(f.Path, append(data, '\n'), 0o600)
}

func (f *RuleFile) Rules() []Rule {
	out := slices.Clone(f.Config)
	for i := range out {
		if out[i].ID == "" {
			out[i].ID = fmt.Sprintf("config-%d", i+1)
		}
		out[i].Source, out[i].Place = "config", ""
	}
	saved, _ := f.Saved() // an unreadable file covers nothing: people are asked
	return append(out, saved...)
}

func (f *RuleFile) Save(r Rule) (Rule, error) {
	if r.Place == "" {
		r.Place = PlaceLocal
	}
	if err := r.Validate(); err != nil {
		return Rule{}, err
	}
	unlock, err := f.lock()
	if err != nil {
		return Rule{}, err
	}
	defer unlock()
	rules, err := f.read()
	if err != nil {
		return Rule{}, err
	}
	for _, old := range rules {
		if old.same(r) {
			return old, nil
		}
	}
	r.ID = newRuleID()
	if r.Added.IsZero() {
		r.Added = time.Now().UTC().Truncate(time.Second)
	}
	if err := f.write(append(rules, r)); err != nil {
		return Rule{}, err
	}
	return r, nil
}

// Remove deletes a saved rule by ID; it reports whether there was one.
func (f *RuleFile) Remove(id string) (bool, error) {
	unlock, err := f.lock()
	if err != nil {
		return false, err
	}
	defer unlock()
	rules, err := f.read()
	if err != nil {
		return false, err
	}
	i := slices.IndexFunc(rules, func(r Rule) bool { return r.ID == id })
	if i < 0 {
		return false, nil
	}
	return true, f.write(slices.Delete(rules, i, i+1))
}

func newRuleID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "r" + hex.EncodeToString(b)
}

// turnPlace is where the turn stands for rules (Policy.Place).
func turnPlace(env *Env) string {
	if env.Policy == nil {
		return PlaceLocal
	}
	return env.Policy.Place
}

// rulesApply reports whether saved rules may answer for this turn at all:
// never for a tainted turn, one whose rules are paused, or one that is not
// the owner's.
func rulesApply(env *Env) bool {
	if env.Rules == nil || env.RulesOff || env.Untrusted() || turnPlace(env) == "" {
		return false
	}
	return env.Policy == nil || !env.Policy.Guest
}

// coveredByRule returns the rule that answers for a call, if one does:
// one from the config, or one saved where this turn is. Callers have
// already ruled out calls that always need a person.
func coveredByRule(env *Env, t Tool, call llm.Block) (Rule, bool) {
	if !rulesApply(env) {
		return Rule{}, false
	}
	place := turnPlace(env)
	var here []Rule
	for _, r := range env.Rules.Rules() {
		if r.Place == "" || r.Place == place {
			here = append(here, r)
		}
	}
	return matchRule(env, t, call, here)
}

// matchRule returns the first of rules that covers the call.
func matchRule(env *Env, t Tool, call llm.Block, rules []Rule) (Rule, bool) {
	if hardlineCheck(t, call.Input) != nil {
		return Rule{}, false
	}
	switch call.ToolName {
	case "exec":
		in, err := decode[execInput](call.Input)
		if err != nil || strings.TrimSpace(in.Command) == "" || mentionsGuarded(in.Command) {
			return Rule{}, false
		}
		words, simple := simpleCommand(in.Command)
		cwd, ok := realPath(env.Resolve(in.Cwd))
		if !posixShell(env) || !simple || !ok || inProtected(env, cwd) || namesGuarded(env, words, cwd) {
			return Rule{}, false
		}
		for _, r := range rules {
			if r.Tool == "exec" && runsIn(env, r, cwd) && sameLine(r.Exact, in.Command) && sameProgram(env, r, in.Command, cwd) {
				return r, true
			}
		}
	case "write_file", "edit_file", "apply_patch":
		targets, ok := fileTargets(t, call.Input)
		if !ok || env.Files != nil || env.remote() {
			return Rule{}, false
		}
		var protected []string
		if env.Policy != nil {
			protected = realPaths(env.Policy.Protected)
		}
		for _, r := range rules {
			if r.Tool != call.ToolName {
				continue
			}
			root, ok := realPath(expandHome(r.Path))
			if !ok || broadFolder(root) {
				continue
			}
			if allUnder(env, targets, []string{root}, protected) {
				return r, true
			}
		}
	}
	return Rule{}, false
}

// allUnder reports whether every target lands, after symlinks, inside one
// of roots, under no hidden folder and no protected one, and is not a
// guarded file.
func allUnder(env *Env, targets, roots, protected []string) bool {
	for _, target := range targets {
		abs := env.Resolve(target)
		real, ok := realPath(abs)
		if !ok || guardedTarget(env, abs) != "" || underAny(protected, real) || underAny(protected, abs) ||
			inProtected(env, real) || inProtected(env, abs) || !inTrusted(roots, real) {
			return false
		}
	}
	return len(targets) > 0
}

// runsIn reports whether a command runs (in cwd, resolved) in the rule's
// folder exactly: the same words in another folder mean other files. A
// rule's folder is compared as stored (its real path when it was saved),
// so a link made there later does not move it.
func runsIn(env *Env, r Rule, cwd string) bool {
	if r.In != "" {
		return filepath.Clean(expandHome(r.In)) == cwd
	}
	ws, ok := realPath(env.Workspace)
	return ok && ws == cwd
}

// sameLine reports whether a command is the rule's line: the same words,
// however they are quoted, when both are plain commands a POSIX shell
// would split the same way; otherwise the same text.
func sameLine(rule, cmd string) bool {
	rule, cmd = strings.TrimSpace(rule), strings.TrimSpace(cmd)
	if rule == cmd {
		return true
	}
	a, aok := shellWords(rule)
	b, bok := shellWords(cmd)
	return aok && bok && slices.Equal(a, b)
}

// sameProgram reports whether a command's program is the one the rule was
// saved for: the same resolved file, when the rule recorded it.
func sameProgram(env *Env, r Rule, cmd, cwd string) bool {
	if r.Program == "" {
		return true
	}
	words, ok := simpleCommand(cmd)
	if !ok {
		return false
	}
	got := resolveProgram(env, words[0], cwd)
	return got != "" && got == r.Program
}

// resolveProgram is where a program runs from: a path is resolved against
// cwd, a bare name is looked up in PATH. "" when it cannot be said (not
// found, or the command runs on another host).
func resolveProgram(env *Env, program, cwd string) string {
	if env.remote() {
		return ""
	}
	p := expandHome(program)
	if strings.ContainsAny(p, `/\`) {
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
	} else {
		found, err := exec.LookPath(program)
		if err != nil {
			return ""
		}
		p = found
	}
	if !filepath.IsAbs(p) {
		var err error
		if p, err = filepath.Abs(p); err != nil {
			return ""
		}
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return ""
	}
	return real
}

// simpleCommand splits a command into its words when it is a simple one:
// a program and plain words, nothing a shell would expand or join, and no
// cd, pushd or popd, which would change the folder the words name paths
// in. Only such commands get rules.
func simpleCommand(cmd string) ([]string, bool) {
	words, ok := shellWords(cmd)
	if !ok {
		return nil, false
	}
	switch strings.ToLower(filepath.Base(words[0])) {
	case "cd", "pushd", "popd":
		return nil, false
	}
	return words, true
}

// inProtected reports whether a real path is in one of Habakkuk's own
// folders (Policy.Protected).
func inProtected(env *Env, p string) bool {
	if env.Policy == nil {
		return false
	}
	dirs := env.Policy.Protected
	if env.Policy.Home != "" {
		dirs = append(slices.Clone(dirs), env.Policy.Home) // config.yaml, .env and the databases live there
	}
	return underAny(realPaths(dirs), p) || underAny(dirs, p)
}

// namesGuarded reports whether a simple command's words name a guarded
// file (an instruction file, the rules file or its lock, a file with a
// second name, followed through symlinks) or anything in Habakkuk's own
// folders. Each word is read as a path, and so is what follows "=" and
// what follows a short option ("-onotes.md"). A path that does not exist
// yet is judged by its nearest existing folder. A program that picks its
// own files is beyond this; that is why exec rules are exact lines.
func namesGuarded(env *Env, words []string, cwd string) bool {
	check := func(p string) bool {
		if _, err := os.Lstat(p); err == nil {
			if guardedTarget(env, p) != "" {
				return true
			}
			real, ok := realPath(p)
			return !ok || inProtected(env, real) || inProtected(env, p)
		}
		for dir := filepath.Dir(p); ; dir = filepath.Dir(dir) {
			if _, err := os.Lstat(dir); err == nil {
				real, ok := realPath(dir)
				return !ok || inProtected(env, real)
			}
			if filepath.Dir(dir) == dir {
				return false
			}
		}
	}
	for _, w := range words {
		cands := []string{w}
		if _, after, ok := strings.Cut(w, "="); ok {
			cands = append(cands, after)
		}
		if len(w) > 2 && w[0] == '-' && w[1] != '-' {
			cands = append(cands, w[2:]) // -onotes.md
		}
		for _, c := range cands {
			if c == "" || c == "-" || strings.HasPrefix(c, "-") && c == w {
				continue
			}
			c = expandHome(c)
			if !filepath.IsAbs(c) {
				c = filepath.Join(cwd, c)
			}
			if check(filepath.Clean(c)) {
				return true
			}
		}
	}
	return false
}

// fileTargets lists the paths a file tool's call writes.
func fileTargets(t Tool, input json.RawMessage) ([]string, bool) {
	if pt, ok := t.(PathTool); ok {
		p, err := pt.TargetPath(input)
		return []string{p}, err == nil && p != ""
	}
	if pt, ok := t.(PathsTool); ok {
		ps, err := pt.TargetPaths(input)
		return ps, err == nil && len(ps) > 0
	}
	return nil, false
}

// ProposeRule is the rule "Always allow" would save for a call, or nil
// when none should be offered: the turn is tainted or not the owner's,
// the call is not one a rule can cover, its command is too long or holds a
// backquote or a line break (it would not show plainly in a chat), or the
// rule would not cover this very call. The caller sets where it applies.
func ProposeRule(env *Env, t Tool, call llm.Block) *Rule {
	if !rulesApply(env) {
		return nil
	}
	var r Rule
	switch call.ToolName {
	case "exec":
		in, err := decode[execInput](call.Input)
		cmd := strings.TrimSpace(in.Command)
		if err != nil || !showsPlainly(cmd) || mentionsGuarded(cmd) || Hardline(cmd) != nil {
			return nil
		}
		words, simple := simpleCommand(cmd)
		cwd, ok := realPath(env.Resolve(in.Cwd))
		if !simple || !ok || !showsPlainly(cwd) || inProtected(env, cwd) {
			return nil
		}
		if !posixShell(env) {
			return nil
		}
		// The program is pinned to the file it is now, or no rule is offered.
		r = Rule{Tool: "exec", Exact: cmd, In: cwd, Program: resolveProgram(env, words[0], cwd)}
		if r.Program == "" {
			return nil
		}
	case "write_file", "edit_file", "apply_patch":
		targets, ok := fileTargets(t, call.Input)
		if !ok {
			return nil
		}
		var dirs []string
		for _, target := range targets {
			real, ok := realPath(env.Resolve(target))
			if !ok {
				return nil
			}
			dirs = append(dirs, filepath.Dir(real))
		}
		dir := commonDir(dirs)
		if broadFolder(dir) || !showsPlainly(dir) {
			return nil
		}
		r = Rule{Tool: call.ToolName, Path: dir}
	default:
		return nil
	}
	if r.Validate() != nil {
		return nil
	}
	if _, ok := matchRule(env, t, call, []Rule{r}); !ok {
		return nil
	}
	return &r
}

// showsPlainly reports whether text can be shown whole and as it is in a
// chat: no backquote or line break, and short enough to read.
func showsPlainly(text string) bool {
	return text != "" && len(text) <= maxOffered && !strings.ContainsAny(text, "`\n\r")
}

// commonDir is the deepest folder that holds every one of dirs.
func commonDir(dirs []string) string {
	if len(dirs) == 0 {
		return string(filepath.Separator)
	}
	out := dirs[0]
	for _, d := range dirs[1:] {
		for !underAny([]string{out}, d) {
			parent := filepath.Dir(out)
			if parent == out {
				return out
			}
			out = parent
		}
	}
	return out
}

// broadFolder reports whether dir is a filesystem root, the home folder or
// a folder that holds it: no rule covers that much.
func broadFolder(dir string) bool {
	dir = filepath.Clean(dir)
	if filepath.Dir(dir) == dir {
		return true
	}
	h := homeDir()
	return h != "" && underAny([]string{dir}, filepath.Clean(h))
}

func absOrHome(p string) bool {
	return strings.HasPrefix(p, "~/") || filepath.IsAbs(p)
}

func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		return filepath.Join(homeDir(), rest)
	}
	return p
}

// Guarded files: the agent's instruction files, and its saved approval
// rules. A write to one always needs a person, in every mode, and no rule
// covers it: a page the agent read must not be able to rewrite who the
// agent is or what it may do unasked. Names are compared without regard to
// case (macOS and Windows disks ignore it), and a file with more than one
// name (a hard link) is guarded too, since one of its names may be one of
// these.

// InstructionFiles are the files that make up an agent's instructions.
var InstructionFiles = []string{"SOUL.md", "AGENTS.md", "IDENTITY.md", "USER.md"}

const rulesGuard = "the saved approval rules"

// guardedTarget names what a path is when a write to it is guarded, or "".
// It looks at the path as written and where it really lands.
func guardedTarget(env *Env, abs string) string {
	if name := guardedPath(env, abs); name != "" {
		return name
	}
	if real, ok := realPath(abs); ok {
		if name := guardedPath(env, real); name != "" {
			return name
		}
	}
	if fi, err := os.Stat(abs); err == nil && fi.Mode().IsRegular() && platform.Links(fi) > 1 {
		return "a file with more than one name (a hard link)"
	}
	return ""
}

// guardedPath names what a path is when it is guarded, or "".
func guardedPath(env *Env, p string) string {
	base := filepath.Base(p)
	for _, name := range InstructionFiles {
		if strings.EqualFold(base, name) {
			return name
		}
	}
	if strings.HasPrefix(fold(base), fold(RulesFileName)) {
		return rulesGuard // approvals.json, its lock, a copy of either
	}
	if name := homeFile(env, p); name != "" {
		return name
	}
	if env.Rules == nil || env.Rules.File() == "" {
		return ""
	}
	f := filepath.Clean(env.Rules.File())
	for _, rf := range []string{f, f + ".lock"} {
		if strings.EqualFold(filepath.Clean(p), rf) {
			return rulesGuard
		}
		if real, ok := realPath(rf); ok && strings.EqualFold(filepath.Clean(p), real) {
			return rulesGuard
		}
		a, aerr := os.Stat(p)
		b, berr := os.Stat(rf)
		if aerr == nil && berr == nil && os.SameFile(a, b) {
			return rulesGuard
		}
	}
	return ""
}

// homeFile names one of Habakkuk's own files at the root of its home when p
// is one (config.yaml, .env, a database), or "".
func homeFile(env *Env, p string) string {
	if env.Policy == nil || env.Policy.Home == "" {
		return ""
	}
	dir := filepath.Dir(filepath.Clean(p))
	home := filepath.Clean(env.Policy.Home)
	same := strings.EqualFold(dir, home)
	if !same {
		if real, ok := realPath(home); ok {
			same = strings.EqualFold(dir, real)
		}
	}
	if !same {
		return ""
	}
	base := strings.ToLower(filepath.Base(p))
	switch {
	case strings.HasPrefix(base, "config.yaml"), strings.HasPrefix(base, ".env"):
		return "Habakkuk's " + base
	case strings.HasSuffix(base, ".db"), strings.Contains(base, ".db-"):
		return "Habakkuk's " + base + " database"
	}
	return ""
}

// guardedWrite is the reason a file tool's call needs a person whatever
// the mode, or "".
func guardedWrite(env *Env, t Tool, call llm.Block) string {
	switch call.ToolName {
	case "write_file", "edit_file", "apply_patch":
	default:
		return ""
	}
	targets, _ := fileTargets(t, call.Input)
	for _, target := range targets {
		switch name := guardedTarget(env, env.Resolve(target)); {
		case name == "":
			continue
		case name == rulesGuard:
			return "it changes the agent's saved approval rules, which always needs your approval"
		case strings.HasPrefix(name, "a file"), strings.HasPrefix(name, "Habakkuk's"):
			return "it changes " + name + ", which always needs your approval"
		default:
			return "it changes " + name + ", one of the agent's instruction files, which always needs your approval"
		}
	}
	return ""
}

// mentionsGuarded reports whether a command names an instruction file or
// the rules file, however it is quoted, escaped or cased (Unicode case
// folding included: "ſOUL.md" is SOUL.md on a disk that ignores case); no
// rule covers such a command.
func mentionsGuarded(cmd string) bool {
	plain := fold(strings.NewReplacer(`'`, "", `"`, "", `\`, "").Replace(cmd))
	for _, name := range append(slices.Clone(InstructionFiles), RulesFileName) {
		if strings.Contains(plain, fold(name)) {
			return true
		}
	}
	return false
}

// fold maps each letter to one of its case forms, the same for all of
// them, so two strings that differ only in case fold to the same text.
func fold(s string) string {
	return strings.Map(func(r rune) rune {
		least := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			least = min(least, f)
		}
		return least
	}, s)
}

// Shell words. Two command lines are the same when a POSIX shell would
// split both into the same words: a program and its arguments, quoted
// either way. Anything a shell would expand or that joins commands (pipes,
// ;, &&, redirects, $ and backquotes, globs, ~, braces, comments), and
// quoting that does not close, is compared as text instead.

// unsafeChars are the characters that, unquoted, make a command more than
// a program and its arguments.
const unsafeChars = ";&|<>()$`\n\r*?[]{}~#!%"

// shellWords splits a plain command into its words; ok is false for
// anything else.
func shellWords(s string) ([]string, bool) {
	var words []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, false
			}
			if cur.Len() == 0 && j > 0 && strings.IndexByte("~=", s[i+1]) >= 0 {
				return nil, false // a quoted ~ is not the home folder: the same words would name another file
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
			inWord = true
		case c == '"':
			inWord = true
			closed := false
			for i++; i < len(s); i++ {
				d := s[i]
				if d == '"' {
					closed = true
					break
				}
				switch d {
				case '$', '`', '!':
					return nil, false
				case '\\':
					if i+1 < len(s) && strings.IndexByte("\"\\", s[i+1]) >= 0 {
						i++
						d = s[i]
					} else if i+1 < len(s) && strings.IndexByte("$`\n", s[i+1]) >= 0 {
						return nil, false
					}
				}
				if cur.Len() == 0 && (d == '~' || d == '=') {
					return nil, false
				}
				cur.WriteByte(d)
			}
			if !closed {
				return nil, false
			}
		case c == '~' && !inWord && i+1 < len(s) && s[i+1] == '/':
			cur.WriteByte(c) // a leading ~/ is the home folder
			inWord = true
		case c == '\\':
			if i+1 >= len(s) || s[i+1] == '\n' || s[i+1] == '\r' {
				return nil, false
			}
			i++
			if cur.Len() == 0 && (s[i] == '~' || s[i] == '=') {
				return nil, false
			}
			cur.WriteByte(s[i])
			inWord = true
		case strings.IndexByte(unsafeChars, c) >= 0:
			return nil, false
		case c == '=' && cur.Len() == 0:
			return nil, false // zsh expands a leading =, and a quoted ~ or = after empty quotes is still first
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	if len(words) == 0 || strings.Contains(words[0], "=") {
		return nil, false // nothing to run, or a variable set for the command
	}
	return words, true
}
