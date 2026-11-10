package nodes

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/testutil"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func openStore(t *testing.T) *Store {
	t.Helper()
	st, err := OpenStore(context.Background(), filepath.Join(t.TempDir(), "nodes.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestPairingCodesAndSecrets(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	code, expires, err := st.NewCode(ctx, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != 11 || code[5] != '-' || time.Until(expires) < 9*time.Minute {
		t.Fatalf("code %q expires %v", code, expires)
	}
	// Codes are typed by people: case, dash and spaces do not matter.
	typed := strings.ToLower(strings.ReplaceAll(code, "-", " "))
	secret, err := st.Redeem(ctx, typed, "laptop", "darwin")
	if err != nil {
		t.Fatal(err)
	}
	if len(secret) < 40 {
		t.Fatalf("secret too short: %q", secret)
	}
	if _, err := st.Redeem(ctx, code, "other", ""); !errors.Is(err, ErrBadCode) {
		t.Fatalf("a used code worked again: %v", err)
	}
	if ok, _ := st.Verify(ctx, "laptop", secret); !ok {
		t.Fatal("the issued secret does not verify")
	}
	if ok, _ := st.Verify(ctx, "laptop", secret+"x"); ok {
		t.Fatal("a wrong secret verified")
	}
	if ok, _ := st.Verify(ctx, "nobody", secret); ok {
		t.Fatal("an unknown node verified")
	}
	// The database keeps hashes, never the secret or the code.
	var stored string
	if err := st.db.QueryRow(`SELECT secret_hash FROM nodes WHERE name = 'laptop'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == secret || strings.Contains(stored, secret) {
		t.Fatal("the secret is stored in the clear")
	}

	// Pairing the name again replaces the secret.
	code2, _, _ := st.NewCode(ctx, time.Minute)
	secret2, err := st.Redeem(ctx, code2, "laptop", "linux")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.Verify(ctx, "laptop", secret); ok {
		t.Fatal("the old secret still works after re-pairing")
	}
	if ok, _ := st.Verify(ctx, "laptop", secret2); !ok {
		t.Fatal("the new secret does not verify")
	}

	// Expired codes fail.
	now := time.Now()
	st.now = func() time.Time { return now }
	code3, _, _ := st.NewCode(ctx, time.Minute)
	st.now = func() time.Time { return now.Add(2 * time.Minute) }
	if _, err := st.Redeem(ctx, code3, "pi", ""); !errors.Is(err, ErrBadCode) {
		t.Fatalf("an expired code worked: %v", err)
	}
	st.now = time.Now

	if _, err := st.Redeem(ctx, "whatever", "bad name!", ""); err == nil {
		t.Fatal("a bad node name was accepted")
	}
	if ok, _ := st.Revoke(ctx, "laptop"); !ok {
		t.Fatal("revoke found nothing")
	}
	if ok, _ := st.Verify(ctx, "laptop", secret2); ok {
		t.Fatal("a revoked node verified")
	}
}

// rig is a hub behind an HTTP test server.
type rig struct {
	hub *Hub
	srv *httptest.Server
	ln  *trackListener
}

// trackListener remembers every connection it accepts, so a test can cut
// them all, websockets included (the server forgets hijacked ones).
type trackListener struct {
	net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func (l *trackListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.mu.Lock()
		l.conns = append(l.conns, c)
		l.mu.Unlock()
	}
	return c, err
}

func (l *trackListener) cutAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.conns {
		c.Close()
	}
	l.conns = nil
}

func newRig(t *testing.T) *rig {
	t.Helper()
	h := &Hub{Store: openStore(t), Log: quiet(), Heartbeat: 100 * time.Millisecond}
	srv := httptest.NewUnstartedServer(h.Public())
	ln := &trackListener{Listener: srv.Listener}
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return &rig{hub: h, srv: srv, ln: ln}
}

func (r *rig) pair(t *testing.T, name string) string {
	t.Helper()
	code, _, err := r.hub.Store.NewCode(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := Pair(context.Background(), r.srv.Client(), r.srv.URL, code, name, "test")
	if err != nil {
		t.Fatal(err)
	}
	return secret
}

func echoCaps() Caps {
	return Caps{
		"test.echo": Func(func(_ context.Context, args json.RawMessage) (*Result, error) {
			var in struct{ Say string }
			_ = json.Unmarshal(args, &in)
			return &Result{Text: "echo: " + in.Say}, nil
		}),
		"test.fail": Func(func(context.Context, json.RawMessage) (*Result, error) {
			return nil, errors.New("it broke")
		}),
		"test.photo": Func(func(context.Context, json.RawMessage) (*Result, error) {
			var buf bytes.Buffer
			_ = png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 4, 3)))
			return &Result{Text: "photo", Files: []File{{Name: "shot.png", MediaType: "image/png", Data: buf.Bytes()}}}, nil
		}),
	}
}

// start runs a client until the test ends and returns how many times it
// has connected.
func (r *rig) start(t *testing.T, c *Client) *atomic.Int32 {
	t.Helper()
	var connects atomic.Int32
	c.Server, c.HTTP, c.Log = r.srv.URL, r.srv.Client(), quiet()
	c.Heartbeat, c.MinBackoff, c.MaxBackoff = 100*time.Millisecond, 20*time.Millisecond, 50*time.Millisecond
	c.OnConnect = func() { connects.Add(1) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = c.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return &connects
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (r *rig) online(name string) func() bool {
	return func() bool { _, ok := r.hub.Online()[name]; return ok }
}

func TestWebsocketInvokeRoundTrip(t *testing.T) {
	r := newRig(t)
	secret := r.pair(t, "laptop")
	r.start(t, &Client{Name: "laptop", Secret: secret, Platform: "test", Caps: echoCaps()})
	waitFor(t, "connection", r.online("laptop"))

	live := r.hub.Online()["laptop"]
	if strings.Join(live.Caps, ",") != "test.echo,test.fail,test.photo" {
		t.Fatalf("caps %v", live.Caps)
	}
	ctx := context.Background()
	res, err := r.hub.Invoke(ctx, "laptop", "test.echo", json.RawMessage(`{"say":"hi"}`), 5*time.Second)
	if err != nil || res.Text != "echo: hi" {
		t.Fatalf("invoke: %v %+v", err, res)
	}
	res, err = r.hub.Invoke(ctx, "laptop", "test.fail", nil, 5*time.Second)
	if err != nil || res.Error != "it broke" {
		t.Fatalf("failing command: %v %+v", err, res)
	}
	res, err = r.hub.Invoke(ctx, "laptop", "test.photo", nil, 5*time.Second)
	if err != nil || len(res.Files) != 1 || !bytes.HasPrefix(res.Files[0].Data, []byte("\x89PNG")) {
		t.Fatalf("file result: %v %+v", err, res)
	}
	if _, err := r.hub.Invoke(ctx, "laptop", "system.run", nil, time.Second); !errors.Is(err, ErrNoCap) {
		t.Fatalf("a command the node does not offer: %v", err)
	}
	if _, err := r.hub.Invoke(ctx, "phone", "test.echo", nil, time.Second); !errors.Is(err, ErrOffline) {
		t.Fatalf("an unconnected node: %v", err)
	}

	// Several invocations at once each get their own answer.
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			say := strings.Repeat("x", i+1)
			res, err := r.hub.Invoke(ctx, "laptop", "test.echo", json.RawMessage(`{"say":"`+say+`"}`), 5*time.Second)
			if err != nil || res.Text != "echo: "+say {
				t.Errorf("concurrent invoke %d: %v %+v", i, err, res)
			}
		}()
	}
	wg.Wait()
}

func TestWrongSecretIsRejected(t *testing.T) {
	r := newRig(t)
	r.pair(t, "laptop")
	c := &Client{Name: "laptop", Secret: "not-the-secret", Caps: echoCaps(), Server: r.srv.URL, HTTP: r.srv.Client(), Log: quiet(),
		MinBackoff: 10 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Run(ctx); !errors.Is(err, ErrRejected) {
		t.Fatalf("Run = %v, want ErrRejected", err)
	}
	if _, ok := r.hub.Online()["laptop"]; ok {
		t.Fatal("a node with a wrong secret got online")
	}
	// A wrong code is refused too.
	if _, err := Pair(ctx, r.srv.Client(), r.srv.URL, "AAAAA-AAAAA", "pi", ""); err == nil {
		t.Fatal("a made-up code paired")
	}
}

func TestReconnectAfterDrop(t *testing.T) {
	r := newRig(t)
	secret := r.pair(t, "pi")
	connects := r.start(t, &Client{Name: "pi", Secret: secret, Caps: echoCaps()})
	waitFor(t, "first connection", r.online("pi"))

	r.hub.Disconnect("pi") // the server drops the connection
	waitFor(t, "reconnect", func() bool { return connects.Load() >= 2 && r.online("pi")() })
	res, err := r.hub.Invoke(context.Background(), "pi", "test.echo", json.RawMessage(`{"say":"back"}`), 5*time.Second)
	if err != nil || res.Text != "echo: back" {
		t.Fatalf("invoke after reconnect: %v %+v", err, res)
	}

	// Every client connection dies at once (a network blip).
	r.ln.cutAll()
	waitFor(t, "reconnect after blip", func() bool { return connects.Load() >= 3 && r.online("pi")() })
}

func TestRevokeCutsOffALiveNode(t *testing.T) {
	r := newRig(t)
	secret := r.pair(t, "pi")
	c := &Client{Name: "pi", Secret: secret, Caps: echoCaps(), Server: r.srv.URL, HTTP: r.srv.Client(), Log: quiet(),
		Heartbeat: 100 * time.Millisecond, MinBackoff: 10 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- c.Run(ctx) }()
	waitFor(t, "connection", r.online("pi"))
	// Another process (hk nodes revoke) removes it from the store only.
	if _, err := r.hub.Store.Revoke(ctx, "pi"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "disconnect", func() bool { return !r.online("pi")() })
	if err := <-errc; !errors.Is(err, ErrRejected) {
		t.Fatalf("Run after revoke = %v, want ErrRejected", err)
	}
}

// approver records approval requests and answers with ok.
type approver struct {
	mu   sync.Mutex
	reqs []tools.ApprovalRequest
	ok   bool
}

func (a *approver) Approve(_ context.Context, req tools.ApprovalRequest) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reqs = append(a.reqs, req)
	return a.ok, nil
}

func (a *approver) asked() int { a.mu.Lock(); defer a.mu.Unlock(); return len(a.reqs) }

func TestNodeInvokeIsApprovalGated(t *testing.T) {
	r := newRig(t)
	secret := r.pair(t, "laptop")
	r.start(t, &Client{Name: "laptop", Secret: secret, Caps: echoCaps()})
	waitFor(t, "connection", r.online("laptop"))

	cfg := &config.Nodes{Allow: map[string][]string{"laptop": {"test.echo"}, "*": {"test.fail"}}}
	reg := tools.NewRegistry()
	RegisterTools(reg, r.hub, cfg)
	blobs, err := store.NewBlobs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	ap := &approver{}
	env := &tools.Env{Agent: "habakkuk", Workspace: ws, Mode: tools.ModeFull, Approver: ap, Blobs: blobs}
	call := func(input string) (llm.Block, []llm.Block) {
		return reg.ExecuteMedia(context.Background(), env, llm.Block{Type: llm.ToolUse, ToolUseID: "c1", ToolName: "node_invoke", Input: json.RawMessage(input)})
	}

	// Allowed in config: runs without asking, even with no one to ask.
	res, _ := call(`{"node":"laptop","command":"test.echo","args":{"say":"free"}}`)
	if res.IsError || !strings.Contains(res.Text, "echo: free") || ap.asked() != 0 {
		t.Fatalf("allowed command: %+v (asked %d)", res, ap.asked())
	}

	// Not allowed: asked even in full mode; a no denies it.
	res, _ = call(`{"node":"laptop","command":"test.photo"}`)
	if !res.IsError || !strings.Contains(res.Text, "declined") || ap.asked() != 1 {
		t.Fatalf("declined command: %+v (asked %d)", res, ap.asked())
	}
	if req := ap.reqs[0]; req.Tool != "node_invoke" || !strings.Contains(req.Summary, "test.photo on node laptop") {
		t.Fatalf("approval request: %+v", req)
	}

	// A yes runs it; the image is saved in the workspace and shown.
	ap.ok = true
	res, media := call(`{"node":"laptop","command":"test.photo"}`)
	if res.IsError || ap.asked() != 2 || len(media) != 1 || media[0].Type != llm.Image {
		t.Fatalf("approved photo: %+v media %d (asked %d)", res, len(media), ap.asked())
	}
	saved, _ := filepath.Glob(filepath.Join(ws, "nodes", "laptop", "*-shot.png"))
	if len(saved) != 1 {
		t.Fatalf("photo not saved in the workspace: %v", saved)
	}

	// Unattended runs (no approver) are denied what config does not allow.
	env.Approver = nil
	res, _ = call(`{"node":"laptop","command":"test.photo"}`)
	if !res.IsError || !strings.Contains(res.Text, "no one is attending") {
		t.Fatalf("unattended: %+v", res)
	}
	// ... but a command allowed for every node ("*") still runs.
	res, _ = call(`{"node":"laptop","command":"test.fail"}`)
	if !res.IsError || !strings.Contains(res.Text, "it broke") {
		t.Fatalf("wildcard-allowed failing command: %+v", res)
	}

	// Read-only agents cannot reach devices at all.
	env.Mode, env.Approver = tools.ModeReadOnly, ap
	res, _ = call(`{"node":"laptop","command":"test.echo","args":{"say":"x"}}`)
	if !res.IsError || !strings.Contains(res.Text, "read-only") {
		t.Fatalf("read-only: %+v", res)
	}

	// nodes_list shows the node and what runs without asking.
	env.Mode = tools.ModeWorkspace
	res = reg.Execute(context.Background(), env, llm.Block{Type: llm.ToolUse, ToolUseID: "c2", ToolName: "nodes_list", Input: json.RawMessage(`{}`)})
	if !strings.Contains(res.Text, "laptop (test, online)") || !strings.Contains(res.Text, "run without asking: test.echo, test.fail") {
		t.Fatalf("nodes_list: %s", res.Text)
	}
}

func TestBrowserNodeOverEvents(t *testing.T) {
	r := newRig(t)
	secret := r.pair(t, "phone")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, r.srv.URL+"/nodes/events?caps=location.get,system.notify&platform=browser", nil)
	req.Header.Set("X-Habakkuk-Node", "phone")
	req.Header.Set("Authorization", "Bearer "+secret)
	res, err := r.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("events: %s", res.Status)
	}
	frames := make(chan Frame, 4)
	go func() {
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var f Frame
				if json.Unmarshal([]byte(data), &f) == nil {
					frames <- f
				}
			}
		}
	}()
	if f := <-frames; f.Type != "welcome" {
		t.Fatalf("first frame %+v", f)
	}
	waitFor(t, "browser node online", r.online("phone"))

	type answer struct {
		res *Result
		err error
	}
	got := make(chan answer, 1)
	go func() {
		res, err := r.hub.Invoke(ctx, "phone", "location.get", nil, 5*time.Second)
		got <- answer{res, err}
	}()
	inv := <-frames
	if inv.Type != "invoke" || inv.Command != "location.get" {
		t.Fatalf("invoke frame %+v", inv)
	}
	post := func(secret string) int {
		body, _ := json.Marshal(Frame{Type: "result", ID: inv.ID, Result: &Result{Text: "43.65, -79.38"}})
		req, _ := http.NewRequest(http.MethodPost, r.srv.URL+"/nodes/result", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Habakkuk-Node", "phone")
		req.Header.Set("Authorization", "Bearer "+secret)
		res, err := r.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if code := post("wrong"); code != http.StatusUnauthorized {
		t.Fatalf("result with a wrong secret: %d", code)
	}
	if code := post(secret); code != http.StatusNoContent {
		t.Fatalf("result: %d", code)
	}
	if a := <-got; a.err != nil || a.res.Text != "43.65, -79.38" {
		t.Fatalf("browser invoke: %v %+v", a.err, a.res)
	}

	// A wrong secret cannot open the stream.
	bad, _ := http.NewRequest(http.MethodGet, r.srv.URL+"/nodes/events", nil)
	bad.Header.Set("X-Habakkuk-Node", "phone")
	bad.Header.Set("Authorization", "Bearer nope")
	if res, err := r.srv.Client().Do(bad); err != nil || res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("events with a wrong secret: %v %v", err, res.Status)
	}
}

func TestAdminAPI(t *testing.T) {
	r := newRig(t)
	admin := httptest.NewServer(r.hub.Admin())
	defer admin.Close()
	res, err := http.Post(admin.URL+"/api/nodes/pair", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var code struct{ Code string }
	_ = json.NewDecoder(res.Body).Decode(&code)
	res.Body.Close()
	if _, err := Pair(context.Background(), r.srv.Client(), r.srv.URL, code.Code, "tablet", "browser"); err != nil {
		t.Fatalf("pair with a dashboard code: %v", err)
	}
	res, _ = http.Get(admin.URL + "/api/nodes")
	var list struct{ Nodes []NodeInfo }
	_ = json.NewDecoder(res.Body).Decode(&list)
	res.Body.Close()
	if len(list.Nodes) != 1 || list.Nodes[0].Name != "tablet" || list.Nodes[0].Online {
		t.Fatalf("list: %+v", list)
	}
	req, _ := http.NewRequest(http.MethodDelete, admin.URL+"/api/nodes/tablet", nil)
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: %s", res.Status)
	}
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != http.StatusNotFound {
		t.Fatalf("revoke again: %s", res.Status)
	}
}

func TestPairingIsThrottled(t *testing.T) {
	r := newRig(t)
	var last error
	for range 25 {
		_, last = Pair(context.Background(), r.srv.Client(), r.srv.URL, "BBBBB-BBBBB", "x", "")
	}
	if last == nil || !strings.Contains(last.Error(), "too many") {
		t.Fatalf("after many bad codes: %v", last)
	}
}

func TestFilesStayInsideTheSharedDirectory(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("no"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skip("cannot create symlinks here:", err)
	}

	caps := LocalCaps(LocalOptions{Dir: root})
	put, get := caps[CmdFilesPut], caps[CmdFilesGet]
	ctx := context.Background()
	if _, err := put.Run(ctx, json.RawMessage(`{"path":"notes/a.txt","text":"hello"}`)); err != nil {
		t.Fatal(err)
	}
	res, err := get.Run(ctx, json.RawMessage(`{"path":"notes/a.txt"}`))
	if err != nil || len(res.Files) != 1 || string(res.Files[0].Data) != "hello" {
		t.Fatalf("get: %v %+v", err, res)
	}
	res, err = get.Run(ctx, json.RawMessage(`{"path":"notes"}`))
	if err != nil || !strings.Contains(res.Text, "a.txt") {
		t.Fatalf("list dir: %v %+v", err, res)
	}
	for _, bad := range []string{"../x", "link/secret.txt", filepath.Join(outside, "secret.txt"), "notes/../../x"} {
		args, _ := json.Marshal(map[string]string{"path": bad, "text": "x"})
		if _, err := get.Run(ctx, args); err == nil {
			t.Errorf("files.get reached %s", bad)
		}
		if _, err := put.Run(ctx, args); err == nil {
			t.Errorf("files.put wrote %s", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "x")); err == nil {
		t.Fatal("a file was written outside the shared directory")
	}
}

func TestLocalCapsRespectFlags(t *testing.T) {
	oldLook, oldOS := lookPath, goos
	defer func() { lookPath, goos = oldLook, oldOS }()
	lookPath = func(string) (string, error) { return "/bin/true", nil } // every tool is "installed"
	goos = "linux"
	caps := LocalCaps(LocalOptions{})
	for _, c := range []string{CmdRun, CmdCamera, CmdFilesGet, CmdFilesPut} {
		if _, ok := caps[c]; ok {
			t.Errorf("%s offered without being allowed", c)
		}
	}
	for _, c := range []string{CmdNotify, CmdScreen, CmdClipRead, CmdClipWrite} {
		if _, ok := caps[c]; !ok {
			t.Errorf("%s not offered on linux with its tools installed", c)
		}
	}
	caps = LocalCaps(LocalOptions{AllowRun: true, AllowCamera: true, Dir: t.TempDir()})
	for _, c := range []string{CmdRun, CmdCamera, CmdFilesGet, CmdFilesPut} {
		if _, ok := caps[c]; !ok {
			t.Errorf("%s not offered when allowed", c)
		}
	}
	lookPath = func(string) (string, error) { return "", errors.New("not found") }
	if caps := LocalCaps(LocalOptions{}); len(caps) != 0 {
		t.Errorf("caps offered with no tools installed: %v", caps.Names())
	}
}

func TestSystemRun(t *testing.T) {
	testutil.UnixOnly(t, "runs /bin/sh (system.run uses cmd /C on Windows)")
	res, err := runShell(context.Background(), json.RawMessage(`{"command":"echo hi; exit 3"}`))
	if err != nil || !strings.Contains(res.Text, "hi") || !strings.Contains(res.Text, "[exit code 3]") {
		t.Fatalf("run: %v %+v", err, res)
	}
	if _, err := runShell(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("an empty command ran")
	}
}

func TestWebsocketURL(t *testing.T) {
	for in, want := range map[string]string{
		"http://127.0.0.1:8484":         "ws://127.0.0.1:8484/nodes",
		"https://habakkuk.example.com/": "wss://habakkuk.example.com/nodes",
		"https://example.com/habakkuk":  "wss://example.com/habakkuk/nodes",
	} {
		if got, err := WebsocketURL(in); err != nil || got != want {
			t.Errorf("WebsocketURL(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := WebsocketURL("ftp://x"); err == nil {
		t.Error("ftp accepted")
	}
}
