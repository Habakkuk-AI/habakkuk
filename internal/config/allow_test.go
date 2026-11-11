package config

import (
	"strings"
	"testing"
)

func TestAllowRules(t *testing.T) {
	agent := func(allow string) string {
		return strings.Replace(mediaBase, "{model: p/m}", "{model: p/m, allow: "+allow+"}", 1)
	}
	good := `[{exact: git status}, {exact: "git log -1", in: ~/code/shop}, {exact: "make test"}, {tool: write_file, path: ~/reports}]`
	cfg, err := Parse([]byte(agent(good)))
	if err != nil {
		t.Fatal(err)
	}
	if rules := cfg.AllowRules("habakkuk"); len(rules) == 0 || rules[0].Source != "config" || rules[0].Place != "" {
		t.Fatalf("rules %+v", rules)
	}
	for _, bad := range []string{
		`[{command: "git log *"}]`, `[{command: git status}]`, // patterns are not supported
		`[{exact: git status, in: relative}]`,
		`[{exact: "make build && make test"}]`, `[{exact: "cat $HOME/x"}]`, `[{exact: "cd sub"}]`, `[{exact: "ls {a,b}"}]`, // not simple
		`[{tool: write_file, path: "~/"}]`, // too broad
		`[{tool: write_file, path: ~/x, exact: "git status"}]`,
		`[{}]`,
	} {
		if _, err := Parse([]byte(agent(bad))); err == nil || !strings.Contains(err.Error(), "allow entry") {
			t.Errorf("%s: want an allow entry error, got %v", bad, err)
		}
	}
}
