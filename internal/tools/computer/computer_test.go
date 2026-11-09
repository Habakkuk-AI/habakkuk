package computer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// fake is a backend with a screen of pixW×pixH pixels and an input space
// of inW×inH (0 for the same as the pixels).
type fake struct {
	mu         sync.Mutex
	pixW, pixH int
	inW, inH   int
	actions    []action
	front      string
	frontErr   error
	shots      int
}

func (f *fake) Name() string                { return "fake" }
func (f *fake) Start(context.Context) error { return nil }
func (f *fake) Close()                      {}
func (f *fake) InputSize(context.Context) (int, int, error) {
	return f.inW, f.inH, nil
}

func (f *fake) Screenshot(context.Context) ([]byte, error) {
	f.mu.Lock()
	f.shots++
	f.mu.Unlock()
	img := image.NewRGBA(image.Rect(0, 0, f.pixW, f.pixH))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = uint8(i/4), uint8(i/4/f.pixW), 128, 255
	}
	var buf bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (f *fake) Do(_ context.Context, a action) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions = append(f.actions, a)
	return nil
}

func (f *fake) Cursor(context.Context) (int, int, error) { return 500, 250, nil }

func (f *fake) Front(context.Context) (string, string, error) {
	return f.front, "Vault - " + f.front, f.frontErr
}

type harness struct {
	t   *testing.T
	m   *Manager
	f   *fake
	reg *tools.Registry
	env *tools.Env
}

func newHarness(t *testing.T, cfg config.Computer, f *fake) *harness {
	t.Helper()
	if cfg.MaxActions == 0 {
		cfg.MaxActions = 100
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	m := New(cfg, Options{Agents: []string{"habakkuk"}, Secrets: func() []string { return []string{"hunter2-very-secret"} }})
	m.newBackend = func(string) (Backend, error) { return f, nil }
	reg := tools.NewRegistry()
	RegisterTools(reg, m)
	blobs, err := store.NewBlobs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env := &tools.Env{Agent: "habakkuk", Mode: tools.ModeFull, Blobs: blobs, Taint: tools.NewTaint(false)}
	return &harness{t: t, m: m, f: f, reg: reg, env: env}
}

func (h *harness) call(name string, input any) (llm.Block, []llm.Block) {
	h.t.Helper()
	raw, _ := json.Marshal(input)
	return h.reg.ExecuteMedia(context.Background(), h.env, llm.Block{Type: llm.ToolUse, ToolUseID: "call_1", ToolName: name, Input: raw})
}

func TestScreenshotScalesAndMapsCoordinates(t *testing.T) {
	h := newHarness(t, config.Computer{}, &fake{pixW: 2732, pixH: 1536})
	res, media := h.call("computer_screenshot", map[string]any{})
	if res.IsError || len(media) != 1 {
		t.Fatalf("%+v %v", res, media)
	}
	if !strings.Contains(res.Text, "Screenshot 1366x768 of a 2732x1536 screen (scaled by 0.500)") || !strings.Contains(res.Text, "untrusted") {
		t.Fatalf("text: %s", res.Text)
	}
	if media[0].ToolUseID != "call_1" || media[0].MediaType != "image/png" {
		t.Fatalf("media %+v", media[0])
	}
	data, _ := h.env.Blobs.Get(media[0].Media)
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width != 1366 || cfg.Height != 768 {
		t.Fatalf("image %+v %v", cfg, err)
	}
	if !h.env.Taint.Marked() {
		t.Fatal("a screenshot must taint the turn")
	}
	res, _ = h.call("computer_click", map[string]any{"x": 683, "y": 384, "double": true})
	if res.IsError {
		t.Fatal(res.Text)
	}
	a := h.f.actions[0]
	if a.kind != "click" || a.x != 1366 || a.y != 768 || a.count != 2 || a.button != "left" {
		t.Fatalf("action %+v", a)
	}
	res, _ = h.call("computer_click", map[string]any{"x": 1366, "y": 10})
	if !res.IsError || !strings.Contains(res.Text, "outside the screenshot") {
		t.Fatalf("out of bounds: %s", res.Text)
	}
	res, _ = h.call("computer_drag", map[string]any{"from_x": 0, "from_y": 0, "to_x": 1365, "to_y": 767})
	if a := h.f.actions[1]; res.IsError || a.x2 != 2730 || a.y2 != 1534 {
		t.Fatalf("drag %+v %s", a, res.Text)
	}
}

func TestRetinaInputSpace(t *testing.T) {
	// A Retina Mac: 2880x1800 pixels, input in 1440x900 points.
	h := newHarness(t, config.Computer{}, &fake{pixW: 2880, pixH: 1800, inW: 1440, inH: 900})
	// No screenshot yet: the geometry is learned from one the model does
	// not see.
	res, media := h.call("computer_move", map[string]any{"x": 683, "y": 427})
	if res.IsError || len(media) != 0 || h.f.shots != 1 {
		t.Fatalf("%s %v shots=%d", res.Text, media, h.f.shots)
	}
	if a := h.f.actions[0]; a.x != 720 || a.y != 450 {
		t.Fatalf("mapped to %d,%d", a.x, a.y)
	}
	res, _ = h.call(NativeTool, map[string]any{"action": "cursor_position"})
	if res.Text != "X=474, Y=237" { // 500,250 points in a 1366x854 shot
		t.Fatalf("cursor: %s", res.Text)
	}
}

func TestZoom(t *testing.T) {
	h := newHarness(t, config.Computer{}, &fake{pixW: 2732, pixH: 1536})
	res, _ := h.call(NativeTool, map[string]any{"action": "zoom", "region": []int{0, 0, 100, 50}})
	if !res.IsError {
		t.Fatal("zoom before a screenshot should fail")
	}
	h.call(NativeTool, map[string]any{"action": "screenshot"})
	res, media := h.call(NativeTool, map[string]any{"action": "zoom", "region": []int{0, 0, 100, 50}})
	if res.IsError || len(media) != 1 {
		t.Fatalf("%s", res.Text)
	}
	data, _ := h.env.Blobs.Get(media[0].Media)
	cfg, _ := png.DecodeConfig(bytes.NewReader(data))
	if cfg.Width != 200 || cfg.Height != 100 {
		t.Fatalf("zoom shows %dx%d, want the 200x100 pixels behind the region", cfg.Width, cfg.Height)
	}
	if res, _ := h.call(NativeTool, map[string]any{"action": "zoom", "region": []int{0, 0, 5000, 50}}); !res.IsError {
		t.Fatal("region outside the screenshot accepted")
	}
}

func TestActionBudget(t *testing.T) {
	h := newHarness(t, config.Computer{MaxActions: 3}, &fake{pixW: 800, pixH: 600})
	for range 3 {
		if res, _ := h.call("computer_move", map[string]any{"x": 1, "y": 1}); res.IsError {
			t.Fatal(res.Text)
		}
	}
	if res, _ := h.call("computer_wait", map[string]any{"ms": 1}); res.IsError {
		t.Fatalf("waits do not count: %s", res.Text)
	}
	res, _ := h.call("computer_move", map[string]any{"x": 1, "y": 1})
	if !res.IsError || !strings.Contains(res.Text, "max_actions") {
		t.Fatalf("fourth action: %s", res.Text)
	}
	// The next turn has a new environment and a new budget.
	h.env = &tools.Env{Agent: "habakkuk", Mode: tools.ModeFull, Taint: tools.NewTaint(false)}
	if res, _ := h.call("computer_move", map[string]any{"x": 1, "y": 1}); res.IsError {
		t.Fatal(res.Text)
	}
}

func TestSecretRefusal(t *testing.T) {
	for text, why := range map[string]string{
		"sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123":    "credential",
		"token ghp_abcdefghijklmnopqrstuvwxyz0123456789": "credential",
		"my pass is hunter2-very-secret ok":              "configured secrets",
		"4111 1111 1111 1111":                            "payment card",
		"-----BEGIN OPENSSH PRIVATE KEY-----":            "credential",
	} {
		if got := secretIn(text, []string{"hunter2-very-secret"}); !strings.Contains(got, why) {
			t.Errorf("%q: %q", text, got)
		}
	}
	for _, ok := range []string{"hello world", "call 416-555-0199", "order 1234567890123", "4111 1111 1111 1112"} {
		if got := secretIn(ok, []string{"hunter2-very-secret"}); got != "" {
			t.Errorf("%q flagged: %s", ok, got)
		}
	}
	h := newHarness(t, config.Computer{}, &fake{pixW: 800, pixH: 600})
	for name, input := range map[string]any{
		"computer_type": map[string]any{"text": "key: sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123"},
		NativeTool:      map[string]any{"action": "type", "text": "hunter2-very-secret"},
		"computer_open": map[string]any{"target": "https://x.test/?t=ghp_abcdefghijklmnopqrstuvwxyz0123456789"},
	} {
		res, _ := h.call(name, input)
		if !res.IsError || !strings.Contains(res.Text, "never type") || !strings.Contains(res.Text, "any mode") {
			t.Errorf("%s: %s", name, res.Text)
		}
	}
	if len(h.f.actions) != 0 {
		t.Fatalf("refused text reached the desktop: %+v", h.f.actions)
	}
	if res, _ := h.call("computer_type", map[string]any{"text": "hello"}); res.IsError || h.f.actions[0].text != "hello" {
		t.Fatalf("ordinary text: %s", res.Text)
	}
}

type approver struct {
	asked []tools.ApprovalRequest
	ok    bool
}

func (a *approver) Approve(_ context.Context, req tools.ApprovalRequest) (bool, error) {
	a.asked = append(a.asked, req)
	return a.ok, nil
}

type allowAll struct{}

func (allowAll) Review(context.Context, tools.ApprovalRequest) tools.Review {
	return tools.Review{Verdict: tools.VerdictAllow, Reason: "fine"}
}

func TestConfirmApps(t *testing.T) {
	f := &fake{pixW: 800, pixH: 600, front: "1Password"}
	h := newHarness(t, config.Computer{ConfirmApps: []string{"1password", "Terminal"}}, f)
	// Full mode and a reviewer that allows everything are not enough.
	h.env.Approvals, h.env.Reviewer = tools.ApproveReviewer, allowAll{}
	res, _ := h.call("computer_click", map[string]any{"x": 1, "y": 1})
	if !res.IsError || !strings.Contains(res.Text, "needs a human's approval") || len(f.actions) != 0 {
		t.Fatalf("unattended: %s", res.Text)
	}
	ap := &approver{ok: true}
	h.env.Approver = ap
	if res, _ := h.call("computer_click", map[string]any{"x": 1, "y": 1}); res.IsError || len(ap.asked) != 1 || !strings.Contains(ap.asked[0].Summary, "1password is in front") {
		t.Fatalf("approved: %s %+v", res.Text, ap.asked)
	}
	// Opening a listed app asks too, whatever is in front.
	f.front = "Finder"
	if res, _ := h.call("computer_open", map[string]any{"target": "Terminal"}); res.IsError || len(ap.asked) != 2 {
		t.Fatalf("open: %s %d", res.Text, len(ap.asked))
	}
	if res, _ := h.call("computer_move", map[string]any{"x": 1, "y": 1}); res.IsError || len(ap.asked) != 2 {
		t.Fatalf("an unlisted app asked: %s", res.Text)
	}
	// When the app in front cannot be told, ask.
	f.frontErr = errors.New("no window")
	if h.call("computer_move", map[string]any{"x": 1, "y": 1}); len(ap.asked) != 3 {
		t.Fatal("unknown front app did not ask")
	}
}

func TestRefusesOtherAgentsAndSandboxes(t *testing.T) {
	h := newHarness(t, config.Computer{}, &fake{pixW: 800, pixH: 600})
	h.env.Agent = "other"
	if res, _ := h.call("computer_screenshot", map[string]any{}); !res.IsError || !strings.Contains(res.Text, "does not have the computer toolset") {
		t.Fatalf("%s", res.Text)
	}
	h.env.Agent, h.env.Backend = "habakkuk", sandboxed{}
	if res, _ := h.call("computer_screenshot", map[string]any{}); !res.IsError || !strings.Contains(res.Text, "off for sandboxed agents") {
		t.Fatalf("%s", res.Text)
	}
}

type sandboxed struct{}

func (sandboxed) Command(context.Context, string, string, bool) (*exec.Cmd, func(), error) {
	return nil, nil, errors.New("unused")
}
func (sandboxed) Remote() bool     { return true }
func (sandboxed) Describe() string { return "a docker container" }

func TestRiskIsExec(t *testing.T) {
	h := newHarness(t, config.Computer{}, &fake{pixW: 800, pixH: 600})
	h.env.Mode = tools.ModeWorkspace
	res, _ := h.call("computer_screenshot", map[string]any{})
	if !res.IsError || !strings.Contains(res.Text, "needs approval") {
		t.Fatalf("workspace mode ran a computer action unasked: %s", res.Text)
	}
	if res, _ := h.call("computer_wait", map[string]any{"ms": 1}); res.IsError {
		t.Fatalf("wait: %s", res.Text)
	}
}

func TestParseNative(t *testing.T) {
	for in, want := range map[string]step{
		`{"action":"left_click","coordinate":[10,20],"text":"shift"}`:              {kind: "click", at: true, x: 10, y: 20, button: "left", count: 1, mods: []string{"shift"}},
		`{"action":"triple_click"}`:                                                {kind: "click", button: "left", count: 3},
		`{"action":"left_click_drag","start_coordinate":[1,2],"coordinate":[3,4]}`: {kind: "drag", x: 1, y: 2, x2: 3, y2: 4},
		`{"action":"scroll","scroll_direction":"up","scroll_amount":5}`:            {kind: "scroll", dy: -5},
		`{"action":"scroll","scroll_direction":"right","coordinate":[7,8]}`:        {kind: "scroll", dx: 3, at: true, x: 7, y: 8},
		`{"action":"wait","duration":2.5}`:                                         {kind: "wait", dur: 2500 * time.Millisecond},
		`{"action":"left_mouse_down"}`:                                             {kind: "down", button: "left"},
		`{"action":"type","text":"hi"}`:                                            {kind: "type", text: "hi"},
	} {
		got, err := parseNative(json.RawMessage(in))
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got.kind != want.kind || got.x != want.x || got.y != want.y || got.x2 != want.x2 || got.y2 != want.y2 || got.at != want.at ||
			got.button != want.button || got.count != want.count || got.dx != want.dx || got.dy != want.dy || got.dur != want.dur ||
			got.text != want.text || strings.Join(got.mods, "+") != strings.Join(want.mods, "+") {
			t.Errorf("%s:\n got %+v\nwant %+v", in, got, want)
		}
	}
	got, _ := parseNative(json.RawMessage(`{"action":"key","text":"ctrl+s","repeat":2}`))
	if got.kind != "key" || xdoCombo(got.keys) != "ctrl+s" || got.repeat != 2 {
		t.Errorf("key: %+v", got)
	}
	for _, bad := range []string{`{"action":"fly"}`, `{"action":"mouse_move"}`, `{"action":"wait","duration":301}`, `{"action":"left_click","text":"ctrl+x"}`, `{"action":"scroll","scroll_direction":"sideways"}`} {
		if _, err := parseNative(json.RawMessage(bad)); err == nil {
			t.Errorf("%s: want an error", bad)
		}
	}
}

func TestGeometry(t *testing.T) {
	if w, h := fit(1920, 1080, MaxShot); w != 1366 || h != 768 {
		t.Errorf("fit 1920x1080: %dx%d", w, h)
	}
	if w, h := fit(1080, 1920, MaxShot); w != 768 || h != 1366 {
		t.Errorf("fit portrait: %dx%d", w, h)
	}
	if w, h := fit(1280, 800, MaxShot); w != 1280 || h != 800 {
		t.Errorf("small screens are not scaled: %dx%d", w, h)
	}
	g := geometry{shotW: 1366, shotH: 768, pixW: 1920, pixH: 1080, inW: 1920, inH: 1080}
	if x, y, _ := g.toInput(1365, 767); x != 1919 || y != 1079 {
		t.Errorf("corner maps to %d,%d", x, y)
	}
	if x, y, _ := g.toInput(683, 384); x != 960 || y != 540 {
		t.Errorf("centre maps to %d,%d", x, y)
	}
	if _, _, err := (geometry{}).toInput(1, 1); err == nil {
		t.Error("unknown geometry accepted")
	}
	// Box filter: a 4x2 image of two halves averages to two pixels.
	src := image.NewRGBA(image.Rect(0, 0, 4, 2))
	for y := range 2 {
		for x := range 4 {
			v := uint8(0)
			if x >= 2 {
				v = 200
			}
			src.Set(x, y, color.RGBA{v, v, v, 255})
		}
	}
	dst := resize(src, 2, 1)
	if dst.RGBAAt(0, 0).R != 0 || dst.RGBAAt(1, 0).R != 200 {
		t.Errorf("resize: %v %v", dst.RGBAAt(0, 0), dst.RGBAAt(1, 0))
	}
}

func TestNativeModel(t *testing.T) {
	for m, want := range map[string]bool{
		"claude-opus-5-5": true, "claude-sonnet-5": true, "claude-haiku-5-5": true, "claude-mythos-preview": true,
		"claude-fable-5-1": true, "claude-opus-4-7": false, "claude-sonnet-4-5-20250929": false, "gpt-5": false,
	} {
		if NativeModel(m) != want {
			t.Errorf("%s: %v", m, !want)
		}
	}
}

func TestXauthFile(t *testing.T) {
	path := t.TempDir() + "/a.xauth"
	if err := writeXauth(path, 99); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	// family, empty address, "99", the cookie name, a 16-byte cookie.
	if len(data) != 2+2+(2+2)+(2+18)+(2+16) || data[0] != 0xFF || data[1] != 0xFF || string(data[6:8]) != "99" ||
		!bytes.Contains(data, []byte("MIT-MAGIC-COOKIE-1")) {
		t.Fatalf("xauth %x", data)
	}
}
