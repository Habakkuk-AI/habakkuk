package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

func TestApprovalsCLI(t *testing.T) {
	home := writeHome(t, "")
	var out bytes.Buffer
	if err := approvalsCmd(home, nil, &out); err != nil || !strings.Contains(out.String(), "No approval rules") {
		t.Fatalf("empty: %v %q", err, out.String())
	}

	// A rule from config.yaml and one saved from a chat.
	home = writeHome(t, "")
	cfgText := strings.Replace(readFile(t, filepath.Join(home, "config.yaml")), "    permission: workspace",
		"    allow:\n      - exact: git status\n    permission: workspace", 1)
	writeFile(t, filepath.Join(home, "config.yaml"), cfgText)
	cfg, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := ruleFile(cfg, "habakkuk").Save(tools.Rule{Tool: "exec", Exact: "git log -1", Source: "chat",
		Place: tools.ChatPlace("telegram", "100"), SavedBy: "7 (Sam)"})
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := approvalsCmd(home, []string{"list"}, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"habakkuk:", "config-1", "exec exactly `git status` in the workspace", saved.ID, "chat",
		"exec exactly `git log -1`", "applies in chat telegram:100, saved by 7 (Sam)",
		"applies everywhere rules apply", "Rules answer only for a message you typed just now"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("list lacks %q:\n%s", want, out.String())
		}
	}
	if err := approvalsCmd(home, []string{"rm", "config-1"}, &out); err == nil || !strings.Contains(err.Error(), "config.yaml") {
		t.Fatalf("rm config rule: %v", err)
	}
	out.Reset()
	if err := approvalsCmd(home, []string{"rm", saved.ID}, &out); err != nil || !strings.Contains(out.String(), "Removed "+saved.ID) {
		t.Fatalf("rm: %v %q", err, out.String())
	}
	if err := approvalsCmd(home, []string{"rm", saved.ID}, &out); err == nil {
		t.Fatal("removed a rule twice")
	}
	if err := approvalsCmd(home, []string{"list", "-agent", "nobody"}, &out); err == nil {
		t.Fatal("unknown agent accepted")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}
