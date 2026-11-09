// Package auth keeps subscription logins: tokens from `habakkuk auth login`
// for providers that take an account sign-in instead of an API key.
//
// Tokens live in $HABAKKUK_HOME/auth.json (mode 0600, replaced atomically).
// Refreshing takes an exclusive lock on auth.json.lock and re-reads the file
// first, so two Habakkuk processes never both spend a single-use refresh token.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
)

// Login kinds.
const (
	ChatGPT = "chatgpt"
	Copilot = "copilot"
	// MCP is an OAuth sign-in to a remote MCP server, stored as
	// "mcp:<server>" (habakkuk mcp login).
	MCP = "mcp"
)

// Kinds lists the logins `habakkuk auth login` supports.
var Kinds = []string{ChatGPT, Copilot}

// ErrNotLoggedIn means there is no stored login of that kind.
var ErrNotLoggedIn = errors.New("not logged in")

// ErrLoginExpired means the provider refused the stored refresh credential;
// only a new login helps.
var ErrLoginExpired = errors.New("login expired or revoked")

// Entry is one stored login. Fields a kind does not use stay empty.
type Entry struct {
	Kind         string    `json:"kind"`
	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"` // copilot: the GitHub OAuth token
	IDToken      string    `json:"id_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitzero"`
	AccountID    string    `json:"account_id,omitempty"` // chatgpt account
	Email        string    `json:"email,omitempty"`
	BaseURL      string    `json:"base_url,omitempty"` // copilot API endpoint
	// MCP OAuth: what a refresh needs.
	TokenURL     string    `json:"token_url,omitempty"`
	ClientID     string    `json:"client_id,omitempty"`
	ClientSecret string    `json:"client_secret,omitempty"`
	TokenAuth    string    `json:"token_auth,omitempty"` // client authentication at the token endpoint: none | post | basic
	Resource     string    `json:"resource,omitempty"`   // RFC 8707 resource indicator: the MCP server
	Scope        string    `json:"scope,omitempty"`
	Issuer       string    `json:"issuer,omitempty"`
	UpdatedAt    time.Time `json:"updated_at,omitzero"`
}

type file struct {
	Version int              `json:"version"`
	Logins  map[string]Entry `json:"logins"`
}

// Store reads and writes auth.json.
type Store struct {
	Path string
	HTTP *http.Client
	Now  func() time.Time
	// Sleep waits between device-flow polls; tests replace it.
	Sleep func(ctx context.Context, d time.Duration) error

	// Endpoints; zero values mean the real services.
	ChatGPTIssuer string // default https://auth.openai.com
	GitHubURL     string // default https://github.com
	GitHubAPIURL  string // default https://api.github.com

	mu sync.Mutex // serializes refreshes within this process
}

// NewStore returns the store at $HABAKKUK_HOME/auth.json.
func NewStore(home string) *Store {
	return &Store{Path: filepath.Join(home, "auth.json")}
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Store) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second}
}

func (s *Store) sleep(ctx context.Context, d time.Duration) error {
	if s.Sleep != nil {
		return s.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (s *Store) read() (file, error) {
	f := file{Version: 1, Logins: map[string]Entry{}}
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return f, fmt.Errorf("%s: %w", s.Path, err)
	}
	if f.Logins == nil {
		f.Logins = map[string]Entry{}
	}
	return f, nil
}

func (s *Store) write(f file) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	return store.WriteFileAtomic(s.Path, append(data, '\n'), 0o600)
}

// update runs fn on the file under the process mutex and the file lock and
// writes the result.
func (s *Store) update(ctx context.Context, fn func(f *file) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := lockFile(ctx, s.Path+".lock")
	if err != nil {
		return err
	}
	defer unlock()
	f, err := s.read()
	if err != nil {
		return err
	}
	if err := fn(&f); err != nil {
		return err
	}
	return s.write(f)
}

// List returns the stored logins by name.
func (s *Store) List() (map[string]Entry, error) {
	f, err := s.read()
	return f.Logins, err
}

// Names returns the stored login names, sorted.
func (s *Store) Names() ([]string, error) {
	f, err := s.read()
	if err != nil {
		return nil, err
	}
	var out []string
	for n := range f.Logins {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// Put stores a login, replacing any of the same name.
func (s *Store) Put(ctx context.Context, name string, e Entry) error {
	e.UpdatedAt = s.now()
	return s.update(ctx, func(f *file) error {
		f.Logins[name] = e
		return nil
	})
}

// Delete removes a login. It reports whether one was stored.
func (s *Store) Delete(ctx context.Context, name string) (bool, error) {
	found := false
	err := s.update(ctx, func(f *file) error {
		_, found = f.Logins[name]
		delete(f.Logins, name)
		return nil
	})
	return found, err
}

// refreshMargin: a token closer than this to expiry is refreshed first.
const refreshMargin = 5 * time.Minute

func (s *Store) fresh(e Entry) bool {
	return e.AccessToken != "" && (e.ExpiresAt.IsZero() || s.now().Add(refreshMargin).Before(e.ExpiresAt))
}

// Get returns a login with an access token valid for at least a few
// minutes, refreshing it first when needed.
func (s *Store) Get(ctx context.Context, name string) (Entry, error) {
	f, err := s.read()
	if err != nil {
		return Entry{}, err
	}
	e, ok := f.Logins[name]
	if !ok {
		return Entry{}, fmt.Errorf("%s: %w (run `%s`)", name, ErrNotLoggedIn, loginCommand(name))
	}
	if s.fresh(e) {
		return e, nil
	}
	err = s.update(ctx, func(f *file) error {
		// Another process may have refreshed while we waited for the lock.
		cur, ok := f.Logins[name]
		if !ok {
			return fmt.Errorf("%s: %w", name, ErrNotLoggedIn)
		}
		if s.fresh(cur) {
			e = cur
			return nil
		}
		next, err := s.refresh(ctx, cur)
		if err != nil {
			return err
		}
		next.UpdatedAt = s.now()
		f.Logins[name] = next
		e = next
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrLoginExpired) {
			return Entry{}, fmt.Errorf("%s: %w (run `%s` again)", name, err, loginCommand(name))
		}
		return Entry{}, err
	}
	return e, nil
}

// Token returns a valid access token for a login.
func (s *Store) Token(ctx context.Context, name string) (string, error) {
	e, err := s.Get(ctx, name)
	return e.AccessToken, err
}

func (s *Store) refresh(ctx context.Context, e Entry) (Entry, error) {
	switch e.Kind {
	case ChatGPT:
		return s.refreshChatGPT(ctx, e)
	case Copilot:
		return s.refreshCopilot(ctx, e)
	case MCP:
		return s.refreshMCP(ctx, e)
	}
	return e, fmt.Errorf("unknown login kind %q", e.Kind)
}

// loginCommand is the command that signs in again for a stored login name.
func loginCommand(name string) string {
	if server, ok := strings.CutPrefix(name, MCP+":"); ok {
		return "habakkuk mcp login " + server
	}
	return "habakkuk auth login " + name
}

// DeviceCode is what the user needs to finish a device login in a browser.
type DeviceCode struct {
	URL       string
	Code      string
	ExpiresIn time.Duration
}

// Login runs the device flow for a kind and stores the result under name.
// show is called once with the page to open and the code to enter.
func (s *Store) Login(ctx context.Context, kind, name string, show func(DeviceCode)) (Entry, error) {
	var e Entry
	var err error
	switch kind {
	case ChatGPT:
		e, err = s.loginChatGPT(ctx, show)
	case Copilot:
		e, err = s.loginCopilot(ctx, show)
	default:
		return Entry{}, fmt.Errorf("unknown login %q (want one of %v)", kind, Kinds)
	}
	if err != nil {
		return Entry{}, err
	}
	if err := s.Put(ctx, name, e); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// httpError reads a failed response into an error that never includes
// request credentials, only the provider's own message.
type httpError struct {
	Status int
	Body   string
}

func (e *httpError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body) }

func readHTTPError(resp *http.Response) *httpError {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return &httpError{Status: resp.StatusCode, Body: string(data)}
}

func decodeJSON(resp *http.Response, v any) error {
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
}
