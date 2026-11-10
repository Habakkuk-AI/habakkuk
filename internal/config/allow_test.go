package config

import (
	"runtime"
	"strings"
	"testing"
)

func TestAllowRules(t *testing.T) {
	agent := func(allow string) string {
		return strings.Replace(mediaBase, "{model: p/m}", "{model: p/m, allow: "+allow+"}", 1)
	}
	good := `[{command: git status}, {command: "git log *", in: ~/code/shop}, {exact: "make build && make test"}, {tool: write_file, path: ~/reports}]`
	if runtime.GOOS == "windows" {
		good = `[{exact: "make build && make test"}, {tool: write_file, path: ~/reports}]`
	}
	cfg, err := Parse([]byte(agent(good)))
	if err != nil {
		t.Fatal(err)
	}
	if rules := cfg.AllowRules("habakkuk"); len(rules) == 0 || rules[0].Source != "config" || rules[0].Place != "" {
		t.Fatalf("rules %+v", rules)
	}
	for _, bad := range []string{
		`[{command: "python3 *"}]`, `[{command: "bash *"}]`, `[{command: "npx *"}]`, // * on something that runs anything
		`[{command: "./run.sh"}]`,                      // a program by relative path
		`[{command: "git status | sh"}]`,               // shell syntax in a pattern
		`[{tool: write_file, path: "~/"}]`,             // too broad
		`[{command: git status, exact: "git status"}]`, // two things at once
		`[{}]`,
	} {
		if _, err := Parse([]byte(agent(bad))); err == nil || !strings.Contains(err.Error(), "allow entry") {
			t.Errorf("%s: want an allow entry error, got %v", bad, err)
		}
	}
}
