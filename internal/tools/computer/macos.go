package computer

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// macos drives this Mac's main display: screenshots with screencapture,
// input with cliclick when it is installed, else osascript (AppleScript
// System Events for keys and typing, JavaScript for Automation posting
// Quartz mouse events for the pointer). The process running Habakkuk needs
// Screen Recording (screenshots) and Accessibility (input) permission in
// System Settings > Privacy & Security.
type macos struct {
	run      runFunc
	cliclick bool
}

func newMacOS() *macos {
	_, err := exec.LookPath("cliclick")
	return &macos{run: execRun, cliclick: err == nil}
}

func (b *macos) Name() string                { return "macos" }
func (b *macos) Start(context.Context) error { return nil }
func (b *macos) Close()                      {}

func (b *macos) Screenshot(ctx context.Context) ([]byte, error) {
	path, done, err := tempPNG()
	if err != nil {
		return nil, err
	}
	defer done()
	data, err := readTemp(ctx, b.run, nil, path, cmd{name: "screencapture", args: []string{"-x", "-t", "png", path}})
	if err != nil {
		return nil, fmt.Errorf("%w (does Habakkuk have Screen Recording permission?)", err)
	}
	return data, nil
}

// jxa runs a JavaScript for Automation script. Scripts only ever embed
// numbers; text goes through AppleScript arguments.
func jxa(script string) cmd {
	return cmd{name: "osascript", args: []string{"-l", "JavaScript", "-e", script}}
}

// InputSize is the main display in points, the space Quartz events and
// cliclick take; screenshots are in pixels, twice that on Retina.
func (b *macos) InputSize(ctx context.Context) (int, int, error) {
	out, err := b.run(ctx, nil, jxa(`ObjC.import('AppKit'); var f = $.NSScreen.mainScreen.frame; Math.round(f.size.width) + ' ' + Math.round(f.size.height)`))
	if err != nil {
		return 0, 0, err
	}
	f := strings.Fields(string(out))
	if len(f) != 2 {
		return 0, 0, fmt.Errorf("unreadable screen size %q", out)
	}
	w, err1 := strconv.Atoi(f[0])
	h, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("unreadable screen size %q", out)
	}
	return w, h, nil
}

func (b *macos) Do(ctx context.Context, a action) error {
	cmds, err := macCommands(a, b.cliclick)
	if err != nil {
		return err
	}
	if err := runAll(ctx, b.run, nil, cmds, false); err != nil {
		return fmt.Errorf("%w (does Habakkuk have Accessibility permission?)", err)
	}
	return nil
}

func (b *macos) Cursor(ctx context.Context) (int, int, error) {
	out, err := b.run(ctx, nil, jxa(`ObjC.import('CoreGraphics'); var p = $.CGEventGetLocation($.CGEventCreate(null)); Math.round(p.x) + ',' + Math.round(p.y)`))
	if err != nil {
		return 0, 0, err
	}
	x, y, ok := strings.Cut(strings.TrimSpace(string(out)), ",")
	xi, err1 := strconv.Atoi(x)
	yi, err2 := strconv.Atoi(y)
	if !ok || err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("unreadable pointer position %q", out)
	}
	return xi, yi, nil
}

const frontScript = `tell application "System Events"
set p to first application process whose frontmost is true
set n to name of p
set t to ""
try
set t to name of front window of p
end try
end tell
return n & linefeed & t`

func (b *macos) Front(ctx context.Context) (string, string, error) {
	out, err := b.run(ctx, nil, cmd{name: "osascript", args: []string{"-e", frontScript}})
	if err != nil {
		return "", "", err
	}
	app, title, _ := strings.Cut(strings.TrimRight(string(out), "\n"), "\n")
	return strings.TrimSpace(app), strings.TrimSpace(title), nil
}

// Quartz event types and flags.
const (
	cgLeftDown, cgLeftUp, cgRightDown, cgRightUp, cgMoved, cgLeftDragged = 1, 2, 3, 4, 5, 6
	cgOtherDown, cgOtherUp                                               = 25, 26
)

var cgFlags = map[string]int{"shift": 0x20000, "ctrl": 0x40000, "alt": 0x80000, "super": 0x100000}

func cgFlagsOf(mods []string) int {
	f := 0
	for _, m := range mods {
		f |= cgFlags[m]
	}
	return f
}

// jxaPrelude defines post(type, x, y, button, clickState, flags) and here(),
// the pointer's position.
const jxaPrelude = `ObjC.import('CoreGraphics');
function post(t, x, y, b, n, f) { var e = $.CGEventCreateMouseEvent(null, t, {x: x, y: y}, b); if (n) $.CGEventSetIntegerValueField(e, 1, n); if (f) $.CGEventSetFlags(e, f); $.CGEventPost(0, e); }
function here() { return $.CGEventGetLocation($.CGEventCreate(null)); }
`

// macCommands is the command line for an action.
func macCommands(a action, cliclick bool) ([]cmd, error) {
	switch a.kind {
	case "open":
		t := strings.TrimSpace(a.text)
		if t == "" {
			return nil, errors.New("nothing to open")
		}
		if isURL(t) || strings.HasPrefix(t, "/") || strings.HasPrefix(t, "~/") {
			return []cmd{{name: "open", args: []string{t}}}, nil
		}
		return []cmd{{name: "open", args: []string{"-a", t}}}, nil
	case "type":
		// Lines go as arguments; Return is pressed between them.
		script := "on run argv\ntell application \"System Events\"\nrepeat with i from 1 to count of argv\nif i > 1 then key code 36\nkeystroke (item i of argv)\nend repeat\nend tell\nend run"
		return []cmd{{name: "osascript", args: append([]string{"-e", script, "--"}, strings.Split(a.text, "\n")...)}}, nil
	case "key":
		if cliclick {
			args, err := cliclickKeyArgs(a.keys)
			if err == nil {
				var all []string
				for range max(a.repeat, 1) {
					all = append(all, args...)
				}
				return []cmd{{name: "cliclick", args: all}}, nil
			}
		}
		script, args, err := appleScriptKey(a.keys, a.repeat)
		if err != nil {
			return nil, err
		}
		return []cmd{{name: "osascript", args: append([]string{"-e", script, "--"}, args...)}}, nil
	case "hold":
		codes, err := macKeyCodes(a.keys)
		if err != nil {
			return nil, err
		}
		var s strings.Builder
		s.WriteString("ObjC.import('CoreGraphics');\nfunction key(c, d) { $.CGEventPost(0, $.CGEventCreateKeyboardEvent(null, c, d)); }\n")
		for _, c := range codes {
			fmt.Fprintf(&s, "key(%d, true);\n", c)
		}
		fmt.Fprintf(&s, "delay(%.3f);\n", a.dur.Seconds())
		for i := len(codes) - 1; i >= 0; i-- {
			fmt.Fprintf(&s, "key(%d, false);\n", codes[i])
		}
		return []cmd{jxa(s.String())}, nil
	}
	if cliclick && len(a.mods) == 0 {
		if c, ok := cliclickMouse(a); ok {
			return []cmd{c}, nil
		}
	}
	script, err := jxaMouse(a)
	if err != nil {
		return nil, err
	}
	return []cmd{jxa(script)}, nil
}

// cliclickMouse is the cliclick command for a pointer action it supports
// (not middle clicks or scrolling).
func cliclickMouse(a action) (cmd, bool) {
	at := "."
	if a.at || a.kind == "move" || a.kind == "drag" {
		at = strconv.Itoa(a.x) + "," + strconv.Itoa(a.y)
	}
	switch a.kind {
	case "move":
		return cmd{name: "cliclick", args: []string{"m:" + at}}, true
	case "click":
		prefix := map[string]map[int]string{"left": {1: "c:", 2: "dc:", 3: "tc:"}, "right": {1: "rc:"}}[a.button][max(a.count, 1)]
		if prefix == "" {
			return cmd{}, false
		}
		return cmd{name: "cliclick", args: []string{prefix + at}}, true
	case "drag":
		end := strconv.Itoa(a.x2) + "," + strconv.Itoa(a.y2)
		return cmd{name: "cliclick", args: []string{"dd:" + at, "m:" + end, "du:" + end}}, true
	case "down", "up":
		if a.button != "left" {
			return cmd{}, false
		}
		return cmd{name: "cliclick", args: []string{map[string]string{"down": "dd:.", "up": "du:."}[a.kind]}}, true
	}
	return cmd{}, false
}

// jxaMouse is a JavaScript for Automation script posting Quartz mouse
// events for a pointer action.
func jxaMouse(a action) (string, error) {
	var s strings.Builder
	s.WriteString(jxaPrelude)
	if a.at || a.kind == "move" || a.kind == "drag" {
		fmt.Fprintf(&s, "var x = %d, y = %d;\n", a.x, a.y)
	} else {
		s.WriteString("var p = here(), x = p.x, y = p.y;\n")
	}
	flags := cgFlagsOf(a.mods)
	downUp := map[string][3]int{"left": {cgLeftDown, cgLeftUp, 0}, "right": {cgRightDown, cgRightUp, 1}, "middle": {cgOtherDown, cgOtherUp, 2}}
	switch a.kind {
	case "move":
		s.WriteString("post(5, x, y, 0, 0, 0);\n")
	case "click":
		du, ok := downUp[a.button]
		if !ok {
			return "", fmt.Errorf("unknown button %q", a.button)
		}
		fmt.Fprintf(&s, "post(%d, x, y, 0, 0, 0);\n", cgMoved)
		fmt.Fprintf(&s, "for (var i = 1; i <= %d; i++) { post(%d, x, y, %d, i, %d); post(%d, x, y, %d, i, %d); delay(0.03); }\n",
			max(a.count, 1), du[0], du[2], flags, du[1], du[2], flags)
	case "drag":
		fmt.Fprintf(&s, "post(%d, x, y, 0, 0, 0); post(%d, x, y, 0, 1, %d); delay(0.1);\n", cgMoved, cgLeftDown, flags)
		fmt.Fprintf(&s, "for (var i = 1; i <= 10; i++) { post(%d, x + (%d - x) * i / 10, y + (%d - y) * i / 10, 0, 0, %d); delay(0.02); }\n",
			cgLeftDragged, a.x2, a.y2, flags)
		fmt.Fprintf(&s, "post(%d, %d, %d, 0, 1, %d);\n", cgLeftUp, a.x2, a.y2, flags)
	case "down", "up":
		du, ok := downUp[a.button]
		if !ok {
			return "", fmt.Errorf("unknown button %q", a.button)
		}
		t := du[0]
		if a.kind == "up" {
			t = du[1]
		}
		fmt.Fprintf(&s, "post(%d, x, y, %d, 1, %d);\n", t, du[2], flags)
	case "scroll":
		if a.dx == 0 && a.dy == 0 {
			return "", errors.New("scroll by zero")
		}
		// Line units; wheel 1 is vertical (positive up), wheel 2
		// horizontal (positive left).
		fmt.Fprintf(&s, "post(%d, x, y, 0, 0, 0);\nvar e = $.CGEventCreateScrollWheelEvent2(null, 1, 2, %d, %d, 0);\nif (%d) $.CGEventSetFlags(e, %d);\n$.CGEventPost(0, e);\n",
			cgMoved, -a.dy, -a.dx, flags, flags)
	default:
		return "", fmt.Errorf("unknown action %q", a.kind)
	}
	return s.String(), nil
}
