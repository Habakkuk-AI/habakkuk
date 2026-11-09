// Package local finds out about a Habakkuk on this computer from its
// HABAKKUK_HOME: where its HTTP surface listens, the token it wants, whether a
// habakkuk serve is already running, and which habakkuk executable to run.
package local

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultListen is habakkuk serve's address when the http section names none.
const DefaultListen = "127.0.0.1:8484"

// Home returns h, or $HABAKKUK_HOME, or ~/.habakkuk, as habakkuk itself does.
func Home(h string) string {
	if h != "" {
		return expand(h)
	}
	if e := os.Getenv("HABAKKUK_HOME"); e != "" {
		return e
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".habakkuk"
	}
	return filepath.Join(home, ".habakkuk")
}

func expand(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

// HTTPConfig is the part of config.yaml the app needs.
type HTTPConfig struct {
	Listen   string `yaml:"listen"`
	TokenEnv string `yaml:"token_env"`
}

// ErrNoHTTP means config.yaml has no http section, so habakkuk serve opens no
// port and there is no dashboard to show.
var ErrNoHTTP = errors.New("config.yaml has no http section, so habakkuk serve has no dashboard; add one (see docs/interfaces/desktop-app.md)")

// ReadHTTP reads the http section of home/config.yaml.
func ReadHTTP(home string) (HTTPConfig, error) {
	data, err := os.ReadFile(filepath.Join(home, "config.yaml"))
	if err != nil {
		return HTTPConfig{}, err
	}
	var cfg struct {
		HTTP *HTTPConfig `yaml:"http"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return HTTPConfig{}, fmt.Errorf("config.yaml: %w", err)
	}
	if cfg.HTTP == nil {
		return HTTPConfig{}, ErrNoHTTP
	}
	if cfg.HTTP.Listen == "" {
		cfg.HTTP.Listen = DefaultListen
	}
	return *cfg.HTTP, nil
}

// Heartbeat is serve.json, which habakkuk serve rewrites every 15 seconds.
type Heartbeat struct {
	PID     int       `json:"pid"`
	Version string    `json:"version"`
	Started time.Time `json:"started"`
	Beat    time.Time `json:"beat"`
	Listen  string    `json:"listen,omitempty"`
}

// Alive reports whether the beat is recent enough that serve is running
// (habakkuk status uses the same 45 seconds).
func (h Heartbeat) Alive(now time.Time) bool {
	return h.PID > 0 && now.Sub(h.Beat) < 45*time.Second
}

// ReadHeartbeat reads home/serve.json.
func ReadHeartbeat(home string) (Heartbeat, error) {
	var hb Heartbeat
	data, err := os.ReadFile(filepath.Join(home, "serve.json"))
	if err != nil {
		return hb, err
	}
	return hb, json.Unmarshal(data, &hb)
}

// Endpoint is where to reach a Habakkuk and the token to send.
type Endpoint struct {
	URL   string // base URL, no trailing slash
	Token string
}

// Resolve works out the endpoint of the Habakkuk in home. addr, when known
// (the address a running serve printed), wins over serve.json, which wins
// over config.yaml. The token is the value of http.token_env from the
// environment, else from home/.env, as habakkuk serve reads it.
func Resolve(home, addr string, now time.Time) (Endpoint, error) {
	cfg, err := ReadHTTP(home)
	if err != nil {
		return Endpoint{}, err
	}
	if addr == "" {
		if hb, err := ReadHeartbeat(home); err == nil && hb.Alive(now) && hb.Listen != "" {
			addr = hb.Listen
		}
	}
	if addr == "" {
		addr = cfg.Listen
	}
	ep := Endpoint{URL: "http://" + dialable(addr)}
	if cfg.TokenEnv != "" {
		ep.Token = os.Getenv(cfg.TokenEnv)
		if ep.Token == "" {
			ep.Token = DotEnv(filepath.Join(home, ".env"))[cfg.TokenEnv]
		}
		if ep.Token == "" {
			return ep, fmt.Errorf("http.token_env names %s, which is set neither in the environment nor in %s", cfg.TokenEnv, filepath.Join(home, ".env"))
		}
	}
	return ep, nil
}

// dialable turns a listen address into one to connect to: an unspecified
// host (":8484", "0.0.0.0", "::") becomes the loopback address.
func dialable(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	return net.JoinHostPort(host, port)
}

// DotEnv reads KEY=VALUE lines the way habakkuk does.
func DotEnv(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return out
}

// FindBinary returns the habakkuk executable to run: explicit when given,
// else one bundled next to the app (Contents/MacOS or Contents/Resources in
// a macOS bundle, the same folder elsewhere), else habakkuk on PATH, else the
// places install.sh puts it.
func FindBinary(explicit string) (string, error) {
	name := "habakkuk"
	if runtime.GOOS == "windows" {
		name = "habakkuk.exe"
	}
	if explicit != "" {
		p := expand(explicit)
		if isExec(p) {
			return p, nil
		}
		return "", fmt.Errorf("%s is not an executable file", p)
	}
	var candidates []string
	if self, err := os.Executable(); err == nil {
		dir := filepath.Dir(self)
		candidates = append(candidates, filepath.Join(dir, name), filepath.Join(dir, "..", "Resources", name))
	}
	if p, err := exec.LookPath(name); err == nil {
		candidates = append(candidates, p)
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".local", "bin", name))
	}
	candidates = append(candidates, "/usr/local/bin/"+name, "/opt/homebrew/bin/"+name)
	for _, p := range candidates {
		if isExec(p) {
			return filepath.Clean(p), nil
		}
	}
	return "", errors.New("no habakkuk executable found: install it (see https://habakkuk.ai/docs/getting-started/installation) or set its path on the connection")
}

func isExec(p string) bool {
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return false
	}
	return runtime.GOOS == "windows" || fi.Mode().Perm()&0o111 != 0
}

// SearchPath returns PATH with the usual install folders added, for a child
// started from a GUI session (whose PATH on macOS is only the system's).
func SearchPath() string {
	parts := filepath.SplitList(os.Getenv("PATH"))
	extra := []string{"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"}
	if home, err := os.UserHomeDir(); err == nil {
		extra = append([]string{filepath.Join(home, ".local", "bin"), filepath.Join(home, "go", "bin")}, extra...)
	}
	if runtime.GOOS == "windows" {
		extra = nil
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range append(parts, extra...) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return strings.Join(out, string(os.PathListSeparator))
}
