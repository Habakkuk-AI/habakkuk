package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
)

// Names tried on PATH, then well-known install locations.
var (
	binaryNames = []string{
		"chromium", "chromium-browser", "google-chrome", "google-chrome-stable",
		"chrome", "chrome-headless-shell", "headless_shell",
	}
	binaryPaths = []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
		"/usr/bin/chromium", "/usr/bin/chromium-browser", "/usr/bin/google-chrome",
		"/snap/bin/chromium", "/opt/google/chrome/chrome",
	}
)

// FindBinary returns the browser to launch: path when set, else the first
// Chrome or Chromium on PATH or in a usual place.
func FindBinary(path string) (string, error) {
	if path != "" {
		p, err := exec.LookPath(path)
		if err != nil {
			return "", fmt.Errorf("browser path %q: %w", path, err)
		}
		return p, nil
	}
	for _, n := range binaryNames {
		if p, err := exec.LookPath(n); err == nil {
			return p, nil
		}
	}
	for _, p := range binaryPaths {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", errors.New("no Chrome or Chromium found; install one (e.g. apt install chromium) or set browser.path")
}

// process is a browser Habakkuk started.
type process struct {
	cmd     *exec.Cmd
	exited  chan struct{}
	pidFile string
	stderr  *tail
}

// launchArgs are the flags for a headless, quiet browser driven over a pipe.
func launchArgs(profile string, headless bool, extra []string) []string {
	args := []string{
		controlFlag,
		"--user-data-dir=" + profile,
		"--no-first-run", "--no-default-browser-check",
		"--disable-background-networking", "--disable-sync", "--disable-component-update",
		"--disable-default-apps", "--disable-dev-shm-usage", "--disable-breakpad",
		"--metrics-recording-only", "--password-store=basic", "--use-mock-keychain",
		"--mute-audio", "--window-size=1280,900",
	}
	if headless {
		args = append(args, "--headless=new", "--hide-scrollbars")
	}
	if os.Geteuid() == 0 || userNamespacesRestricted() {
		// Chrome's sandbox refuses to run as root, as in most containers,
		// and cannot start where the kernel restricts user namespaces.
		args = append(args, "--no-sandbox")
	}
	return append(append(args, extra...), "about:blank")
}

// tail keeps the end of the browser's stderr for error messages.
type tail struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := strings.Split(strings.TrimSpace(string(t.buf)), "\n")
	if len(lines) > 5 {
		lines = lines[len(lines)-5:]
	}
	return strings.Join(lines, "\n")
}

// wsURL turns cdp_url into the browser's DevTools WebSocket URL: ws:// URLs
// are used as they are, http:// ones are asked through /json/version.
func wsURL(ctx context.Context, cdpURL string) (string, error) {
	if strings.HasPrefix(cdpURL, "ws://") || strings.HasPrefix(cdpURL, "wss://") {
		return cdpURL, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(cdpURL, "/")+"/json/version", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("reaching the browser at %s (is it running with --remote-debugging-port?): %w", cdpURL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var v struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &v) != nil || v.WebSocketDebuggerURL == "" {
		return "", fmt.Errorf("%s/json/version answered %d without a webSocketDebuggerUrl: %s", cdpURL, resp.StatusCode, bytes.TrimSpace(body[:min(len(body), 200)]))
	}
	return v.WebSocketDebuggerURL, nil
}

// userNamespacesRestricted reports whether this Linux kernel stops
// unprivileged programs from creating user namespaces, which Chrome's sandbox
// needs: some distributions restrict them through AppArmor or a sysctl.
func userNamespacesRestricted() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	for path, blocked := range map[string]string{
		"/proc/sys/kernel/apparmor_restrict_unprivileged_userns": "1",
		"/proc/sys/kernel/unprivileged_userns_clone":             "0",
	} {
		if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) == blocked {
			return true
		}
	}
	return false
}
