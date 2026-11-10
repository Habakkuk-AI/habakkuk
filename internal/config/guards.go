package config

import "fmt"

// LoopGuard stops a turn that goes round in circles. Repeat counts one
// failing tool call made again and again (same tool, same arguments, same
// error); idle counts rounds in a row whose every call repeats an earlier
// call of the turn and gets the same result back. At *_warn the model is told
// plainly that it is repeating itself; at *_stop the turn ends with a message
// to the user. 0 takes the default, -1 turns that check off.
type LoopGuard struct {
	RepeatWarn int `yaml:"repeat_warn"` // default 3
	RepeatStop int `yaml:"repeat_stop"` // default 5
	IdleWarn   int `yaml:"idle_warn"`   // default 8
	IdleStop   int `yaml:"idle_stop"`   // default 12
}

// Loop guard defaults.
const (
	DefaultRepeatWarn = 3
	DefaultRepeatStop = 5
	DefaultIdleWarn   = 8
	DefaultIdleStop   = 12
)

// Project instruction files (project_files): on loads them, untrusted
// unless they sit in the agent's own workspace; trusted loads them as the
// owner's own instructions; off skips them.
const (
	ProjectFilesOn      = "on"
	ProjectFilesTrusted = "trusted"
	ProjectFilesOff     = "off"
)

// applyGuardDefaults fills the loop guard and project file defaults.
func (a *Agent) applyGuardDefaults() {
	if a.LoopGuard == nil {
		a.LoopGuard = &LoopGuard{}
	}
	g := a.LoopGuard
	for _, f := range []struct {
		v   *int
		def int
	}{{&g.RepeatWarn, DefaultRepeatWarn}, {&g.RepeatStop, DefaultRepeatStop}, {&g.IdleWarn, DefaultIdleWarn}, {&g.IdleStop, DefaultIdleStop}} {
		if *f.v == 0 {
			*f.v = f.def
		}
	}
	if a.ProjectFiles == "" {
		a.ProjectFiles = ProjectFilesOn
	}
}

func (a *Agent) validateGuards(name string) []error {
	var errs []error
	if g := a.LoopGuard; g != nil {
		for _, f := range []struct {
			key string
			v   int
		}{{"repeat_warn", g.RepeatWarn}, {"repeat_stop", g.RepeatStop}, {"idle_warn", g.IdleWarn}, {"idle_stop", g.IdleStop}} {
			if f.v < -1 {
				errs = append(errs, fmt.Errorf("agent %s: loop_guard.%s must be a positive number, or -1 to turn it off", name, f.key))
			}
		}
	}
	switch a.ProjectFiles {
	case "", ProjectFilesOn, ProjectFilesTrusted, ProjectFilesOff:
	default:
		errs = append(errs, fmt.Errorf("agent %s: project_files %q must be on, trusted or off", name, a.ProjectFiles))
	}
	return errs
}
