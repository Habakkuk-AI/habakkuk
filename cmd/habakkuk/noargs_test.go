package main

import (
	"bytes"
	"strings"
	"testing"
)

// A command that takes no arguments prints its help for -h instead of
// running (hk init -h used to write a config, hk serve -h to start serving),
// and refuses anything else.
func TestNoArgCommandsAnswerHelp(t *testing.T) {
	for cmd := range takesNoArgs {
		for _, h := range []string{"-h", "--help", "help"} {
			var out, errOut bytes.Buffer
			if code := noArgsCmd(&out, &errOut, cmd, []string{h}); code != 0 {
				t.Errorf("hk %s %s: exit %d", cmd, h, code)
			}
			if !strings.HasPrefix(out.String(), "  hk "+cmd) {
				t.Errorf("hk %s %s printed %q", cmd, h, out.String())
			}
		}
		var out, errOut bytes.Buffer
		if code := noArgsCmd(&out, &errOut, cmd, []string{"extra"}); code != 2 || !strings.Contains(errOut.String(), "takes no arguments") {
			t.Errorf("hk %s extra: exit %d, stderr %q", cmd, code, errOut.String())
		}
	}
}

func TestCommandUsageKeepsContinuationLines(t *testing.T) {
	got := commandUsage("serve")
	if lines := strings.Count(got, "\n"); lines < 2 {
		t.Fatalf("serve usage has %d lines: %q", lines, got)
	}
	if strings.Contains(got, "hk channels") {
		t.Fatalf("serve usage ran into the next command: %q", got)
	}
}
