package tools

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// Approval rules ("always allow"). A rule answers yes for an agent in
// place of a person, for one kind of call the person has already said
// yes to:
//
//   - a command: a program and its arguments ("git status"), where a final
//     "*" takes any further arguments ("npm run *"), run in a folder (the
//     workspace unless the rule names one);
//   - an exact command line, for commands with pipes, redirects or other
//     shell syntax a pattern cannot judge;
//   - a file tool (write_file, edit_file, apply_patch) writing inside a
//     folder.
//
// Rules come from the config (agents.<name>.allow) and from "Always allow"
// answers, which are saved in the agent's folder. A rule never answers:
//
//   - for a turn that has read outside content (it is tainted): the same
//     text that steered the agent could have written the command;
//   - for a guest's turn: only the owner's own requests are covered;
//   - for a call that always needs a person: a ConfirmTool's, or a write
//     to the agent's instruction files or to its saved rules;
//   - for a command on the hardline blocklist, which never runs at all.

// Rule is one "always allow".
type Rule struct {
	ID string `json:"id"`
	// Tool is exec, or the file tool the rule covers.
	Tool string `json:"tool"`
	// Command is a program and its arguments, as you would type them; a
	// final "*" takes any further arguments. Exact is a command line that
	// must match character for character. An exec rule has one of the two.
	Command string `json:"command,omitempty"`
	Exact   string `json:"exact,omitempty"`
	// In is the folder an exec rule's command must run in, or below; ""
	// is the agent's workspace.
	In string `json:"in,omitempty"`
	// Path is the folder a file tool's rule lets it write in, or below.
	Path string `json:"path,omitempty"`
	// Added is when the rule was saved; Source is where it came from
	// ("chat", "terminal", "config").
	Added  time.Time `json:"added,omitzero"`
	Source string    `json:"source,omitempty"`
}

// RuleTools are the file tools a rule can cover, besides exec.
var RuleTools = []string{"write_file", "edit_file", "apply_patch"}

// Validate checks that the rule says one clear thing.
func (r Rule) Validate() error {
	switch {
	case r.Tool == "exec":
		if (r.Command == "") == (r.Exact == "") {
			return errors.New("an exec rule needs exactly one of command or exact")
		}
		if r.Path != "" {
			return errors.New("path is for file tools; an exec rule takes in")
		}
		if r.Command != "" {
			if _, _, ok := parsePattern(r.Command); !ok {
				return fmt.Errorf("command %q is not a plain program and arguments (no pipes, redirects, variables, wildcards or ~; a final * takes any further arguments)", r.Command)
			}
		}
		if r.In != "" && !absOrHome(r.In) {
			return fmt.Errorf("in %q must be an absolute path or start with ~/", r.In)
		}
	case slices.Contains(RuleTools, r.Tool):
		if r.Command != "" || r.Exact != "" || r.In != "" {
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
	switch {
	case r.Exact != "":
		return fmt.Sprintf("exec exactly `%s` in %s", r.Exact, r.folder())
	case r.Command != "":
		cmd, more := r.Pattern()
		if more {
			return fmt.Sprintf("exec `%s` with any further arguments, in %s", cmd, r.folder())
		}
		return fmt.Sprintf("exec `%s` in %s", cmd, r.folder())
	default:
		return fmt.Sprintf("%s in %s", r.Tool, r.Path)
	}
}

// Pattern is an exec rule's command as a program and arguments, with more
// set when it takes any further arguments.
func (r Rule) Pattern() (cmd string, more bool) {
	words, more, ok := parsePattern(r.Command)
	if !ok {
		return r.Command, false
	}
	return joinWords(words), more
}

func (r Rule) folder() string {
	if r.In == "" {
		return "the workspace"
	}
	return r.In
}

// same reports whether two rules cover the same calls.
func (r Rule) same(o Rule) bool {
	return r.Tool == o.Tool && r.Command == o.Command && r.Exact == o.Exact &&
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

// ruleLocks serialize changes to each rules file: every turn gets its own
// RuleFile, and two turns may save at once.
var ruleLocks sync.Map // path -> *sync.Mutex

func (f *RuleFile) lock() func() {
	m, _ := ruleLocks.LoadOrStore(filepath.Clean(f.Path), &sync.Mutex{})
	m.(*sync.Mutex).Lock()
	return m.(*sync.Mutex).Unlock
}

// RulesFileName is the file in an agent's folder that holds its saved
// rules.
const RulesFileName = "approvals.json"

type ruleDoc struct {
	Version int    `json:"version"`
	Rules   []Rule `json:"rules"`
}

func (f *RuleFile) File() string { return f.Path }

// Saved reads the saved rules.
func (f *RuleFile) Saved() ([]Rule, error) {
	defer f.lock()()
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
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
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
		out[i].Source = "config"
	}
	saved, _ := f.Saved() // an unreadable file covers nothing: people are asked
	return append(out, saved...)
}

func (f *RuleFile) Save(r Rule) (Rule, error) {
	if err := r.Validate(); err != nil {
		return Rule{}, err
	}
	defer f.lock()()
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
	defer f.lock()()
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

// rulesApply reports whether saved rules may answer for this turn at all:
// never for a tainted turn or a guest's.
func rulesApply(env *Env) bool {
	if env.Rules == nil || env.Taint.Marked() {
		return false
	}
	return env.Policy == nil || !env.Policy.Guest
}

// coveredByRule returns the rule that answers for a call, if one does.
// Callers have already ruled out calls that always need a person.
func coveredByRule(env *Env, t Tool, call llm.Block) (Rule, bool) {
	if !rulesApply(env) {
		return Rule{}, false
	}
	return matchRule(env, t, call, env.Rules.Rules())
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
		cwd := env.Resolve(in.Cwd)
		words, simple := shellWords(in.Command)
		for _, r := range rules {
			if r.Tool != "exec" || !runsIn(env, r.In, cwd) {
				continue
			}
			if r.Exact != "" && strings.TrimSpace(r.Exact) == strings.TrimSpace(in.Command) {
				return r, true
			}
			if r.Command != "" && simple && matchWords(r.Command, words) {
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
		if !ok || guardedPath(env, abs) != "" || guardedPath(env, real) != "" ||
			underAny(protected, real) || underAny(protected, abs) || !inTrusted(roots, real) {
			return false
		}
	}
	return len(targets) > 0
}

// runsIn reports whether a command run in cwd is in the rule's folder.
func runsIn(env *Env, in, cwd string) bool {
	real, ok := realPath(cwd)
	if !ok {
		return false
	}
	if in == "" {
		ws, ok := realPath(env.Workspace)
		return ok && underAny([]string{ws}, real)
	}
	root, ok := realPath(expandHome(in))
	return ok && underAny([]string{root}, real)
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
// when none should be offered: the turn is tainted or a guest's, the call
// is not one a rule can cover, or the rule would not cover this very call.
func ProposeRule(env *Env, t Tool, call llm.Block) *Rule {
	if !rulesApply(env) {
		return nil
	}
	var r Rule
	switch call.ToolName {
	case "exec":
		in, err := decode[execInput](call.Input)
		cmd := strings.TrimSpace(in.Command)
		if err != nil || cmd == "" || mentionsGuarded(cmd) || Hardline(cmd) != nil {
			return nil
		}
		r = Rule{Tool: "exec"}
		if words, ok := shellWords(cmd); ok && !strings.Contains(words[0], "=") {
			r.Command = joinWords(words)
		} else {
			r.Exact = cmd
		}
		if in.Cwd != "" {
			cwd, ok := realPath(env.Resolve(in.Cwd))
			ws, wok := realPath(env.Workspace)
			if !ok {
				return nil
			}
			if !wok || !underAny([]string{ws}, cwd) {
				r.In = cwd
			}
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
		if broadFolder(dir) {
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
// agent is or what it may do unasked.

// InstructionFiles are the files that make up an agent's instructions.
var InstructionFiles = []string{"SOUL.md", "AGENTS.md", "IDENTITY.md", "USER.md"}

// guardedPath names what a path is when it is guarded, or "".
func guardedPath(env *Env, p string) string {
	base := filepath.Base(p)
	for _, name := range InstructionFiles {
		if strings.EqualFold(base, name) {
			return name
		}
	}
	if env.Rules != nil && env.Rules.File() != "" {
		f := filepath.Clean(env.Rules.File())
		if filepath.Clean(p) == f {
			return "the saved approval rules"
		}
		if real, ok := realPath(f); ok && filepath.Clean(p) == real {
			return "the saved approval rules"
		}
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
		abs := env.Resolve(target)
		name := guardedPath(env, abs)
		if name == "" {
			if real, ok := realPath(abs); ok {
				name = guardedPath(env, real)
			}
		}
		if name == "" {
			continue
		}
		if name == "the saved approval rules" {
			return "it changes the agent's saved approval rules, which always needs your approval"
		}
		return "it changes " + name + ", one of the agent's instruction files, which always needs your approval"
	}
	return ""
}

// mentionsGuarded reports whether a command names an instruction file; no
// rule covers such a command.
func mentionsGuarded(cmd string) bool {
	low := strings.ToLower(cmd)
	for _, name := range InstructionFiles {
		if strings.Contains(low, strings.ToLower(name)) {
			return true
		}
	}
	return strings.Contains(low, RulesFileName)
}

// Shell words. Patterns judge only plain commands: a program and its
// arguments. Anything a shell would expand or that joins commands (pipes,
// ;, &&, redirects, $ and backquotes, globs, ~, braces, comments) makes
// the command one no pattern covers, and so does quoting that does not
// close: when in doubt, a person is asked.

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
				cur.WriteByte(d)
			}
			if !closed {
				return nil, false
			}
		case c == '\\':
			if i+1 >= len(s) || s[i+1] == '\n' || s[i+1] == '\r' {
				return nil, false
			}
			i++
			cur.WriteByte(s[i])
			inWord = true
		case strings.IndexByte(unsafeChars, c) >= 0:
			return nil, false
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

// parsePattern splits a rule's command into its words; any is true when
// it ends in "*", which takes any further arguments.
func parsePattern(p string) (words []string, any bool, ok bool) {
	p = strings.TrimSpace(p)
	if rest, cut := strings.CutSuffix(p, " *"); cut {
		p, any = strings.TrimSpace(rest), true
	}
	words, ok = shellWords(p)
	return words, any, ok
}

// matchWords reports whether a command's words fit a rule's pattern.
func matchWords(pattern string, words []string) bool {
	want, any, ok := parsePattern(pattern)
	if !ok || len(words) < len(want) || (!any && len(words) != len(want)) {
		return false
	}
	return slices.Equal(words[:len(want)], want)
}

// joinWords writes words back as a command, quoting any word a shell
// would not take as it is.
func joinWords(words []string) string {
	out := make([]string, len(words))
	for i, w := range words {
		if w != "" && !strings.ContainsAny(w, unsafeChars+" \t'\"\\") {
			out[i] = w
			continue
		}
		out[i] = "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
	}
	return strings.Join(out, " ")
}
