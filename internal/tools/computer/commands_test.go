package computer

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseCombo(t *testing.T) {
	for _, tc := range []struct {
		in   string
		mods []string
		key  string
	}{
		{"ctrl+l", []string{"ctrl"}, "l"},
		{"Return", nil, "return"},
		{"enter", nil, "return"},
		{"cmd+shift+T", []string{"super", "shift"}, "T"},
		{"Control + Alt + Delete", []string{"ctrl", "alt"}, "delete"},
		{"ctrl++", []string{"ctrl"}, "+"},
		{"+", nil, "+"},
		{"F5", nil, "f5"},
		{"Page_Down", nil, "pagedown"},
		{"shift", []string{"shift"}, ""},
		{"XF86AudioMute", nil, "XF86AudioMute"},
	} {
		c, err := parseCombo(tc.in)
		if err != nil {
			t.Fatalf("%q: %v", tc.in, err)
		}
		if !reflect.DeepEqual(c.mods, tc.mods) || c.key != tc.key {
			t.Errorf("%q: got %+v", tc.in, c)
		}
	}
	for _, bad := range []string{"", "ctrl+", "l+ctrl+x", "ctrl++x"} {
		if _, err := parseCombo(bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
	if _, err := parseMods("ctrl+l"); err == nil {
		t.Error("a key is not a held modifier")
	}
}

func TestKeyMappings(t *testing.T) {
	c := func(s string) combo { k, _ := parseCombo(s); return k }
	for in, want := range map[string]string{"ctrl+l": "ctrl+l", "Return": "Return", "alt+Tab": "alt+Tab", "pgdn": "Next", "cmd+/": "super+slash", "f11": "F11"} {
		if got := xdoCombo(c(in)); got != want {
			t.Errorf("xdotool %q: %q, want %q", in, got, want)
		}
	}
	args, err := cliclickKeyArgs(c("cmd+shift+t"))
	if err != nil || !reflect.DeepEqual(args, []string{"kd:cmd,shift", "t:t", "ku:cmd,shift"}) {
		t.Errorf("cliclick: %v %v", args, err)
	}
	args, _ = cliclickKeyArgs(c("BackSpace"))
	if !reflect.DeepEqual(args, []string{"kp:delete"}) {
		t.Errorf("cliclick backspace: %v", args)
	}
	script, sargs, err := appleScriptKey(c("cmd+l"), 2)
	if err != nil || !strings.Contains(script, "keystroke (item 1 of argv) using {command down}") || !strings.Contains(script, "repeat 2 times") || !reflect.DeepEqual(sargs, []string{"l"}) {
		t.Errorf("applescript: %s %v %v", script, sargs, err)
	}
	script, sargs, _ = appleScriptKey(c("ctrl+Left"), 1)
	if !strings.Contains(script, "key code 123 using {control down}") || sargs != nil {
		t.Errorf("applescript arrow: %s %v", script, sargs)
	}
	codes, err := evdevCodes(c("ctrl+L"))
	if err != nil || !reflect.DeepEqual(codes, []int{29, 42, 38}) {
		t.Errorf("evdev: %v %v", codes, err)
	}
	if got := ydoKeyArgs([]int{29, 38}, true, true); !reflect.DeepEqual(got, []string{"29:1", "38:1", "38:0", "29:0"}) {
		t.Errorf("ydotool key args %v", got)
	}
}

func argv(cmds []cmd) []string {
	var out []string
	for _, c := range cmds {
		out = append(out, strings.Join(append([]string{c.name}, c.args...), " "))
	}
	return out
}

func TestX11Commands(t *testing.T) {
	ctrlL, _ := parseCombo("ctrl+l")
	for _, tc := range []struct {
		a    action
		want []string
	}{
		{action{kind: "click", at: true, x: 10, y: 20, button: "left", count: 2},
			[]string{"xdotool mousemove 10 20 click --repeat 2 --delay 80 1"}},
		{action{kind: "click", button: "right", count: 1, mods: []string{"shift"}},
			[]string{"xdotool keydown shift click --repeat 1 --delay 80 3 keyup shift"}},
		{action{kind: "move", x: 5, y: 6}, []string{"xdotool mousemove 5 6"}},
		{action{kind: "drag", x: 0, y: 0, x2: 100, y2: 50},
			[]string{"xdotool mousemove 0 0 mousedown 1 sleep 0.1 mousemove 50 25 mousemove 100 50 sleep 0.1 mouseup 1"}},
		{action{kind: "type", text: "-rf hello"}, []string{"xdotool type --delay 12 -- -rf hello"}},
		{action{kind: "key", keys: ctrlL, repeat: 3}, []string{"xdotool key --repeat 3 --delay 50 -- ctrl+l"}},
		{action{kind: "hold", keys: ctrlL, dur: time.Second}, []string{"xdotool keydown ctrl+l", "xdotool keyup ctrl+l"}},
		{action{kind: "scroll", at: true, x: 1, y: 2, dy: -3, dx: 2},
			[]string{"xdotool mousemove 1 2 click --repeat 3 --delay 30 4 click --repeat 2 --delay 30 7"}},
		{action{kind: "down", button: "left"}, []string{"xdotool mousedown 1"}},
	} {
		cmds, err := x11Commands(tc.a)
		if err != nil {
			t.Fatalf("%+v: %v", tc.a, err)
		}
		if got := argv(cmds); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s:\n got %q\nwant %q", tc.a.kind, got, tc.want)
		}
	}
	if _, err := x11Commands(action{kind: "click", button: "fourth"}); err == nil {
		t.Error("unknown button accepted")
	}
	for tool, want := range map[string]string{
		"import": "import -silent -window root png:-",
		"scrot":  "scrot --overwrite /tmp/x.png",
		"ffmpeg": "ffmpeg -loglevel error -f x11grab -i :7 -frames:v 1 -f image2 -vcodec png pipe:1",
	} {
		if got := argv([]cmd{x11ShotCmd(tool, ":7", "/tmp/x.png")})[0]; got != want {
			t.Errorf("%s: %q", tool, got)
		}
	}
}

func TestLinuxOpen(t *testing.T) {
	look := func(name string) (string, error) {
		if name == "firefox" {
			return "/usr/bin/firefox", nil
		}
		return "", errors.New("not found")
	}
	for in, want := range map[string]string{
		"https://example.com":     "xdg-open https://example.com",
		"/home/me/a.pdf":          "xdg-open /home/me/a.pdf",
		"firefox --new-window x":  "firefox --new-window x",
		"org.gnome.TextEditor":    "gtk-launch org.gnome.TextEditor",
		"mailto:someone@example.": "xdg-open mailto:someone@example.",
	} {
		c, err := linuxOpen(in, look)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got := argv([]cmd{c})[0]; got != want || !c.detach {
			t.Errorf("%q: %q (detach %v)", in, got, c.detach)
		}
	}
	if _, err := linuxOpen("nosuchapp --flag", look); err == nil {
		t.Error("unknown app with arguments accepted")
	}
}

func TestWaylandCommands(t *testing.T) {
	ctrlL, _ := parseCombo("ctrl+l")
	for _, tc := range []struct {
		a    action
		want []string
	}{
		{action{kind: "click", at: true, x: 10, y: 20, button: "right", count: 1},
			[]string{"ydotool mousemove --absolute -x 10 -y 20", "ydotool click --repeat 1 --next-delay 80 0xC1"}},
		{action{kind: "key", keys: ctrlL, repeat: 2}, []string{"ydotool key 29:1 38:1 38:0 29:0 29:1 38:1 38:0 29:0"}},
		{action{kind: "type", text: "hi"}, []string{"ydotool type -- hi"}},
		{action{kind: "scroll", dy: 2}, []string{"ydotool mousemove --wheel -x 0 -y -2"}},
		{action{kind: "drag", x: 1, y: 1, x2: 3, y2: 3},
			[]string{"ydotool mousemove --absolute -x 1 -y 1", "ydotool click 0x40", "ydotool mousemove --absolute -x 2 -y 2", "ydotool mousemove --absolute -x 3 -y 3", "ydotool click 0x80"}},
		{action{kind: "click", button: "left", count: 1, mods: []string{"ctrl"}},
			[]string{"ydotool key 29:1", "ydotool click --repeat 1 --next-delay 80 0xC0", "ydotool key 29:0"}},
	} {
		cmds, err := waylandCommands(tc.a)
		if err != nil {
			t.Fatalf("%+v: %v", tc.a, err)
		}
		if got := argv(cmds); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s:\n got %q\nwant %q", tc.a.kind, got, tc.want)
		}
	}
}

func TestMacCommands(t *testing.T) {
	cmdL, _ := parseCombo("cmd+l")
	// With cliclick.
	for _, tc := range []struct {
		a    action
		want string
	}{
		{action{kind: "click", at: true, x: 10, y: 20, button: "left", count: 1}, "cliclick c:10,20"},
		{action{kind: "click", button: "left", count: 2}, "cliclick dc:."},
		{action{kind: "click", at: true, x: 1, y: 2, button: "right", count: 1}, "cliclick rc:1,2"},
		{action{kind: "drag", x: 1, y: 2, x2: 3, y2: 4}, "cliclick dd:1,2 m:3,4 du:3,4"},
		{action{kind: "key", keys: cmdL, repeat: 1}, "cliclick kd:cmd t:l ku:cmd"},
		{action{kind: "open", text: "Safari"}, "open -a Safari"},
		{action{kind: "open", text: "https://example.com"}, "open https://example.com"},
	} {
		cmds, err := macCommands(tc.a, true)
		if err != nil {
			t.Fatalf("%+v: %v", tc.a, err)
		}
		if got := argv(cmds)[0]; got != tc.want {
			t.Errorf("%s: %q, want %q", tc.a.kind, got, tc.want)
		}
	}
	// Without: osascript, with text only ever in arguments.
	cmds, _ := macCommands(action{kind: "type", text: `say "hi"` + "\nnext"}, false)
	if c := cmds[0]; c.name != "osascript" || !reflect.DeepEqual(c.args[len(c.args)-2:], []string{`say "hi"`, "next"}) || strings.Contains(c.args[1], "hi") {
		t.Errorf("type: %q", c.args)
	}
	cmds, _ = macCommands(action{kind: "click", at: true, x: 10, y: 20, button: "middle", count: 1}, true)
	if c := cmds[0]; c.name != "osascript" || c.args[1] != "JavaScript" || !strings.Contains(c.args[3], "var x = 10, y = 20") || !strings.Contains(c.args[3], "post(25, x, y, 2, i, 0)") {
		t.Errorf("middle click: %q", c.args)
	}
	cmds, _ = macCommands(action{kind: "click", at: true, x: 10, y: 20, button: "left", count: 1, mods: []string{"super"}}, true)
	if c := cmds[0]; c.name != "osascript" || !strings.Contains(c.args[3], "1048576") {
		t.Errorf("cmd-click: %q", c.args)
	}
	cmds, _ = macCommands(action{kind: "scroll", at: true, x: 5, y: 5, dy: 3}, true)
	if !strings.Contains(cmds[0].args[3], "CGEventCreateScrollWheelEvent2(null, 1, 2, -3, 0, 0)") {
		t.Errorf("scroll: %q", cmds[0].args[3])
	}
	cmds, _ = macCommands(action{kind: "key", keys: cmdL, repeat: 1}, false)
	if c := cmds[0]; c.name != "osascript" || !strings.Contains(c.args[1], "command down") || c.args[len(c.args)-1] != "l" {
		t.Errorf("key without cliclick: %q", c.args)
	}
}

func TestRunAllReleasesHeldKey(t *testing.T) {
	var ran []string
	run := func(ctx context.Context, env []string, c cmd) ([]byte, error) {
		ran = append(ran, strings.Join(c.args, " "))
		return nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runAll(ctx, run, nil, []cmd{{name: "xdotool", args: []string{"keydown", "a"}, sleep: time.Minute}, {name: "xdotool", args: []string{"keyup", "a"}}}, true)
	if err == nil || !reflect.DeepEqual(ran, []string{"keydown a", "keyup a"}) {
		t.Fatalf("err %v, ran %v", err, ran)
	}
}
