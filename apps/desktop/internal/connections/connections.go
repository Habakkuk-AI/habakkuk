// Package connections keeps the desktop app's list of Habakkuk servers it can
// connect to, which one is active, and the app's own settings, in one JSON
// file. Tokens never go into that file: they live in a Secrets store (the
// OS keychain in the app).
package connections

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// Kinds of connection.
const (
	Local  = "local"  // a habakkuk serve on this computer, started by the app or already running
	Remote = "remote" // any Habakkuk reachable over HTTP(S), with a token
)

// DefaultHotkey opens the quick ask window.
const DefaultHotkey = "CmdOrCtrl+Shift+Space"

// Connection is one Habakkuk the app can show.
type Connection struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	// URL is a remote's base URL, e.g. https://habakkuk.example.com; the
	// dashboard is at URL/app/.
	URL string `json:"url,omitempty"`
	// Home is a local connection's HABAKKUK_HOME; empty means $HABAKKUK_HOME or
	// ~/.habakkuk.
	Home string `json:"home,omitempty"`
	// Binary is the habakkuk executable a local connection runs; empty means
	// the one bundled with the app, else the first found on the usual paths.
	Binary string `json:"binary,omitempty"`
	// Manual leaves a local habakkuk serve alone when the connection is
	// activated; the user starts it from the tray or the connections page.
	Manual bool `json:"manual,omitempty"`
}

// Settings are the app's own preferences.
type Settings struct {
	Hotkey       string `json:"hotkey"`
	StartAtLogin bool   `json:"start_at_login"`
	// Quiet turns notifications off.
	Quiet bool `json:"quiet,omitempty"`
	// Port is the loopback port the app serves the dashboard on. It is kept
	// so the page's own preferences (theme, chat list), which the browser
	// stores per origin, survive a restart. 0 picks one.
	Port int `json:"port,omitempty"`
}

type file struct {
	Version     int          `json:"version"`
	Active      string       `json:"active"`
	Connections []Connection `json:"connections"`
	Settings    Settings     `json:"settings"`
}

// Secrets stores tokens by connection ID.
type Secrets interface {
	Get(id string) (string, error) // ErrNoSecret when there is none
	Set(id, secret string) error
	Delete(id string) error // nil when there was none
}

// ErrNoSecret is what Secrets.Get returns for an ID with no token.
var ErrNoSecret = errors.New("no token stored")

// ErrNotFound is returned for an unknown connection ID.
var ErrNotFound = errors.New("no such connection")

// Manager reads and changes the file. It is safe for concurrent use; every
// change is written through at once.
type Manager struct {
	path    string
	secrets Secrets

	mu sync.Mutex
	f  file
}

// DefaultPath is where the app keeps its file: $HABAKKUK_DESKTOP_CONFIG, or
// config.json in the OS's per-user config folder.
func DefaultPath() (string, error) {
	if p := os.Getenv("HABAKKUK_DESKTOP_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "Habakkuk Desktop", "config.json"), nil
}

// Open loads path, or starts a new file with one local connection (this
// computer's default HABAKKUK_HOME) when it does not exist yet.
func Open(path string, secrets Secrets) (*Manager, error) {
	m := &Manager{path: path, secrets: secrets}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		local := Connection{ID: newID(), Name: "This computer", Kind: Local}
		m.f = file{Version: 1, Active: local.ID, Connections: []Connection{local}}
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(data, &m.f); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	if m.f.Settings.Hotkey == "" {
		m.f.Settings.Hotkey = DefaultHotkey
	}
	if m.find(m.f.Active) < 0 {
		m.f.Active = ""
	}
	return m, nil
}

// List returns the connections in the order they were added.
func (m *Manager) List() []Connection {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.f.Connections)
}

// Get returns one connection.
func (m *Manager) Get(id string) (Connection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.find(id)
	if i < 0 {
		return Connection{}, ErrNotFound
	}
	return m.f.Connections[i], nil
}

// Active returns the connection the app shows, and false when there is none.
func (m *Manager) Active() (Connection, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.find(m.f.Active)
	if i < 0 {
		return Connection{}, false
	}
	return m.f.Connections[i], true
}

// Activate makes id the active connection.
func (m *Manager) Activate(id string) (Connection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.find(id)
	if i < 0 {
		return Connection{}, ErrNotFound
	}
	prev := m.f.Active
	m.f.Active = id
	if err := m.write(); err != nil {
		m.f.Active = prev
		return Connection{}, err
	}
	return m.f.Connections[i], nil
}

// Save adds c (when its ID is empty or unknown) or replaces the connection
// with its ID, and returns it as stored. A non-nil token replaces the stored
// one ("" removes it); nil keeps it. The first connection saved becomes the
// active one.
func (m *Manager) Save(c Connection, token *string) (Connection, error) {
	c, err := normalize(c)
	if err != nil {
		return Connection{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	old := slices.Clone(m.f.Connections)
	oldActive := m.f.Active
	if c.ID == "" {
		c.ID = newID()
	}
	if i := m.find(c.ID); i >= 0 {
		m.f.Connections[i] = c
	} else {
		m.f.Connections = append(m.f.Connections, c)
	}
	if m.f.Active == "" {
		m.f.Active = c.ID
	}
	if token != nil && m.secrets != nil {
		var err error
		if *token == "" {
			err = m.secrets.Delete(c.ID)
		} else {
			err = m.secrets.Set(c.ID, *token)
		}
		if err != nil {
			m.f.Connections, m.f.Active = old, oldActive
			return Connection{}, fmt.Errorf("keychain: %w", err)
		}
	}
	if err := m.write(); err != nil {
		m.f.Connections, m.f.Active = old, oldActive
		return Connection{}, err
	}
	return c, nil
}

// Delete removes a connection and its token. Deleting the active connection
// makes the first remaining one active.
func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.find(id)
	if i < 0 {
		return ErrNotFound
	}
	old := slices.Clone(m.f.Connections)
	oldActive := m.f.Active
	m.f.Connections = slices.Delete(m.f.Connections, i, i+1)
	if m.f.Active == id {
		m.f.Active = ""
		if len(m.f.Connections) > 0 {
			m.f.Active = m.f.Connections[0].ID
		}
	}
	if err := m.write(); err != nil {
		m.f.Connections, m.f.Active = old, oldActive
		return err
	}
	if m.secrets != nil {
		if err := m.secrets.Delete(id); err != nil {
			return fmt.Errorf("keychain: %w", err)
		}
	}
	return nil
}

// Token returns a connection's stored token, "" when there is none.
func (m *Manager) Token(id string) (string, error) {
	if m.secrets == nil {
		return "", nil
	}
	tok, err := m.secrets.Get(id)
	if errors.Is(err, ErrNoSecret) {
		return "", nil
	}
	return tok, err
}

// HasToken reports whether a token is stored for id, without returning it.
func (m *Manager) HasToken(id string) bool {
	tok, err := m.Token(id)
	return err == nil && tok != ""
}

// Settings returns the app settings.
func (m *Manager) Settings() Settings {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.f.Settings
}

// UpdateSettings changes the settings with fn and writes them.
func (m *Manager) UpdateSettings(fn func(*Settings)) (Settings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.f.Settings
	fn(&s)
	if strings.TrimSpace(s.Hotkey) == "" {
		s.Hotkey = DefaultHotkey
	}
	if s.Port < 0 || s.Port > 65535 {
		return m.f.Settings, fmt.Errorf("port %d is out of range", s.Port)
	}
	prev := m.f.Settings
	m.f.Settings = s
	if err := m.write(); err != nil {
		m.f.Settings = prev
		return prev, err
	}
	return s, nil
}

func (m *Manager) find(id string) int {
	if id == "" {
		return -1
	}
	return slices.IndexFunc(m.f.Connections, func(c Connection) bool { return c.ID == id })
}

// write replaces the file atomically. Callers hold m.mu.
func (m *Manager) write() error {
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m.f, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(m.path), ".config-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), m.path)
}

// normalize checks a connection and trims what can be trimmed.
func normalize(c Connection) (Connection, error) {
	c.Name = strings.TrimSpace(c.Name)
	c.URL = strings.TrimSpace(c.URL)
	c.Home = strings.TrimSpace(c.Home)
	c.Binary = strings.TrimSpace(c.Binary)
	if c.Name == "" {
		return c, errors.New("name is empty")
	}
	switch c.Kind {
	case Local:
		c.URL = ""
	case Remote:
		u, err := ParseURL(c.URL)
		if err != nil {
			return c, err
		}
		c.URL = u.String()
		c.Home, c.Binary, c.Manual = "", "", false
	default:
		return c, fmt.Errorf("kind must be %q or %q", Local, Remote)
	}
	return c, nil
}

// ParseURL checks a remote's base URL: http or https, a host, no query,
// fragment or credentials. A trailing slash and a trailing /app/ (the
// dashboard's own address, which people paste) are dropped.
func ParseURL(s string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("url: %w", err)
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, errors.New("url must start with http:// or https://")
	case u.Host == "":
		return nil, errors.New("url has no host")
	case u.User != nil:
		return nil, errors.New("url must not hold credentials; use the token field")
	case u.RawQuery != "" || u.Fragment != "":
		return nil, errors.New("url must not have a query or fragment")
	}
	u.Path = strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), "/app")
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawPath = ""
	return u, nil
}

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return "c" + hex.EncodeToString(b)
}
