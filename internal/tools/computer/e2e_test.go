package computer

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// TestXvfbEndToEnd drives a real virtual desktop: Xvfb, xdotool and a
// screenshot command must be installed (Linux only), or it skips.
func TestXvfbEndToEnd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("virtual desktops need Linux")
	}
	for _, bin := range []string{"Xvfb", "xdotool"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is not installed", bin)
		}
	}
	if have(shotTools...) == "" {
		t.Skip("no screenshot command (import, scrot, ffmpeg) is installed")
	}
	home := t.TempDir()
	on := true
	cfg := config.Computer{Backend: "xvfb", Display: ":150", Screen: "1600x900", MaxActions: 50, Timeout: 20 * time.Second, Native: &on}
	m := New(cfg, Options{Home: home, Agents: []string{"habakkuk"}})
	defer m.Close()
	reg := tools.NewRegistry()
	RegisterTools(reg, m)
	blobs, err := store.NewBlobs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env := &tools.Env{Agent: "habakkuk", Mode: tools.ModeFull, Blobs: blobs, Taint: tools.NewTaint(false)}
	call := func(name string, input any) (llm.Block, []llm.Block) {
		t.Helper()
		raw, _ := json.Marshal(input)
		start := time.Now()
		res, media := reg.ExecuteMedia(context.Background(), env, llm.Block{Type: llm.ToolUse, ToolUseID: "c", ToolName: name, Input: raw})
		t.Logf("%s %s: %s", name, raw, time.Since(start).Round(time.Millisecond))
		if res.IsError {
			t.Fatalf("%s %s: %s", name, raw, res.Text)
		}
		return res, media
	}

	res, media := call("computer_screenshot", map[string]any{})
	if len(media) != 1 || !strings.Contains(res.Text, "Screenshot 1366x768 of a 1600x900 screen") {
		t.Fatalf("screenshot: %s (%d images)", res.Text, len(media))
	}
	// Screenshot (683, 384) is screen (800, 450).
	call(NativeTool, map[string]any{"action": "mouse_move", "coordinate": []int{683, 384}})
	if x, y, err := m.desks["habakkuk"].b.Cursor(context.Background()); err != nil || x != 800 || y != 450 {
		t.Fatalf("pointer at %d,%d (%v), want 800,450", x, y, err)
	}
	if res, _ := call(NativeTool, map[string]any{"action": "cursor_position"}); res.Text != "X=683, Y=384" {
		t.Fatalf("cursor_position: %s", res.Text)
	}
	call("computer_click", map[string]any{"x": 100, "y": 100})
	call(NativeTool, map[string]any{"action": "left_click", "coordinate": []int{50, 60}, "text": "shift"})
	call("computer_drag", map[string]any{"from_x": 10, "from_y": 10, "to_x": 200, "to_y": 150})
	call("computer_key", map[string]any{"combo": "ctrl+l"})
	call(NativeTool, map[string]any{"action": "hold_key", "text": "shift", "duration": 0.2})
	call("computer_type", map[string]any{"text": "hello -- world"})
	call("computer_scroll", map[string]any{"x": 300, "y": 300, "dy": 2, "dx": -1})
	if !env.Taint.Marked() {
		t.Fatal("screenshot did not taint the turn")
	}

	running := RunningDesktops(filepath.Join(home, "computer"))
	if len(running) != 1 || running[0].Agent != "habakkuk" || running[0].Display == "" {
		t.Fatalf("running desktops: %+v", running)
	}
	if v := View(cfg, home); !strings.Contains(v, "x11vnc -display "+running[0].Display) {
		t.Fatalf("view: %s", v)
	}
	// The display is private: without the cookie, clients are refused.
	probe := exec.Command("xdotool", "getdisplaygeometry")
	probe.Env = append(os.Environ(), "DISPLAY="+running[0].Display, "XAUTHORITY="+filepath.Join(t.TempDir(), "none"))
	if out, err := probe.CombinedOutput(); err == nil {
		t.Fatalf("a client without the cookie reached the display: %s", out)
	}

	m.Close()
	if left := RunningDesktops(filepath.Join(home, "computer")); len(left) != 0 {
		t.Fatalf("desktops left running: %+v", left)
	}
}
