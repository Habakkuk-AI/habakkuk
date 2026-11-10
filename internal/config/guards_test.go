package config

import (
	"strings"
	"testing"
)

func TestGuardDefaults(t *testing.T) {
	cfg, err := Parse([]byte(mediaBase))
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Agents["habakkuk"]
	if g := a.LoopGuard; g.RepeatWarn != 3 || g.RepeatStop != 5 || g.IdleWarn != 8 || g.IdleStop != 12 {
		t.Fatalf("loop guard defaults: %+v", g)
	}
	if a.ProjectFiles != ProjectFilesOn {
		t.Fatalf("project_files default: %q", a.ProjectFiles)
	}
	cfg, err = Parse([]byte(strings.Replace(mediaBase, "{model: p/m}",
		"{model: p/m, loop_guard: {repeat_stop: 9, idle_warn: -1, idle_stop: -1}, project_files: trusted}", 1)))
	if err != nil {
		t.Fatal(err)
	}
	a = cfg.Agents["habakkuk"]
	if g := a.LoopGuard; g.RepeatWarn != 3 || g.RepeatStop != 9 || g.IdleWarn != -1 || g.IdleStop != -1 || a.ProjectFiles != "trusted" {
		t.Fatalf("explicit: %+v %q", g, a.ProjectFiles)
	}
}

func TestGuardValidation(t *testing.T) {
	for agent, want := range map[string]string{
		"{model: p/m, loop_guard: {repeat_stop: -2}}": "loop_guard.repeat_stop",
		"{model: p/m, project_files: maybe}":          "project_files",
	} {
		if _, err := Parse([]byte(strings.Replace(mediaBase, "{model: p/m}", agent, 1))); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", agent, want, err)
		}
	}
}
