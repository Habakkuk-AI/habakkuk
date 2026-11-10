package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
)

const displayChannel = `
channels:
  # The family chat.
  tg:
    type: telegram
    token_env: TELEGRAM_BOT_TOKEN # from BotFather
    allow_from: ["7"]
`

// progress and approvals_detail are the two channel settings a person can
// flip while serve runs. A change, whether saved through the gateway (a
// chat command, the dashboard) or made by hand in config.yaml, applies from
// the next turn with no restart and no restart notice.
func TestDisplaySettingsReloadWithoutARestart(t *testing.T) {
	ctx := context.Background()
	a, home, logs := openTestApp(t, displayChannel)
	if a.Gateway == nil {
		t.Fatal("no gateway")
	}
	path := filepath.Join(home, "config.yaml")
	w := &watcher{a: a, seen: a.fingerprints()}
	if d := a.Gateway.Display("tg"); d.Progress != config.ProgressPlain || d.ApprovalsDetail != config.ApprovalsSummary {
		t.Fatalf("defaults = %+v, want plain and summary", d)
	}

	// Saved through the gateway: written to the file, comments kept, and
	// live at once.
	d, err := a.Gateway.ChangeDisplay("tg", "detailed", "")
	if err != nil {
		t.Fatal(err)
	}
	if d.Progress != config.ProgressDetailed || a.Gateway.Display("tg").Progress != config.ProgressDetailed {
		t.Fatalf("after the change: %+v", d)
	}
	data, _ := os.ReadFile(path)
	for _, keep := range []string{"# The family chat.", "# from BotFather", "progress: detailed"} {
		if !strings.Contains(string(data), keep) {
			t.Fatalf("config.yaml lost %q:\n%s", keep, data)
		}
	}
	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Fatal("no config.yaml.bak")
	}
	future := time.Now().Add(time.Minute)
	os.Chtimes(path, future, future)
	if changed := w.check(ctx); strings.Join(changed, ",") != "config" {
		t.Fatalf("changed = %v", changed)
	}
	if strings.Contains(logs.String(), "restart habakkuk serve to apply") {
		t.Fatalf("a display setting asked for a restart: %s", logs.String())
	}
	if d := a.Gateway.Display("tg"); d.Progress != config.ProgressDetailed {
		t.Fatalf("the watcher undid the change: %+v", d)
	}

	// Edited by hand: the watcher applies it.
	hand := strings.Replace(string(data), "progress: detailed", "progress: off\n    approvals_detail: full", 1)
	os.WriteFile(path, []byte(hand), 0o600)
	os.Chtimes(path, future.Add(time.Minute), future.Add(time.Minute))
	w.check(ctx)
	if d := a.Gateway.Display("tg"); d.Progress != config.ProgressOff || d.ApprovalsDetail != config.ApprovalsFull {
		t.Fatalf("a hand edit was not applied: %+v", d)
	}
	if !strings.Contains(logs.String(), "channel display settings reloaded") || strings.Contains(logs.String(), "restart habakkuk serve to apply") {
		t.Fatalf("log: %s", logs.String())
	}

	// Anything else on a channel still needs the restart, and says so.
	other := strings.Replace(hand, `allow_from: ["7"]`, `allow_from: ["7", "8"]`, 1)
	os.WriteFile(path, []byte(other), 0o600)
	os.Chtimes(path, future.Add(2*time.Minute), future.Add(2*time.Minute))
	w.check(ctx)
	if !strings.Contains(logs.String(), "sections=channels") {
		t.Fatalf("no restart notice for another channel setting: %s", logs.String())
	}

	// A bad value is refused and changes nothing.
	if _, err := a.Gateway.ChangeDisplay("tg", "loud", ""); err == nil {
		t.Fatal("a bad progress value was accepted")
	}
	if d := a.Gateway.Display("tg"); d.Progress != config.ProgressOff {
		t.Fatalf("a refused change was applied: %+v", d)
	}
}
