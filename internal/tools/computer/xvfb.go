package computer

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/platform"
)

// windowManagers are lightweight window managers started on a virtual
// desktop when one is installed, so windows get frames and focus.
var windowManagers = []string{"openbox", "fluxbox", "icewm", "jwm", "matchbox-window-manager", "xfwm4", "twm"}

// displays hands out virtual display numbers within this process, so two
// agents starting at once never pick the same one.
var displays struct {
	sync.Mutex
	used map[int]bool
}

// xvfb is a private virtual desktop for one agent: an Xvfb server on its
// own display, readable only with a cookie Habakkuk writes, with a window
// manager when one is installed. It is driven like any X display.
type xvfb struct {
	*x11
	agent  string
	dir    string // $HABAKKUK_HOME/computer
	base   int
	w, h   int
	log    *slog.Logger
	server *exec.Cmd
	wm     *exec.Cmd
	num    int
	exited chan struct{}
}

// State is what hk computer view reads about a running virtual
// desktop.
type State struct {
	Agent   string `json:"agent"`
	Display string `json:"display"`
	Screen  string `json:"screen"`
	PID     int    `json:"pid"`
	Auth    string `json:"auth"` // the XAUTHORITY file
}

func newXvfb(agent, dir string, base, w, h int, log *slog.Logger) *xvfb {
	return &xvfb{x11: newX11("", nil), agent: agent, dir: dir, base: base, w: w, h: h, log: log}
}

func (b *xvfb) Name() string { return "xvfb (" + b.display + ")" }

func (b *xvfb) running() bool {
	if b.server == nil {
		return false
	}
	select {
	case <-b.exited:
		return false
	default:
		return true
	}
}

// Start starts the virtual desktop, or a new one when it stopped.
func (b *xvfb) Start(ctx context.Context) error {
	if b.running() {
		return nil
	}
	b.stop()
	if _, err := exec.LookPath("Xvfb"); err != nil {
		return errors.New("the Xvfb command is not installed (apt install xvfb)")
	}
	if err := os.MkdirAll(b.dir, 0o700); err != nil {
		return err
	}
	b.killStale()
	var lastErr error
	for n := b.base; n < b.base+64; n++ {
		if !claimDisplay(n) {
			continue
		}
		err := b.startOn(ctx, n)
		if err == nil {
			return nil
		}
		releaseDisplay(n)
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return fmt.Errorf("could not start a virtual desktop: %w", lastErr)
}

// claimDisplay reserves display n unless this process or another X server
// holds it.
func claimDisplay(n int) bool {
	displays.Lock()
	defer displays.Unlock()
	if displays.used[n] {
		return false
	}
	for _, p := range []string{fmt.Sprintf("/tmp/.X%d-lock", n), fmt.Sprintf("/tmp/.X11-unix/X%d", n)} {
		if _, err := os.Lstat(p); err == nil {
			return false
		}
	}
	if displays.used == nil {
		displays.used = map[int]bool{}
	}
	displays.used[n] = true
	return true
}

func releaseDisplay(n int) {
	displays.Lock()
	delete(displays.used, n)
	displays.Unlock()
}

func (b *xvfb) startOn(ctx context.Context, n int) error {
	auth := filepath.Join(b.dir, b.agent+".xauth")
	if err := writeXauth(auth, n); err != nil {
		return err
	}
	display := ":" + strconv.Itoa(n)
	server := exec.Command("Xvfb", display, "-screen", "0", fmt.Sprintf("%dx%dx24", b.w, b.h), "-nolisten", "tcp", "-auth", auth)
	platform.SetProcessGroup(server)
	var stderr bytes.Buffer
	server.Stderr = &stderr
	if err := server.Start(); err != nil {
		return err
	}
	exited := make(chan struct{})
	go func() { _ = server.Wait(); close(exited) }()
	if err := waitForX(ctx, n, exited); err != nil {
		_ = platform.KillGroup(server.Process.Pid)
		<-exited
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			err = fmt.Errorf("%w: %s", err, lastLine(msg))
		}
		return err
	}
	b.server, b.exited, b.num = server, exited, n
	b.display = display
	b.env = []string{"DISPLAY=" + display, "XAUTHORITY=" + auth}
	b.startWM()
	st := State{Agent: b.agent, Display: display, Screen: fmt.Sprintf("%dx%d", b.w, b.h), PID: server.Process.Pid, Auth: auth}
	if data, err := json.Marshal(st); err == nil {
		_ = os.WriteFile(b.statePath(), data, 0o600)
	}
	b.log.Info("virtual desktop started", "agent", b.agent, "display", display, "screen", st.Screen)
	return nil
}

// waitForX waits until the server accepts connections on its socket.
func waitForX(ctx context.Context, n int, exited <-chan struct{}) error {
	sock := fmt.Sprintf("/tmp/.X11-unix/X%d", n)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			return fmt.Errorf("the virtual display :%d exited at start", n)
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if c, err := net.DialTimeout("unix", sock, 200*time.Millisecond); err == nil {
			_ = c.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("the virtual display :%d did not come up", n)
}

func (b *xvfb) startWM() {
	for _, name := range windowManagers {
		if _, err := exec.LookPath(name); err != nil {
			continue
		}
		wm := exec.Command(name)
		wm.Env = append(os.Environ(), b.env...)
		platform.SetProcessGroup(wm)
		if err := wm.Start(); err != nil {
			continue
		}
		go func() { _ = wm.Wait() }()
		b.wm = wm
		return
	}
}

func (b *xvfb) statePath() string { return filepath.Join(b.dir, b.agent+".json") }

// killStale stops a virtual desktop an earlier Habakkuk left running (it was
// killed before it could clean up).
func (b *xvfb) killStale() {
	data, err := os.ReadFile(b.statePath())
	if err != nil {
		return
	}
	var st State
	if json.Unmarshal(data, &st) == nil && st.PID > 0 && isXvfb(st.PID) {
		_ = platform.KillGroup(st.PID)
	}
	_ = os.Remove(b.statePath())
}

// isXvfb reports whether pid is an Xvfb process (Linux only; elsewhere it
// errs toward leaving the process alone).
func isXvfb(pid int) bool {
	comm, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
	return err == nil && strings.TrimSpace(string(comm)) == "Xvfb"
}

func (b *xvfb) stop() {
	if b.wm != nil && b.wm.Process != nil {
		_ = platform.KillGroup(b.wm.Process.Pid)
		b.wm = nil
	}
	if b.server != nil && b.server.Process != nil {
		_ = platform.TerminateGroup(b.server.Process.Pid)
		select {
		case <-b.exited:
		case <-time.After(2 * time.Second):
			_ = platform.KillGroup(b.server.Process.Pid)
			<-b.exited
		}
		b.server = nil
		releaseDisplay(b.num)
		_ = os.Remove(b.statePath())
	}
}

func (b *xvfb) Close() {
	b.stop()
	_ = os.Remove(filepath.Join(b.dir, b.agent+".xauth"))
}

// writeXauth writes an Xauthority file holding one random
// MIT-MAGIC-COOKIE-1 for display n, valid from any host name (family
// FamilyWild), so only processes given the file can use the display.
func writeXauth(path string, n int) error {
	cookie := make([]byte, 16)
	if _, err := rand.Read(cookie); err != nil {
		return err
	}
	var buf bytes.Buffer
	field := func(b []byte) {
		_ = binary.Write(&buf, binary.BigEndian, uint16(len(b)))
		buf.Write(b)
	}
	_ = binary.Write(&buf, binary.BigEndian, uint16(0xFFFF)) // FamilyWild
	field(nil)
	field([]byte(strconv.Itoa(n)))
	field([]byte("MIT-MAGIC-COOKIE-1"))
	field(cookie)
	return os.WriteFile(path, buf.Bytes(), 0o600)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// RunningDesktops lists the virtual desktops recorded under dir
// ($HABAKKUK_HOME/computer) whose server is still alive.
func RunningDesktops(dir string) []State {
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	var out []State
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var st State
		if json.Unmarshal(data, &st) != nil || st.PID <= 0 || !platform.Alive(st.PID) {
			continue
		}
		out = append(out, st)
	}
	return out
}
