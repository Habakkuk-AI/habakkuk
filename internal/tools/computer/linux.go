package computer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// x11 drives an X display: screenshots with ImageMagick's import, scrot or
// ffmpeg's x11grab, input with xdotool.
type x11 struct {
	display string
	env     []string // DISPLAY, XAUTHORITY
	run     runFunc
	look    func(string) (string, error)
	shooter string // chosen on first use
}

func newX11(display string, env []string) *x11 {
	return &x11{display: display, env: append([]string{"DISPLAY=" + display}, env...), run: execRun, look: exec.LookPath}
}

func (b *x11) Name() string                                { return "linux (X11 " + b.display + ")" }
func (b *x11) Start(context.Context) error                 { return nil }
func (b *x11) Close()                                      {}
func (b *x11) InputSize(context.Context) (int, int, error) { return 0, 0, nil }

// shotTools are the X screenshot commands, in order of preference.
var shotTools = []string{"import", "scrot", "ffmpeg"}

func (b *x11) Screenshot(ctx context.Context) ([]byte, error) {
	if b.shooter == "" {
		for _, t := range shotTools {
			if _, err := b.look(t); err == nil {
				b.shooter = t
				break
			}
		}
		if b.shooter == "" {
			return nil, errors.New("no screenshot command found: install ImageMagick (import), scrot or ffmpeg")
		}
	}
	if b.shooter == "scrot" {
		path, done, err := tempPNG()
		if err != nil {
			return nil, err
		}
		defer done()
		return readTemp(ctx, b.run, b.env, path, x11ShotCmd("scrot", b.display, path))
	}
	return b.run(ctx, b.env, x11ShotCmd(b.shooter, b.display, ""))
}

// x11ShotCmd captures the root window as PNG: to stdout, or to path for
// scrot, which cannot write to a pipe.
func x11ShotCmd(tool, display, path string) cmd {
	switch tool {
	case "scrot":
		return cmd{name: "scrot", args: []string{"--overwrite", path}}
	case "ffmpeg":
		return cmd{name: "ffmpeg", args: []string{"-loglevel", "error", "-f", "x11grab", "-i", display,
			"-frames:v", "1", "-f", "image2", "-vcodec", "png", "pipe:1"}}
	}
	return cmd{name: "import", args: []string{"-silent", "-window", "root", "png:-"}}
}

var xButtons = map[string]string{"left": "1", "middle": "2", "right": "3"}

// xdoMods holds modifiers down around the rest of a chained command.
func xdoMods(mods []string, inner ...string) []string {
	var args []string
	for _, m := range mods {
		args = append(args, "keydown", m)
	}
	args = append(args, inner...)
	for i := len(mods) - 1; i >= 0; i-- {
		args = append(args, "keyup", mods[i])
	}
	return args
}

func xdoMove(x, y int) []string {
	return []string{"mousemove", strconv.Itoa(x), strconv.Itoa(y)}
}

// x11Commands is the xdotool command line for an action.
func x11Commands(a action) ([]cmd, error) {
	xdo := func(args ...string) []cmd { return []cmd{{name: "xdotool", args: args}} }
	switch a.kind {
	case "move":
		return xdo(xdoMove(a.x, a.y)...), nil
	case "click":
		btn, ok := xButtons[a.button]
		if !ok {
			return nil, fmt.Errorf("unknown button %q", a.button)
		}
		var inner []string
		if a.at {
			inner = xdoMove(a.x, a.y)
		}
		inner = append(inner, "click", "--repeat", strconv.Itoa(max(a.count, 1)), "--delay", "80", btn)
		return xdo(xdoMods(a.mods, inner...)...), nil
	case "drag":
		inner := append(xdoMove(a.x, a.y), "mousedown", "1", "sleep", "0.1")
		inner = append(inner, xdoMove((a.x+a.x2)/2, (a.y+a.y2)/2)...)
		inner = append(inner, xdoMove(a.x2, a.y2)...)
		inner = append(inner, "sleep", "0.1", "mouseup", "1")
		return xdo(xdoMods(a.mods, inner...)...), nil
	case "down", "up":
		btn, ok := xButtons[a.button]
		if !ok {
			return nil, fmt.Errorf("unknown button %q", a.button)
		}
		return xdo("mouse"+a.kind, btn), nil
	case "type":
		return xdo("type", "--delay", "12", "--", a.text), nil
	case "key":
		return xdo("key", "--repeat", strconv.Itoa(max(a.repeat, 1)), "--delay", "50", "--", xdoCombo(a.keys)), nil
	case "hold":
		k := xdoCombo(a.keys)
		return []cmd{{name: "xdotool", args: []string{"keydown", k}, sleep: a.dur}, {name: "xdotool", args: []string{"keyup", k}}}, nil
	case "scroll":
		var inner []string
		if a.at {
			inner = xdoMove(a.x, a.y)
		}
		if a.dy != 0 {
			btn := "5"
			if a.dy < 0 {
				btn = "4"
			}
			inner = append(inner, "click", "--repeat", strconv.Itoa(abs(a.dy)), "--delay", "30", btn)
		}
		if a.dx != 0 {
			btn := "7"
			if a.dx < 0 {
				btn = "6"
			}
			inner = append(inner, "click", "--repeat", strconv.Itoa(abs(a.dx)), "--delay", "30", btn)
		}
		if a.dx == 0 && a.dy == 0 {
			return nil, errors.New("scroll by zero")
		}
		return xdo(xdoMods(a.mods, inner...)...), nil
	}
	return nil, fmt.Errorf("unknown action %q", a.kind)
}

func (b *x11) Do(ctx context.Context, a action) error {
	if a.kind == "open" {
		c, err := linuxOpen(a.text, b.look)
		if err != nil {
			return err
		}
		_, err = b.run(ctx, b.env, c)
		return err
	}
	cmds, err := x11Commands(a)
	if err != nil {
		return err
	}
	return runAll(ctx, b.run, b.env, cmds, a.kind == "hold")
}

func (b *x11) Cursor(ctx context.Context) (int, int, error) {
	out, err := b.run(ctx, b.env, cmd{name: "xdotool", args: []string{"getmouselocation", "--shell"}})
	if err != nil {
		return 0, 0, err
	}
	return parseMouseLocation(string(out))
}

// parseMouseLocation reads xdotool getmouselocation --shell.
func parseMouseLocation(s string) (int, int, error) {
	x, y := -1, -1
	for _, line := range strings.Fields(s) {
		k, v, _ := strings.Cut(line, "=")
		n, err := strconv.Atoi(v)
		if err != nil {
			continue
		}
		switch k {
		case "X":
			x = n
		case "Y":
			y = n
		}
	}
	if x < 0 || y < 0 {
		return 0, 0, fmt.Errorf("unreadable pointer position %q", s)
	}
	return x, y, nil
}

func (b *x11) Front(ctx context.Context) (string, string, error) {
	out, err := b.run(ctx, b.env, cmd{name: "xdotool", args: []string{"getactivewindow", "getwindowname"}})
	if err != nil {
		return "", "", err
	}
	title := strings.TrimSpace(string(out))
	app := ""
	if out, err := b.run(ctx, b.env, cmd{name: "xdotool", args: []string{"getactivewindow", "getwindowpid"}}); err == nil {
		if comm, err := os.ReadFile("/proc/" + strings.TrimSpace(string(out)) + "/comm"); err == nil {
			app = strings.TrimSpace(string(comm))
		}
	}
	return app, title, nil
}

// linuxOpen opens a URL or file with xdg-open, or starts an app: a command
// on PATH (with arguments), else a desktop entry through gtk-launch.
func linuxOpen(target string, look func(string) (string, error)) (cmd, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return cmd{}, errors.New("nothing to open")
	}
	if isURL(target) || strings.HasPrefix(target, "/") || strings.HasPrefix(target, "~/") {
		if strings.HasPrefix(target, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				target = home + target[1:]
			}
		}
		return cmd{name: "xdg-open", args: []string{target}, detach: true}, nil
	}
	f := strings.Fields(target)
	if _, err := look(f[0]); err == nil {
		return cmd{name: f[0], args: f[1:], detach: true}, nil
	}
	if len(f) == 1 {
		return cmd{name: "gtk-launch", args: f, detach: true}, nil
	}
	return cmd{}, fmt.Errorf("no app %q on PATH", f[0])
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// wayland drives a Wayland session: screenshots with grim, input with
// ydotool (its daemon, ydotoold, must be running).
type wayland struct {
	run  runFunc
	look func(string) (string, error)
}

func newWayland() *wayland { return &wayland{run: execRun, look: exec.LookPath} }

func (b *wayland) Name() string                                { return "linux (Wayland)" }
func (b *wayland) Start(context.Context) error                 { return nil }
func (b *wayland) Close()                                      {}
func (b *wayland) InputSize(context.Context) (int, int, error) { return 0, 0, nil }

func (b *wayland) Screenshot(ctx context.Context) ([]byte, error) {
	return b.run(ctx, nil, cmd{name: "grim", args: []string{"-t", "png", "-"}})
}

var ydoButtons = map[string]int{"left": 0, "right": 1, "middle": 2}

// waylandCommands is the ydotool command line for an action. ydotool's
// click codes put the button in the low bits, 0x40 for down, 0x80 for up.
func waylandCommands(a action) ([]cmd, error) {
	ydo := func(args ...string) cmd { return cmd{name: "ydotool", args: args} }
	move := func(x, y int) cmd {
		return ydo("mousemove", "--absolute", "-x", strconv.Itoa(x), "-y", strconv.Itoa(y))
	}
	var mods []int
	for _, m := range a.mods {
		mods = append(mods, evdevMods[m])
	}
	wrap := func(cmds ...cmd) []cmd {
		if len(mods) == 0 {
			return cmds
		}
		return append(append([]cmd{ydo(append([]string{"key"}, ydoKeyArgs(mods, true, false)...)...)}, cmds...),
			ydo(append([]string{"key"}, ydoKeyArgs(mods, false, true)...)...))
	}
	switch a.kind {
	case "move":
		return []cmd{move(a.x, a.y)}, nil
	case "click":
		btn, ok := ydoButtons[a.button]
		if !ok {
			return nil, fmt.Errorf("unknown button %q", a.button)
		}
		var cmds []cmd
		if a.at {
			cmds = append(cmds, move(a.x, a.y))
		}
		cmds = append(cmds, ydo("click", "--repeat", strconv.Itoa(max(a.count, 1)), "--next-delay", "80", fmt.Sprintf("0x%X", 0xC0|btn)))
		return wrap(cmds...), nil
	case "drag":
		return wrap(move(a.x, a.y), ydo("click", "0x40"), move((a.x+a.x2)/2, (a.y+a.y2)/2), move(a.x2, a.y2), ydo("click", "0x80")), nil
	case "down", "up":
		btn, ok := ydoButtons[a.button]
		if !ok {
			return nil, fmt.Errorf("unknown button %q", a.button)
		}
		code := 0x40
		if a.kind == "up" {
			code = 0x80
		}
		return []cmd{ydo("click", fmt.Sprintf("0x%X", code|btn))}, nil
	case "type":
		return []cmd{ydo("type", "--", a.text)}, nil
	case "key", "hold":
		codes, err := evdevCodes(a.keys)
		if err != nil {
			return nil, err
		}
		if a.kind == "hold" {
			return []cmd{
				{name: "ydotool", args: append([]string{"key"}, ydoKeyArgs(codes, true, false)...), sleep: a.dur},
				ydo(append([]string{"key"}, ydoKeyArgs(codes, false, true)...)...),
			}, nil
		}
		var args []string
		for range max(a.repeat, 1) {
			args = append(args, ydoKeyArgs(codes, true, true)...)
		}
		return []cmd{ydo(append([]string{"key"}, args...)...)}, nil
	case "scroll":
		if a.dx == 0 && a.dy == 0 {
			return nil, errors.New("scroll by zero")
		}
		var cmds []cmd
		if a.at {
			cmds = append(cmds, move(a.x, a.y))
		}
		// Wheel values follow the kernel: positive y scrolls up.
		cmds = append(cmds, ydo("mousemove", "--wheel", "-x", strconv.Itoa(a.dx), "-y", strconv.Itoa(-a.dy)))
		return wrap(cmds...), nil
	}
	return nil, fmt.Errorf("unknown action %q", a.kind)
}

func (b *wayland) Do(ctx context.Context, a action) error {
	if a.kind == "open" {
		c, err := linuxOpen(a.text, b.look)
		if err != nil {
			return err
		}
		_, err = b.run(ctx, nil, c)
		return err
	}
	cmds, err := waylandCommands(a)
	if err != nil {
		return err
	}
	return runAll(ctx, b.run, nil, cmds, a.kind == "hold")
}

func (b *wayland) Cursor(context.Context) (int, int, error) {
	return 0, 0, errors.New("the pointer position cannot be read on Wayland")
}

func (b *wayland) Front(context.Context) (string, string, error) {
	return "", "", errors.New("the app in front cannot be read on Wayland")
}
