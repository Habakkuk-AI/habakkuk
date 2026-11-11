package skills

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"gopkg.in/yaml.v3"
)

// Requires is what a skill needs from the machine Habakkuk runs on, from the
// "requires" block of its frontmatter:
//
//	requires:
//	  bins: [gh]            # programs that must be on PATH
//	  env: [GITHUB_TOKEN]   # environment variables that must be set
//	  os: [darwin, linux]   # operating systems it works on (GOOS names)
//
// A skill whose requirements are not met is not offered to agents: it is
// left out of the prompt and /skills, cannot be loaded, and `hk skills`
// says what is missing.
type Requires struct {
	Bins names `yaml:"bins,omitempty"`
	Env  names `yaml:"env,omitempty"`
	OS   names `yaml:"os,omitempty"`
}

// names is a list of names that may be written as one word in YAML.
type names []string

func (w *names) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*w = strings.Fields(strings.ReplaceAll(n.Value, ",", " "))
		return nil
	}
	var list []string
	if err := n.Decode(&list); err != nil {
		return err
	}
	*w = list
	return nil
}

// missing lists the requirements this machine does not meet, in words.
func (l *Library) missing(r Requires) []string {
	var out []string
	if len(r.OS) > 0 && !slices.Contains(r.OS, runtime.GOOS) {
		out = append(out, "runs on "+strings.Join(r.OS, ", ")+" only")
	}
	for _, bin := range r.Bins {
		if !l.hasBin(bin) {
			out = append(out, "the program "+bin+" on PATH")
		}
	}
	for _, name := range r.Env {
		if !l.hasEnv(name) {
			out = append(out, name+" set in the environment or .env")
		}
	}
	return out
}

func (l *Library) hasBin(name string) bool {
	if l.LookPath != nil {
		return l.LookPath(name)
	}
	_, err := exec.LookPath(name)
	return err == nil
}

func (l *Library) hasEnv(name string) bool {
	if l.Env != nil {
		return l.Env(name)
	}
	return os.Getenv(name) != ""
}

// Entry is a skill and, when agents cannot use it right now, why.
type Entry struct {
	Skill
	// Off is where the skill was turned off: "config" (skills.disabled or
	// skills.bundled in config.yaml), "command" (hk skills disable), or "".
	Off string
	// Missing are the requirements this machine does not meet.
	Missing []string
	// Unapproved: installed from outside, ships scripts, not approved yet.
	Unapproved bool
}

// Usable reports whether agents are offered the skill.
func (e Entry) Usable() bool { return e.Off == "" && len(e.Missing) == 0 && !e.Unapproved }

// Reason says why the skill is not usable, with the way to change that.
func (e Entry) Reason() string {
	switch {
	case e.Off == "config":
		return "turned off in config.yaml (skills.disabled)"
	case e.Off != "":
		return "turned off (hk skills enable " + e.Name + " turns it back on)"
	case e.Unapproved:
		return "ships scripts and is not approved (hk skills approve " + e.Name + ")"
	case len(e.Missing) > 0:
		return "needs " + strings.Join(e.Missing, " and ")
	}
	return ""
}

// check fills in why a skill, found by precedence, cannot be used.
func (l *Library) check(s Skill, off map[string]string) Entry {
	e := Entry{Skill: s, Off: off[s.Name]}
	e.Unapproved = s.Shared && !s.Plugin && !s.Bundled && l.Disabled(s)
	e.Missing = l.missing(s.Requires)
	return e
}

// NoCommand says, for a skill whose name is already a command, why
// /NAME will not run it and what does; "" for any other name.
func (l *Library) NoCommand(name string) string {
	if l.CommandTaken == nil || !l.CommandTaken(name) {
		return ""
	}
	return fmt.Sprintf("/%s is one of Habakkuk's own commands, so this skill has no command of its own: run it with /skill %s, or give it another name", name, name)
}

// State kept by `hk skills disable` and `enable`, beside the lock file.
const stateFile = ".state.json"

type state struct {
	Off []string `json:"off"`
}

func (l *Library) readState() state {
	var st state
	if data, err := os.ReadFile(filepath.Join(l.SharedDir(), stateFile)); err == nil {
		_ = json.Unmarshal(data, &st)
	}
	return st
}

// off maps every skill name that is turned off to where: "config" for
// config.yaml, "command" for hk skills disable. The state file is read each
// time, so a change reaches a running `hk serve` with the next session.
func (l *Library) off() map[string]string {
	out := map[string]string{}
	for _, name := range l.readState().Off {
		out[name] = "command"
	}
	for _, name := range l.Off {
		out[name] = "config"
	}
	return out
}

// IsOff reports whether a skill name is turned off, and where ("config" or
// "command").
func (l *Library) IsOff(name string) (where string, off bool) {
	where, off = l.off()[name]
	return where, off
}

// SetOff turns a skill off, or back on, by name, for every agent and
// wherever the skill comes from. Nothing is removed. A name turned off in
// config.yaml stays off until it is taken out there.
func (l *Library) SetOff(name string, off bool) error {
	if err := ValidName(name); err != nil {
		return err
	}
	inConfig := fmt.Errorf("skill %s is turned off in config.yaml (skills.disabled); remove it there", name)
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.readState()
	has := slices.Contains(st.Off, name)
	switch {
	case off && !has:
		st.Off = append(st.Off, name)
		slices.Sort(st.Off)
	case !off && has:
		st.Off = slices.DeleteFunc(st.Off, func(n string) bool { return n == name })
	case !off && slices.Contains(l.Off, name):
		return inConfig
	default:
		return nil
	}
	if st.Off == nil {
		st.Off = []string{}
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(l.SharedDir(), 0o700); err != nil {
		return err
	}
	if err := store.WriteFileAtomic(filepath.Join(l.SharedDir(), stateFile), append(data, '\n'), 0o600); err != nil {
		return err
	}
	if !off && slices.Contains(l.Off, name) {
		return inConfig
	}
	return nil
}
