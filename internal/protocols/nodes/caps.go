package nodes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// Capability is something a node can do when asked.
type Capability interface {
	Run(ctx context.Context, args json.RawMessage) (*Result, error)
}

// Func adapts a function to Capability.
type Func func(ctx context.Context, args json.RawMessage) (*Result, error)

func (f Func) Run(ctx context.Context, args json.RawMessage) (*Result, error) { return f(ctx, args) }

// Caps are a node's capabilities by command name.
type Caps map[string]Capability

// Names lists the commands, sorted.
func (c Caps) Names() []string {
	out := make([]string, 0, len(c))
	for n := range c {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// LocalOptions says what a `hk node run` device may do beyond the safe
// defaults.
type LocalOptions struct {
	AllowRun    bool   // offer system.run (a shell)
	AllowCamera bool   // offer camera.snap
	Dir         string // files.get and files.put work inside this directory; "" offers neither
	// Protected are folders files.put never writes, however reached (a
	// link, another letter case): the Habakkuk homes on this machine.
	Protected []string
}

// The platform and tool lookup, replaceable in tests.
var (
	goos     = runtime.GOOS
	lookPath = exec.LookPath
	getenv   = os.Getenv
)

func have(name string) bool { _, err := lookPath(name); return err == nil }

// LocalCaps offers what this machine can do: each capability only when the
// tools it needs are installed, and the risky ones only when allowed.
func LocalCaps(o LocalOptions) Caps {
	c := Caps{}
	if o.AllowRun {
		c[CmdRun] = Func(runShell)
	}
	if n := notifier(); n != nil {
		c[CmdNotify] = n
	}
	if s := screenshotter(); s != nil {
		c[CmdScreen] = s
	}
	if o.AllowCamera && have("ffmpeg") && (goos == "darwin" || goos == "linux") {
		c[CmdCamera] = Func(cameraSnap)
	}
	if l := locator(); l != nil {
		c[CmdLocation] = l
	}
	if r, w := clipboard(); r != nil {
		c[CmdClipRead], c[CmdClipWrite] = r, w
	}
	if o.Dir != "" {
		root := o.Dir
		c[CmdFilesGet] = Func(func(ctx context.Context, args json.RawMessage) (*Result, error) { return filesGet(root, args) })
		protected := o.Protected
		c[CmdFilesPut] = Func(func(ctx context.Context, args json.RawMessage) (*Result, error) {
			return filesPut(root, protected, args)
		})
	}
	return c
}

// run runs a program and returns its standard output, with standard error
// in the error when it fails.
func run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 500 {
			msg = msg[:500]
		}
		if msg != "" {
			return out, fmt.Errorf("%s: %w: %s", name, err, msg)
		}
		return out, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

func decode(args json.RawMessage, v any) error {
	if len(args) == 0 {
		return nil
	}
	if err := json.Unmarshal(args, v); err != nil {
		return fmt.Errorf("invalid args: %w", err)
	}
	return nil
}

// system.run

func runShell(ctx context.Context, args json.RawMessage) (*Result, error) {
	var in struct {
		Command string `json:"command"`
		Cwd     string `json:"cwd"`
		Timeout int    `json:"timeout_seconds"`
	}
	if err := decode(args, &in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Command) == "" {
		return nil, errors.New(`args.command is required`)
	}
	timeout := time.Minute
	if in.Timeout > 0 {
		timeout = min(time.Duration(in.Timeout)*time.Second, 10*time.Minute)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var cmd *exec.Cmd
	if goos == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", in.Command)
	} else {
		cmd = exec.CommandContext(ctx, "/bin/sh", "-c", in.Command)
	}
	cmd.Dir = in.Cwd
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	text := out.String()
	if len(text) > MaxText {
		text = text[:MaxText/2] + "\n[... output cut ...]\n" + text[len(text)-MaxText/2:]
	}
	code := 0
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		code = exit.ExitCode()
	case ctx.Err() != nil:
		return &Result{Text: text + "\n[timed out after " + timeout.String() + "]"}, nil
	case err != nil:
		return nil, err
	}
	return &Result{Text: fmt.Sprintf("%s\n[exit code %d]", strings.TrimRight(text, "\n"), code)}, nil
}

// system.notify

func notifier() Capability {
	switch {
	case goos == "darwin" && have("osascript"):
		return Func(func(ctx context.Context, args json.RawMessage) (*Result, error) {
			title, msg, err := notifyArgs(args)
			if err != nil {
				return nil, err
			}
			// The text goes in as arguments, never into the script itself.
			_, err = run(ctx, nil, "osascript", "-e", "on run argv", "-e",
				"display notification (item 2 of argv) with title (item 1 of argv)", "-e", "end run", title, msg)
			if err != nil {
				return nil, err
			}
			return &Result{Text: "notification shown"}, nil
		})
	case goos == "linux" && have("notify-send"):
		return Func(func(ctx context.Context, args json.RawMessage) (*Result, error) {
			title, msg, err := notifyArgs(args)
			if err != nil {
				return nil, err
			}
			if _, err := run(ctx, nil, "notify-send", "--app-name=Habakkuk", "--", title, msg); err != nil {
				return nil, err
			}
			return &Result{Text: "notification shown"}, nil
		})
	}
	return nil
}

func notifyArgs(args json.RawMessage) (title, msg string, err error) {
	var in struct {
		Title   string `json:"title"`
		Message string `json:"message"`
	}
	if err := decode(args, &in); err != nil {
		return "", "", err
	}
	if in.Message == "" {
		return "", "", errors.New("args.message is required")
	}
	if in.Title == "" {
		in.Title = "Habakkuk"
	}
	return in.Title, in.Message, nil
}

// screen.snapshot

func screenshotter() Capability {
	var argv func(path string) []string
	switch {
	case goos == "darwin" && have("screencapture"):
		argv = func(p string) []string { return []string{"screencapture", "-x", "-t", "png", p} }
	case goos == "linux" && getenv("WAYLAND_DISPLAY") != "" && have("grim"):
		argv = func(p string) []string { return []string{"grim", p} }
	case goos == "linux" && have("import"):
		argv = func(p string) []string { return []string{"import", "-window", "root", p} }
	case goos == "linux" && have("grim"):
		argv = func(p string) []string { return []string{"grim", p} }
	default:
		return nil
	}
	return Func(func(ctx context.Context, _ json.RawMessage) (*Result, error) {
		return captureFile(ctx, "screen.png", "image/png", argv)
	})
}

// captureFile runs a program that writes a file and returns the file.
func captureFile(ctx context.Context, name, mediaType string, argv func(path string) []string) (*Result, error) {
	dir, err := os.MkdirTemp("", "habakkuk-node-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, name)
	cmd := argv(path)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := run(ctx, nil, cmd[0], cmd[1:]...); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s wrote nothing (on macOS, allow screen or camera access for the terminal running habakkuk): %w", cmd[0], err)
	}
	if len(data) > MaxFile {
		return nil, fmt.Errorf("%s is %d MB, over the %d MB limit", name, len(data)>>20, MaxFile>>20)
	}
	return &Result{Text: fmt.Sprintf("captured %s (%d KB)", name, len(data)>>10),
		Files: []File{{Name: name, MediaType: mediaType, Data: data}}}, nil
}

// camera.snap

func cameraSnap(ctx context.Context, args json.RawMessage) (*Result, error) {
	var in struct {
		Device string `json:"device"`
	}
	if err := decode(args, &in); err != nil {
		return nil, err
	}
	return captureFile(ctx, "camera.jpg", "image/jpeg", func(p string) []string {
		base := []string{"ffmpeg", "-hide_banner", "-loglevel", "error", "-y"}
		if goos == "darwin" {
			dev := in.Device
			if dev == "" {
				dev = "0"
			}
			return append(base, "-f", "avfoundation", "-framerate", "30", "-i", dev, "-frames:v", "1", p)
		}
		dev := in.Device
		if dev == "" {
			dev = "/dev/video0"
		}
		return append(base, "-f", "v4l2", "-i", dev, "-frames:v", "1", p)
	})
}

// location.get: CoreLocationCLI on macOS; on Linux GeoClue through its
// where-am-i client. GeoClue ties a client to the D-Bus connection that
// created it, so a chain of one-shot gdbus calls cannot drive it; its own
// demo client holds the connection for the whole request.
func locator() Capability {
	switch goos {
	case "darwin":
		if !have("CoreLocationCLI") {
			return nil
		}
		return Func(func(ctx context.Context, _ json.RawMessage) (*Result, error) {
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			var lastErr error
			for _, args := range [][]string{{"-once", "-json"}, {"-json"}, {"-once", "-format", "%latitude, %longitude (accuracy %h_accuracy m)"}} {
				out, err := run(ctx, nil, "CoreLocationCLI", args...)
				if err == nil && len(bytes.TrimSpace(out)) > 0 {
					return &Result{Text: strings.TrimSpace(string(out))}, nil
				}
				lastErr = err
			}
			return nil, fmt.Errorf("CoreLocationCLI found no location (allow Location Services for it): %v", lastErr)
		})
	case "linux":
		for _, p := range []string{"/usr/libexec/geoclue-2.0/demos/where-am-i", "/usr/lib/geoclue-2.0/demos/where-am-i"} {
			if _, err := os.Stat(p); err == nil {
				bin := p
				return Func(func(ctx context.Context, _ json.RawMessage) (*Result, error) {
					ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
					defer cancel()
					out, err := run(ctx, nil, bin, "-t", "30")
					if text := geoclueText(string(out)); text != "" {
						return &Result{Text: text}, nil
					}
					if err != nil {
						return nil, fmt.Errorf("GeoClue: %w", err)
					}
					return nil, errors.New("GeoClue found no location")
				})
			}
		}
	}
	return nil
}

// geoclueText keeps the coordinate lines of where-am-i's output.
func geoclueText(out string) string {
	var keep []string
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		for _, p := range []string{"Latitude:", "Longitude:", "Accuracy:", "Altitude:", "Timestamp:"} {
			if strings.HasPrefix(line, p) {
				keep = append(keep, line)
			}
		}
	}
	return strings.Join(keep, "\n")
}

// clipboard.read and clipboard.write

func clipboard() (read, write Capability) {
	var get, put []string
	switch {
	case goos == "darwin" && have("pbpaste") && have("pbcopy"):
		get, put = []string{"pbpaste"}, []string{"pbcopy"}
	case goos == "linux" && getenv("WAYLAND_DISPLAY") != "" && have("wl-paste") && have("wl-copy"):
		get, put = []string{"wl-paste", "--no-newline"}, []string{"wl-copy"}
	case goos == "linux" && have("xclip"):
		get, put = []string{"xclip", "-selection", "clipboard", "-o"}, []string{"xclip", "-selection", "clipboard", "-i"}
	case goos == "linux" && have("xsel"):
		get, put = []string{"xsel", "--clipboard", "--output"}, []string{"xsel", "--clipboard", "--input"}
	default:
		return nil, nil
	}
	read = Func(func(ctx context.Context, _ json.RawMessage) (*Result, error) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		out, err := run(ctx, nil, get[0], get[1:]...)
		if err != nil {
			return nil, err
		}
		if len(out) > MaxText {
			out = append(out[:MaxText], "\n[... cut ...]"...)
		}
		return &Result{Text: string(out)}, nil
	})
	write = Func(func(ctx context.Context, args json.RawMessage) (*Result, error) {
		var in struct {
			Text string `json:"text"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if _, err := run(ctx, []byte(in.Text), put[0], put[1:]...); err != nil {
			return nil, err
		}
		return &Result{Text: fmt.Sprintf("clipboard set (%d characters)", len([]rune(in.Text)))}, nil
	})
	return read, write
}

// files.get and files.put

// inside resolves a path the server sent against root and refuses anything
// that leaves it, symbolic links included.
func inside(root, p string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(absRoot); err == nil {
		absRoot = r
	}
	if p == "" {
		return "", errors.New("args.path is required")
	}
	full := p
	if !filepath.IsAbs(p) {
		full = filepath.Join(absRoot, p)
	}
	full = filepath.Clean(full)
	// Resolve the deepest part that exists, so a link anywhere on the way
	// cannot point out of the root.
	probe, rest := full, ""
	for {
		if r, err := filepath.EvalSymlinks(probe); err == nil {
			full = filepath.Join(r, rest)
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		rest = filepath.Join(filepath.Base(probe), rest)
		probe = parent
	}
	rel, err := filepath.Rel(absRoot, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside the directory this node shares", p)
	}
	return full, nil
}

func filesGet(root string, args json.RawMessage) (*Result, error) {
	var in struct {
		Path string `json:"path"`
	}
	if err := decode(args, &in); err != nil {
		return nil, err
	}
	path, err := inside(root, in.Path)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, err
		}
		var b strings.Builder
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() {
				name += "/"
			}
			b.WriteString(name + "\n")
		}
		return &Result{Text: b.String()}, nil
	}
	if st.Size() > MaxFile {
		return nil, fmt.Errorf("%s is %d MB, over the %d MB limit", in.Path, st.Size()>>20, MaxFile>>20)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return &Result{Text: fmt.Sprintf("%s (%d bytes)", in.Path, len(data)),
		Files: []File{{Name: filepath.Base(path), Data: data}}}, nil
}

// homeMarks are what a Habakkuk home holds besides its config: the agents
// and trusted-projects folders and its databases.
var homeMarks = []string{"agents", "projects", "control.db", "transcripts.db", "state.db", "web.db", "usage.db", "nodes.db", "memory.db", "history.db"}

// isHabakkukHome reports whether dir looks like a Habakkuk home: a config
// (config.yaml or config.yml) beside any of homeMarks, or an agents folder
// beside any of the others (a home whose config is missing). The node does
// not know where the gateway it serves keeps its home, and should not be
// told; recognising any home on this machine covers the gateway's too,
// whatever HABAKKUK_HOME it runs with.
func isHabakkukHome(dir string) bool {
	has := func(name string) bool { _, err := os.Stat(filepath.Join(dir, name)); return err == nil }
	config := has("config.yaml") || has("config.yml")
	agents := has("agents")
	for _, mark := range homeMarks {
		if mark == "agents" {
			if config && agents {
				return true
			}
			continue
		}
		if has(mark) && (config || agents) {
			return true
		}
	}
	return false
}

// underProtected reports whether path (links resolved) is in one of the
// protected folders, compared as folders, not as strings, or in any
// Habakkuk home on this machine.
func underProtected(path string, protected []string) bool {
	for cur := filepath.Dir(path); ; cur = filepath.Dir(cur) {
		if isHabakkukHome(cur) {
			return true
		}
		if filepath.Dir(cur) == cur {
			break
		}
	}
	var infos []os.FileInfo
	for _, d := range protected {
		if d == "" {
			continue
		}
		if real, err := filepath.EvalSymlinks(d); err == nil {
			d = real
		}
		if rel, err := filepath.Rel(d, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
		if fi, err := os.Stat(d); err == nil {
			infos = append(infos, fi)
		}
	}
	for cur := path; ; cur = filepath.Dir(cur) {
		if fi, err := os.Stat(cur); err == nil {
			for _, p := range infos {
				if os.SameFile(fi, p) {
					return true
				}
			}
		}
		if filepath.Dir(cur) == cur {
			return false
		}
	}
}

func filesPut(root string, protected []string, args json.RawMessage) (*Result, error) {
	var in struct {
		Path string `json:"path"`
		Data []byte `json:"data"` // base64
		Text string `json:"text"`
	}
	if err := decode(args, &in); err != nil {
		return nil, err
	}
	path, err := inside(root, in.Path)
	if err != nil {
		return nil, err
	}
	if underProtected(path, protected) {
		return nil, fmt.Errorf("%s is inside Habakkuk's own folder on this machine, which files.put never writes", in.Path)
	}
	data := in.Data
	if data == nil {
		data = []byte(in.Text)
	}
	if len(data) > MaxFile {
		return nil, fmt.Errorf("the file is over the %d MB limit", MaxFile>>20)
	}
	rootReal, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if r, err := filepath.EvalSymlinks(rootReal); err == nil {
		rootReal = r
	}
	rel, err := filepath.Rel(rootReal, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("%s is outside the directory this node shares", in.Path)
	}
	// Written without following a link anywhere on the way: a link put in
	// the shared folder (at a temp name, or as a folder) cannot lead the
	// write out of it (putfile_*.go). The protected check runs again just
	// before the file takes its name.
	if err := putFile(rootReal, rel, data, func() bool { return underProtected(path, protected) }); err != nil {
		return nil, err
	}
	return &Result{Text: fmt.Sprintf("wrote %s (%d bytes)", in.Path, len(data))}, nil
}
