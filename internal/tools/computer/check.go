package computer

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/config"
)

// nativeFamily matches Claude model ids of a family and major version
// that take the native computer-use toolset (computer_toolset_20260801):
// Claude 5 and later.
var nativeFamily = regexp.MustCompile(`^claude-(opus|sonnet|haiku|fable|mythos)-(\d+)(?:-|$)`)

// NativeModel reports whether an Anthropic model id takes the native
// computer-use toolset. Older models keep the computer_* tools.
func NativeModel(model string) bool {
	if model == "claude-mythos-preview" {
		return true
	}
	m := nativeFamily.FindStringSubmatch(model)
	if m == nil {
		return false
	}
	major, err := strconv.Atoi(m[2])
	return err == nil && major >= 5 && major < 100 // not a date stamp
}

// Check is one line of hk doctor's report.
type Check struct {
	What   string
	OK     bool
	Detail string
}

var lookPath = exec.LookPath

func have(names ...string) string {
	for _, n := range names {
		if _, err := lookPath(n); err == nil {
			return n
		}
	}
	return ""
}

// Doctor checks the commands the configured backend needs.
func Doctor(cfg config.Computer) []Check {
	var out []Check
	need := func(what string, hint string, names ...string) {
		if n := have(names...); n != "" {
			out = append(out, Check{What: what, OK: true, Detail: n})
		} else {
			out = append(out, Check{What: what, Detail: "MISSING: " + hint})
		}
	}
	optional := func(what, hint string, names ...string) {
		if n := have(names...); n != "" {
			out = append(out, Check{What: what, OK: true, Detail: n})
		} else {
			out = append(out, Check{What: what, OK: true, Detail: "none (optional: " + hint + ")"})
		}
	}
	switch cfg.Backend {
	case "macos":
		need("screenshots", "screencapture ships with macOS", "screencapture")
		need("input", "osascript ships with macOS", "osascript")
		optional("faster input", "brew install cliclick", "cliclick")
		out = append(out, Check{What: "permissions", OK: true,
			Detail: "grant the app running Habakkuk Screen Recording and Accessibility in System Settings > Privacy & Security"})
	case "xvfb":
		need("virtual display", "apt install xvfb", "Xvfb")
		need("screenshots", "apt install imagemagick (or scrot, or ffmpeg)", shotTools...)
		need("input", "apt install xdotool", "xdotool")
		optional("window manager", "apt install openbox", windowManagers...)
		optional("watching", "apt install x11vnc novnc", "x11vnc")
	default:
		display := cfg.Display
		if display == "" {
			display = getenv("DISPLAY")
		}
		if display == "" && getenv("WAYLAND_DISPLAY") != "" {
			need("screenshots (Wayland)", "apt install grim", "grim")
			need("input (Wayland)", "apt install ydotool, and run ydotoold", "ydotool")
			break
		}
		if display == "" {
			out = append(out, Check{What: "display", Detail: "MISSING: no DISPLAY; set computer.display or use backend xvfb"})
		} else {
			out = append(out, Check{What: "display", OK: true, Detail: display})
		}
		need("screenshots", "apt install imagemagick (or scrot, or ffmpeg)", shotTools...)
		need("input", "apt install xdotool", "xdotool")
		optional("opening URLs", "apt install xdg-utils", "xdg-open")
	}
	return out
}

// View explains how to watch the agents' desktops.
func View(cfg config.Computer, home string) string {
	if cfg.Backend != "xvfb" {
		return fmt.Sprintf("The computer backend is %s: agents drive this machine's own screen, so watch it directly.\n", cfg.Backend)
	}
	running := RunningDesktops(filepath.Join(home, "computer"))
	if len(running) == 0 {
		return "No virtual desktops are running. An agent's first computer action starts one (under hk serve, chat or run).\n"
	}
	var s strings.Builder
	vnc, novnc := have("x11vnc"), have("novnc_proxy", "websockify")
	for _, st := range running {
		n, _ := displayNumber(st.Display)
		port := 5900 + n
		fmt.Fprintf(&s, "agent %s: virtual desktop %s (%s), Xvfb pid %d\n", st.Agent, st.Display, st.Screen, st.PID)
		fmt.Fprintf(&s, "  serve it over VNC on loopback (view only; drop -viewonly to take over):\n")
		fmt.Fprintf(&s, "    x11vnc -display %s -auth %s -localhost -viewonly -shared -forever -nopw -rfbport %d\n", st.Display, st.Auth, port)
		fmt.Fprintf(&s, "  then open a VNC viewer on 127.0.0.1:%d (from another machine: ssh -L %d:127.0.0.1:%d this-host)\n", port, port, port)
		switch novnc {
		case "novnc_proxy":
			fmt.Fprintf(&s, "  or in a browser: novnc_proxy --vnc 127.0.0.1:%d --listen 127.0.0.1:6080, then http://127.0.0.1:6080/vnc.html\n", port)
		case "websockify":
			fmt.Fprintf(&s, "  or in a browser: websockify --web /usr/share/novnc 127.0.0.1:6080 127.0.0.1:%d, then http://127.0.0.1:6080/vnc.html\n", port)
		}
	}
	if vnc == "" {
		s.WriteString("x11vnc is not installed: apt install x11vnc (and novnc for a browser view).\n")
	}
	return s.String()
}
