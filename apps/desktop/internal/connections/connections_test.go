package connections

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// memSecrets is a Secrets in memory.
type memSecrets struct {
	mu   sync.Mutex
	m    map[string]string
	fail error
}

func newMem() *memSecrets { return &memSecrets{m: map[string]string{}} }

func (s *memSecrets) Get(id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[id]
	if !ok {
		return "", ErrNoSecret
	}
	return v, nil
}

func (s *memSecrets) Set(id, v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	s.m[id] = v
	return nil
}

func (s *memSecrets) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
	return nil
}

func ptr(s string) *string { return &s }

func TestOpenNewFileHasLocalDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.json")
	m, err := Open(path, newMem())
	if err != nil {
		t.Fatal(err)
	}
	list := m.List()
	if len(list) != 1 || list[0].Kind != Local || list[0].Name != "This computer" {
		t.Fatalf("default connections = %+v", list)
	}
	act, ok := m.Active()
	if !ok || act.ID != list[0].ID {
		t.Fatalf("active = %+v, %v", act, ok)
	}
	if m.Settings().Hotkey != DefaultHotkey {
		t.Fatalf("hotkey = %q", m.Settings().Hotkey)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("opening must not write the file yet: %v", err)
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	sec := newMem()
	m, err := Open(path, sec)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := m.Save(Connection{Name: " Work ", Kind: Remote, URL: "https://habakkuk.example.com/app/"}, ptr("s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	if remote.ID == "" || remote.Name != "Work" || remote.URL != "https://habakkuk.example.com" {
		t.Fatalf("saved = %+v", remote)
	}
	if _, err := m.UpdateSettings(func(s *Settings) { s.Hotkey = "Ctrl+Alt+F"; s.StartAtLogin = true; s.Port = 47001 }); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Activate(remote.ID); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "s3cret") {
		t.Fatal("the token was written to the config file")
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
		}
	}

	again, err := Open(path, sec)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.List(); len(got) != 2 || got[1] != remote {
		t.Fatalf("reloaded list = %+v", got)
	}
	if act, _ := again.Active(); act.ID != remote.ID {
		t.Fatalf("reloaded active = %+v", act)
	}
	if s := again.Settings(); s.Hotkey != "Ctrl+Alt+F" || !s.StartAtLogin || s.Port != 47001 {
		t.Fatalf("reloaded settings = %+v", s)
	}
	if tok, _ := again.Token(remote.ID); tok != "s3cret" {
		t.Fatalf("token = %q", tok)
	}
}

func TestSwitchingAndDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	sec := newMem()
	m, _ := Open(path, sec)
	local, _ := m.Active()
	a, _ := m.Save(Connection{Name: "A", Kind: Remote, URL: "http://10.0.0.2:8484"}, ptr("ta"))
	b, _ := m.Save(Connection{Name: "B", Kind: Remote, URL: "https://b.example.com/habakkuk"}, nil)

	if act, _ := m.Active(); act.ID != local.ID {
		t.Fatal("saving must not switch away from the active connection")
	}
	for _, id := range []string{a.ID, b.ID, local.ID, b.ID} {
		c, err := m.Activate(id)
		if err != nil || c.ID != id {
			t.Fatalf("Activate(%s) = %+v, %v", id, c, err)
		}
		if act, _ := m.Active(); act.ID != id {
			t.Fatalf("active = %s, want %s", act.ID, id)
		}
	}
	if _, err := m.Activate("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Activate unknown = %v", err)
	}
	if b.URL != "https://b.example.com/habakkuk" || m.HasToken(b.ID) {
		t.Fatalf("b = %+v, has token %v", b, m.HasToken(b.ID))
	}

	// A nil token keeps the stored one; "" removes it.
	a.Name = "A2"
	if _, err := m.Save(a, nil); err != nil {
		t.Fatal(err)
	}
	if tok, _ := m.Token(a.ID); tok != "ta" {
		t.Fatalf("token after nil save = %q", tok)
	}
	if _, err := m.Save(a, ptr("")); err != nil {
		t.Fatal(err)
	}
	if m.HasToken(a.ID) {
		t.Fatal("empty token must remove it")
	}
	m.Save(a, ptr("ta2"))

	// Deleting the active connection falls back to the first one.
	if err := m.Delete(b.ID); err != nil {
		t.Fatal(err)
	}
	if act, _ := m.Active(); act.ID != local.ID {
		t.Fatalf("active after delete = %+v", act)
	}
	if err := m.Delete(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := sec.m[a.ID]; ok {
		t.Fatal("delete left the token in the keychain")
	}
	if err := m.Delete(local.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Active(); ok {
		t.Fatal("no connections left, yet one is active")
	}
	again, _ := Open(path, sec)
	if len(again.List()) != 0 {
		t.Fatalf("reloaded = %+v", again.List())
	}
}

func TestKeychainFailureKeepsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	sec := newMem()
	m, _ := Open(path, sec)
	sec.fail = errors.New("locked")
	if _, err := m.Save(Connection{Name: "X", Kind: Remote, URL: "https://x.example.com"}, ptr("t")); err == nil {
		t.Fatal("want an error when the keychain refuses")
	}
	if n := len(m.List()); n != 1 {
		t.Fatalf("a failed save left %d connections", n)
	}
}

func TestValidation(t *testing.T) {
	m, _ := Open(filepath.Join(t.TempDir(), "c.json"), newMem())
	bad := []Connection{
		{Name: "", Kind: Local},
		{Name: "x", Kind: "ssh"},
		{Name: "x", Kind: Remote, URL: "ftp://h"},
		{Name: "x", Kind: Remote, URL: "https://"},
		{Name: "x", Kind: Remote, URL: "https://u:p@h"},
		{Name: "x", Kind: Remote, URL: "https://h/?token=1"},
		{Name: "x", Kind: Remote, URL: "https://h/#x"},
	}
	for _, c := range bad {
		if _, err := m.Save(c, nil); err == nil {
			t.Errorf("Save(%+v) = nil error", c)
		}
	}
	// A remote drops local-only fields.
	c, err := m.Save(Connection{Name: "r", Kind: Remote, URL: "http://127.0.0.1:8484/", Home: "/x", Binary: "/y", Manual: true}, nil)
	if err != nil || c.Home != "" || c.Binary != "" || c.Manual || c.URL != "http://127.0.0.1:8484" {
		t.Fatalf("remote = %+v, %v", c, err)
	}
	if _, err := m.UpdateSettings(func(s *Settings) { s.Port = 70000 }); err == nil {
		t.Fatal("port out of range accepted")
	}
	if s, _ := m.UpdateSettings(func(s *Settings) { s.Hotkey = " " }); s.Hotkey != DefaultHotkey {
		t.Fatalf("blank hotkey = %q", s.Hotkey)
	}
}

func TestOpenCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte("{not json"), 0o600)
	if _, err := Open(path, newMem()); err == nil {
		t.Fatal("want an error for a corrupt file")
	}
}

func TestDefaultPathEnv(t *testing.T) {
	t.Setenv("HABAKKUK_DESKTOP_CONFIG", "/tmp/x.json")
	if p, _ := DefaultPath(); p != "/tmp/x.json" {
		t.Fatalf("DefaultPath = %q", p)
	}
}
