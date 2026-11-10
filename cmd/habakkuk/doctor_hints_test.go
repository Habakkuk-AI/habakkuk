package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
)

func TestKeyFix(t *testing.T) {
	if got := keyFix("/h", []string{"ANTHROPIC_API_KEY"}); !strings.Contains(got, "ANTHROPIC_API_KEY=...") || !strings.Contains(got, "habakkuk setup model") {
		t.Fatalf("keyFix = %q", got)
	}
}

// A clean doctor run never sends someone to start a serve that is already
// there.
func TestDoctorAllGoodKnowsServeIsRunning(t *testing.T) {
	withChannels := &config.Config{Channels: map[string]*config.Channel{"tg": {Type: "telegram"}}}
	now := time.Now()
	for _, c := range []struct {
		name string
		hb   *heartbeat
		cfg  *config.Config
		want string
		not  string
	}{
		{"no serve, channels", nil, withChannels, "Next: habakkuk serve", "is running"},
		{"no serve, terminal only", nil, &config.Config{}, "Next: habakkuk chat", "serve"},
		{"running", &heartbeat{PID: os.Getpid(), Started: now.Add(-time.Hour), Beat: now}, withChannels,
			"habakkuk serve is running.", "Next:"},
		{"still starting", &heartbeat{PID: os.Getpid(), Started: now.Add(-5 * time.Second), Beat: now, Starting: "channels slack"}, withChannels,
			"habakkuk serve is running: it is still starting up", "Next:"},
		{"alive but quiet", &heartbeat{PID: os.Getpid(), Started: now.Add(-time.Hour), Beat: now.Add(-10 * time.Minute)}, withChannels,
			"habakkuk serve is running, but it has not reported for a while", "Next:"},
		{"exited", &heartbeat{PID: 1 << 22, Started: now.Add(-time.Hour), Beat: now.Add(-time.Minute)}, withChannels,
			"Next: habakkuk serve", "is running"},
	} {
		home := t.TempDir()
		if c.hb != nil {
			data, _ := json.Marshal(c.hb)
			if err := os.WriteFile(filepath.Join(home, heartbeatFile), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		var out bytes.Buffer
		doctorAllGood(&out, home, c.cfg)
		if got := out.String(); !strings.Contains(got, c.want) || strings.Contains(got, c.not) {
			t.Errorf("%s: %q; want it to say %q and not %q", c.name, got, c.want, c.not)
		}
	}
}
