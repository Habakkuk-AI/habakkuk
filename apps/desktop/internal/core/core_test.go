package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/connections"
	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/fakehabakkuk"
	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/habakkukapi"
	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/notify"
	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/server"
	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/supervisor"
)

// With FAKE_HABAKKUK_SERVE set, the test binary is a fake `hk serve`: it
// starts a fake Habakkuk API on a free port and prints the address the way
// hk serve does.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_HABAKKUK_SERVE") != "" {
		term := make(chan os.Signal, 1)
		signal.Notify(term, syscall.SIGTERM, os.Interrupt)
		f := fakehabakkuk.New("")
		fmt.Fprintf(os.Stderr, "habakkuk fake serving; home %s\n", os.Getenv("HABAKKUK_HOME"))
		fmt.Fprintf(os.Stderr, "http on http://%s\n", strings.TrimPrefix(f.URL, "http://"))
		<-term
		f.Close()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type memSecrets struct {
	mu sync.Mutex
	m  map[string]string
}

func (s *memSecrets) Get(id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.m[id]; ok {
		return v, nil
	}
	return "", connections.ErrNoSecret
}
func (s *memSecrets) Set(id, v string) error { s.mu.Lock(); s.m[id] = v; s.mu.Unlock(); return nil }
func (s *memSecrets) Delete(id string) error { s.mu.Lock(); delete(s.m, id); s.mu.Unlock(); return nil }

type fakeUI struct {
	mu        sync.Mutex
	shown     []string
	notices   []notify.Notice
	focused   bool
	askShown  bool
	hotkeyErr error
	hotkey    string
	login     bool
}

func (u *fakeUI) ShowMain(p string) { u.mu.Lock(); u.shown = append(u.shown, p); u.mu.Unlock() }
func (u *fakeUI) Notify(n notify.Notice) {
	u.mu.Lock()
	u.notices = append(u.notices, n)
	u.mu.Unlock()
}
func (u *fakeUI) Changed()                      {}
func (u *fakeUI) MainFocused() bool             { u.mu.Lock(); defer u.mu.Unlock(); return u.focused }
func (u *fakeUI) AskVisible() bool              { u.mu.Lock(); defer u.mu.Unlock(); return u.askShown }
func (u *fakeUI) SetStartAtLogin(on bool) error { u.mu.Lock(); u.login = on; u.mu.Unlock(); return nil }
func (u *fakeUI) SetHotkey(a string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.hotkeyErr != nil {
		return u.hotkeyErr
	}
	u.hotkey = a
	return nil
}
func (u *fakeUI) noticeList() []notify.Notice {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]notify.Notice{}, u.notices...)
}
func (u *fakeUI) lastShown() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.shown) == 0 {
		return ""
	}
	return u.shown[len(u.shown)-1]
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type rig struct {
	c     *Controller
	ui    *fakeUI
	srv   *server.Server
	conns *connections.Manager
	home  string
	local connections.Connection
}

// newRig builds a controller whose active connection is a local Habakkuk in a
// temp home, served by the fake binary.
func newRig(t *testing.T) *rig {
	t.Helper()
	t.Setenv("FAKE_HABAKKUK_SERVE", "1") // inherited by the child only
	home := t.TempDir()
	// The configured port is wrong on purpose: the app must follow the
	// address serve prints.
	os.WriteFile(filepath.Join(home, "config.yaml"), []byte("http:\n  listen: 127.0.0.1:1\n"), 0o600)
	conns, err := connections.Open(filepath.Join(t.TempDir(), "config.json"), &memSecrets{m: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	loc, _ := conns.Active()
	loc.Home = home
	loc, err = conns.Save(loc, nil)
	if err != nil {
		t.Fatal(err)
	}
	ui := &fakeUI{}
	r := &rig{ui: ui, conns: conns, home: home, local: loc}
	r.c = &Controller{
		Conns: conns, UI: ui, Version: "test", Grace: 3 * time.Second,
		FindBinary: func(string) (string, error) { return os.Executable() },
		Tune: func(w *notify.Watcher) {
			w.ApprovalsEvery, w.StatusEvery, w.ConversationsEvery, w.RetryAfter =
				20*time.Millisecond, 20*time.Millisecond, 20*time.Millisecond, 10*time.Millisecond
		},
	}
	r.srv = server.New(r.c.API(), nil)
	r.c.Server = r.srv
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer scancel()
		r.c.Shutdown(sctx)
		cancel()
	})
	r.c.Start(ctx)
	return r
}

func TestLocalServeAndSwitching(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake serve stops on SIGTERM")
	}
	r := newRig(t)
	c := r.c

	waitFor(t, "the local Habakkuk", func() bool {
		return strings.HasPrefix(r.srv.Target(), "http://127.0.0.1:") && r.srv.Target() != "http://127.0.0.1:1" && c.Snapshot().Status.Connected
	})
	st := c.Snapshot()
	if st.Local == nil || st.Local.State != supervisor.Running || st.Local.PID == 0 || st.Local.External || st.Local.Home != r.home {
		t.Fatalf("local = %+v", st.Local)
	}
	pid := st.Local.PID
	lines, _ := c.Logs(0)
	var all []string
	for _, l := range lines {
		all = append(all, l.Text)
	}
	if !strings.Contains(strings.Join(all, "\n"), "home "+r.home) {
		t.Fatalf("logs = %q (HABAKKUK_HOME not passed?)", all)
	}

	// Switch to a remote Habakkuk.
	f := fakehabakkuk.New("tok")
	defer f.Close()
	tok := "tok"
	remote, err := r.conns.Save(connections.Connection{Name: "Remote", Kind: connections.Remote, URL: f.URL}, &tok)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Activate(remote.ID); err != nil {
		t.Fatal(err)
	}
	if r.srv.Target() != f.URL {
		t.Fatalf("target = %q, want %q", r.srv.Target(), f.URL)
	}
	if r.ui.lastShown() != "/desktop/#wait" {
		t.Fatalf("shown %q", r.ui.lastShown())
	}
	waitFor(t, "the remote status", func() bool { return c.Snapshot().Status.Connected })
	if st := c.Snapshot(); st.Local != nil || st.ActiveName != "Remote" {
		t.Fatalf("remote snapshot = %+v", st)
	}

	// And back: the serve the app started is still the same process.
	if err := c.Activate(r.local.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the local Habakkuk again", func() bool { return c.Snapshot().Status.Connected && r.srv.Target() != f.URL })
	if st := c.Snapshot(); st.Local.PID != pid || st.Local.External {
		t.Fatalf("after switching back: %+v (pid was %d)", st.Local, pid)
	}

	// Pause and resume.
	if err := c.StopLocal(); err != nil {
		t.Fatal(err)
	}
	if st := c.Snapshot(); st.Local.State != supervisor.Stopped {
		t.Fatalf("after stop: %+v", st.Local)
	}
	waitFor(t, "the disconnected status", func() bool { return !c.Snapshot().Status.Connected })
	if err := c.StartLocal(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the restarted Habakkuk", func() bool {
		st := c.Snapshot()
		return st.Status.Connected && st.Local.PID != 0 && st.Local.PID != pid
	})

	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c.Shutdown(sctx)
	if st := c.Snapshot(); st.Local.State != supervisor.Stopped {
		t.Fatalf("after shutdown: %+v", st.Local)
	}
}

func TestLocalProblems(t *testing.T) {
	r := newRig(t)
	c := r.c
	empty := t.TempDir()
	conn, _ := r.conns.Save(connections.Connection{Name: "Empty", Kind: connections.Local, Home: empty}, nil)
	c.Activate(conn.ID)
	if st := c.Snapshot(); !strings.Contains(st.Problem, "hk setup") || r.srv.Target() != "" {
		t.Fatalf("no config: problem %q target %q", st.Problem, r.srv.Target())
	}
	os.WriteFile(filepath.Join(empty, "config.yaml"), []byte("agents: {}\n"), 0o600)
	c.Reconnect()
	if st := c.Snapshot(); !strings.Contains(st.Problem, "http section") {
		t.Fatalf("no http: problem %q", st.Problem)
	}

	// A serve already running (another pid beating) is left alone.
	os.WriteFile(filepath.Join(empty, "config.yaml"), []byte("http: {}\n"), 0o600)
	hb, _ := json.Marshal(map[string]any{"pid": 999999, "beat": time.Now(), "listen": "127.0.0.1:9"})
	os.WriteFile(filepath.Join(empty, "serve.json"), hb, 0o600)
	c.Reconnect()
	st := c.Snapshot()
	if st.Problem != "" || !st.Local.External || st.Local.PID != 0 || r.srv.Target() != "http://127.0.0.1:9" {
		t.Fatalf("external: %+v target %q", st, r.srv.Target())
	}

	// A missing binary is a problem for an automatic start, not a manual one.
	os.Remove(filepath.Join(empty, "serve.json"))
	c.FindBinary = func(string) (string, error) { return "", errors.New("no habakkuk executable found") }
	conn.Home = t.TempDir()
	os.WriteFile(filepath.Join(conn.Home, "config.yaml"), []byte("http: {}\n"), 0o600)
	saved, _ := r.conns.Save(conn, nil)
	c.Reconnect()
	if st := c.Snapshot(); !strings.Contains(st.Problem, "no habakkuk executable") {
		t.Fatalf("missing binary: %q", st.Problem)
	}
	saved.Manual = true
	r.conns.Save(saved, nil)
	c.Reconnect()
	if st := c.Snapshot(); st.Problem != "" || r.srv.Target() != "http://127.0.0.1:8484" {
		t.Fatalf("manual: %q target %q", st.Problem, r.srv.Target())
	}
	if err := c.StartLocal(); err == nil {
		t.Fatal("StartLocal without a binary succeeded")
	}
}

func TestNotificationsAndApprove(t *testing.T) {
	r := newRig(t)
	c := r.c
	f := fakehabakkuk.New("tok")
	defer f.Close()
	tok := "tok"
	remote, _ := r.conns.Save(connections.Connection{Name: "R", Kind: connections.Remote, URL: f.URL}, &tok)
	a := habakkukapi.Approval{ID: "ap1", Tool: "exec", Summary: "ls"}
	a.To.Account, a.To.Conversation = "web", "c1"
	f.AddApproval(a)
	c.Activate(remote.ID)
	waitFor(t, "the approval notice", func() bool { return len(r.ui.noticeList()) == 1 })
	if n := r.ui.noticeList()[0]; n.Approval != "ap1" || n.Conversation != "c1" {
		t.Fatalf("notice = %+v", n)
	}
	if err := c.Approve(context.Background(), "ap1", false); err != nil {
		t.Fatal(err)
	}
	if approve, ok := f.Answer("ap1"); !ok || approve {
		t.Fatalf("answer = %v, %v", approve, ok)
	}

	// Replies: quiet while the main window has focus, or when notifications
	// are off; the quick ask conversation is quiet while its window shows.
	f.AddConversation(habakkukapi.Conversation{Conversation: "c1", LastAt: time.Now()})
	f.AddConversation(habakkukapi.Conversation{Conversation: "quick-1", LastAt: time.Now()})
	waitFor(t, "the streams", func() bool { return f.HasFollow("c1") && f.HasFollow("quick-1") })
	r.ui.mu.Lock()
	r.ui.focused = true
	r.ui.mu.Unlock()
	f.Push("c1", "assistant", "while focused", time.Now())
	time.Sleep(100 * time.Millisecond)
	r.ui.mu.Lock()
	r.ui.focused, r.ui.askShown = false, true
	r.ui.mu.Unlock()
	c.SetAskConversation("quick-1")
	f.Push("quick-1", "assistant", "in the ask window", time.Now())
	time.Sleep(100 * time.Millisecond)
	if n := len(r.ui.noticeList()); n != 1 {
		t.Fatalf("quiet replies notified: %+v", r.ui.noticeList())
	}
	f.Push("c1", "assistant", "now notify", time.Now())
	waitFor(t, "the reply notice", func() bool { return len(r.ui.noticeList()) == 2 })
	if n := r.ui.noticeList()[1]; n.Kind != notify.Reply || n.Body != "now notify" {
		t.Fatalf("reply = %+v", n)
	}
	r.conns.UpdateSettings(func(s *connections.Settings) { s.Quiet = true })
	f.Push("c1", "assistant", "muted", time.Now())
	time.Sleep(100 * time.Millisecond)
	if n := len(r.ui.noticeList()); n != 2 {
		t.Fatalf("notified with notifications off: %+v", r.ui.noticeList())
	}
}

func call(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	data, _ := io.ReadAll(rec.Body)
	var out map[string]any
	json.Unmarshal(data, &out)
	return rec.Code, out
}

func TestAPI(t *testing.T) {
	r := newRig(t)
	c := r.c
	h := c.API()
	f := fakehabakkuk.New("tok")
	defer f.Close()

	code, st := call(t, h, "GET", "/state", "")
	if code != 200 || st["active"] != r.local.ID || st["version"] != "test" {
		t.Fatalf("state: %d %v", code, st)
	}

	// Add a remote with a token; the response never echoes it.
	code, saved := call(t, h, "POST", "/connections", fmt.Sprintf(`{"name":"Work","kind":"remote","url":%q,"token":"tok"}`, f.URL+"/app/"))
	if code != 200 || saved["url"] != f.URL || saved["has_token"] != true || saved["token"] != nil {
		t.Fatalf("save: %d %v", code, saved)
	}
	id := saved["id"].(string)
	if code, out := call(t, h, "POST", "/connections", `{"name":"x","kind":"remote","url":"ftp://x"}`); code != 400 || out["error"] == nil {
		t.Fatalf("bad url: %d %v", code, out)
	}
	if code, _ := call(t, h, "POST", "/connections", `{"id":"nope","name":"x","kind":"local"}`); code != 404 {
		t.Fatalf("unknown id: %d", code)
	}
	if code, _ := call(t, h, "POST", "/connections", `{"name":"x","kind":"local","surprise":1}`); code != 400 {
		t.Fatalf("unknown field: %d", code)
	}

	// Test uses the stored token when none is typed.
	if _, out := call(t, h, "POST", "/test", fmt.Sprintf(`{"id":%q,"url":%q}`, id, f.URL)); out["ok"] != true || out["default"] != "habakkuk" {
		t.Fatalf("test stored token: %v", out)
	}
	if _, out := call(t, h, "POST", "/test", fmt.Sprintf(`{"url":%q,"token":"bad"}`, f.URL)); out["ok"] != false || !strings.Contains(out["error"].(string), "token") {
		t.Fatalf("test bad token: %v", out)
	}

	if code, out := call(t, h, "POST", "/connections/"+id+"/activate", "{}"); code != 200 || out["active"] != id {
		t.Fatalf("activate: %d %v", code, out)
	}
	if code, _ := call(t, h, "POST", "/connections/nope/activate", "{}"); code != 404 {
		t.Fatalf("activate unknown: %d", code)
	}
	if code, out := call(t, h, "POST", "/local/start", "{}"); code != 400 || !strings.Contains(out["error"].(string), "not on this computer") {
		t.Fatalf("local start on a remote: %d %v", code, out)
	}

	// Settings: a hotkey the OS refuses is reported and not saved.
	r.ui.hotkeyErr = errors.New("taken")
	code, out := call(t, h, "PUT", "/settings", `{"hotkey":"Cmd+Q","start_at_login":true,"quiet":true}`)
	if code != 200 || !strings.Contains(out["warning"].(string), "taken") {
		t.Fatalf("settings: %d %v", code, out)
	}
	if s := r.conns.Settings(); s.Hotkey != connections.DefaultHotkey || !s.StartAtLogin || !s.Quiet || !r.ui.login {
		t.Fatalf("settings saved = %+v", s)
	}
	r.ui.hotkeyErr = nil
	call(t, h, "PUT", "/settings", `{"hotkey":"Ctrl+Alt+F","start_at_login":true,"quiet":false}`)
	if s := r.conns.Settings(); s.Hotkey != "Ctrl+Alt+F" || r.ui.hotkey != "Ctrl+Alt+F" {
		t.Fatalf("hotkey = %q / %q", s.Hotkey, r.ui.hotkey)
	}

	if code, _ := call(t, h, "POST", "/open", `{"path":"https://evil.example/"}`); code != 400 {
		t.Fatalf("open outside: %d", code)
	}
	if code, _ := call(t, h, "POST", "/open", `{"path":"/app/#/approvals"}`); code != 204 || r.ui.lastShown() != "/app/#/approvals" {
		t.Fatalf("open: %d %q", code, r.ui.lastShown())
	}
	if code, _ := call(t, h, "POST", "/ask", `{"conversation":"quick-9"}`); code != 204 {
		t.Fatalf("ask: %d", code)
	}
	if code, out := call(t, h, "GET", "/logs?after=0", ""); code != 200 || out["lines"] == nil {
		t.Fatalf("logs: %d %v", code, out)
	}

	// Deleting the active connection falls back to the first one.
	if code, _ := call(t, h, "DELETE", "/connections/"+id, ""); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	if act, _ := r.conns.Active(); act.ID != r.local.ID {
		t.Fatalf("active after delete = %+v", act)
	}
	if r.conns.HasToken(id) {
		t.Fatal("the token survived the delete")
	}
}
