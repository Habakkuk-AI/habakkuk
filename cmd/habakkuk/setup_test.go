package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	plat "github.com/habakkuk-ai/habakkuk/internal/core/platform"
	"github.com/habakkuk-ai/habakkuk/internal/testutil"
)

// fakeModel is an OpenAI-compatible API: GET /v1/models and a streamed
// chat completion, for the key "sk-test".
type fakeModel struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls int
	delay time.Duration
	open  bool // a local server: no key needed
}

func (f *fakeModel) set(delay time.Duration, open bool) {
	f.mu.Lock()
	f.delay, f.open = delay, open
	f.mu.Unlock()
}

func (f *fakeWA) fail(status, code int) {
	f.mu.Lock()
	f.status, f.code = status, code
	f.mu.Unlock()
}

func (f *fakeModel) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeWA) polls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.queries)
}

func (f *fakeWA) wait(body string) {
	f.mu.Lock()
	f.waiting = body
	f.mu.Unlock()
}

func newFakeModel(t *testing.T) *fakeModel {
	f := &fakeModel{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		delay, open := f.delay, f.open
		f.mu.Unlock()
		if !open && r.URL.Path != "/v1/models" && r.Header.Get("Authorization") != "Bearer sk-test" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":{"message":"Incorrect API key provided","type":"invalid_request_error","code":"invalid_api_key"}}`)
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			io.WriteString(w, `{"object":"list","data":[{"id":"nomic-embed-text","object":"model"},{"id":"llama-test","object":"model"}]}`)
		case "/v1/chat/completions":
			f.mu.Lock()
			f.calls++
			f.mu.Unlock()
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			for _, c := range []string{
				`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`,
				`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			} {
				io.WriteString(w, "data: "+c+"\n\n")
			}
			io.WriteString(w, "data: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// fakeWA is the WhatsApp agent platform for the key "wa-key": each poll
// answers with the waiting messages, which polling never consumes.
type fakeWA struct {
	srv     *httptest.Server
	mu      sync.Mutex
	queries []string
	waiting string // the body of a poll that finds messages; "" answers 204
	status  int    // when set, polls fail with this status and a platform error
	code    int
}

func newFakeWA(t *testing.T) *fakeWA {
	f := &fakeWA{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.queries = append(f.queries, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		waiting, status, code := f.waiting, f.status, f.code
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer wa-key" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":{"message":"(#190) invalid token","type":"OAuthException","code":190}}`)
			return
		}
		if status != 0 {
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "error", "code": code}})
			return
		}
		if r.URL.Path != "/updates" || waiting == "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		io.WriteString(w, waiting)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

const waHello = `{"object":"whatsapp_business_account","entry":[{"id":"1","changes":[{"field":"messages","value":{
	"contacts":[{"wa_id":"user:15550001","profile":{"name":"Ada"}}],
	"messages":[{"from":"user:15550001","id":"m1","timestamp":"1700000000","type":"text","text":{"body":"hi"}}]}}]}],"next_offset":7}`

// fakeTelegram is the Bot API for the token "123:tg".
func newFakeTelegram(t *testing.T, bodies *[]string) *httptest.Server {
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		*bodies = append(*bodies, r.URL.Path+" "+string(b))
		mu.Unlock()
		switch r.URL.Path {
		case "/bot123:tg/getMe":
			io.WriteString(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"Nova","username":"nova_bot"}}`)
		case "/bot123:tg/getUpdates":
			io.WriteString(w, `{"ok":true,"result":[{"update_id":5,"message":{"message_id":1,"from":{"id":4242,"is_bot":false,"first_name":"Ada"},"chat":{"id":4242,"type":"private"},"text":"/start"}}]}`)
		default:
			io.WriteString(w, `{"ok":false,"error_code":401,"description":"Unauthorized"}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

type setupRun struct {
	userHome string // where another agent's home may be; "" is an empty folder
	env      map[string]string
	ep       endpoints
	stdin    string
	goos     string
	served   bool
	chatted  bool
	cmds     [][]string // service commands setup ran
	onCmd    func()
}

func (r *setupRun) run(t *testing.T, home string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	s, err := newSetup(context.Background(), home, args, strings.NewReader(r.stdin), &out)
	if err != nil {
		return out.String(), err
	}
	s.getenv = func(k string) string { return r.env[k] }
	s.userHome = r.userHome
	if s.userHome == "" {
		s.userHome = t.TempDir() // never find this machine's other agents
	}
	s.ep = r.ep
	if s.ep.local == nil {
		s.ep.local = map[string]string{} // never probe this machine's servers
	}
	s.goos = r.goos
	s.runServe = func(context.Context, string) error { r.served = true; return nil }
	s.runChat = func(context.Context, string) error { r.chatted = true; return nil }
	s.runCmd = func(name string, args ...string) error {
		r.cmds = append(r.cmds, append([]string{name}, args...))
		if r.onCmd != nil {
			r.onCmd()
		}
		return nil
	}
	s.serveWait = 2 * time.Second
	err = s.run()
	return out.String(), err
}

func mustLoad(t *testing.T, home string) *config.Config {
	t.Helper()
	cfg, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// TestSetupHappyPathNonInteractive is the scripted path from nothing to a
// WhatsApp agent: model and channel checked live (against fakes), names
// written, doctor clean. It also times the whole run.
func TestSetupHappyPathNonInteractive(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, ".env"), []byte("# mine\nOTHER=1\n"), 0o644)
	model, wa := newFakeModel(t), newFakeWA(t)
	wa.wait(waHello)
	r := &setupRun{env: map[string]string{"OPENAI_API_KEY": "sk-test", "WHATSAPP_AGENT_KEY": "wa-key"},
		ep: endpoints{whatsappAgent: wa.srv.URL}, goos: "linux"}
	start := time.Now()
	out, err := r.run(t, home, "-y", "-provider", "openai", "-base-url", model.srv.URL+"/v1", "-model", "gpt-test",
		"-channel", "whatsappagent", "-name", "Nova", "-user", "Ada")
	took := time.Since(start)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	t.Logf("happy path (model check, WhatsApp check, config, names, doctor) took %s", took)
	if took > 10*time.Second {
		t.Errorf("setup took %s", took)
	}

	cfg := mustLoad(t, home)
	if a := cfg.Agents["habakkuk"]; a.Model != "openai/gpt-test" || a.Permission != "workspace" || a.Workspace != defaultWorkspace {
		t.Fatalf("agent = %+v", a)
	}
	if p := cfg.Providers["openai"]; p.Preset != "openai" || p.BaseURL != model.srv.URL+"/v1" || p.KeysEnv[0] != "OPENAI_API_KEY" {
		t.Fatalf("provider = %+v", p)
	}
	ch := cfg.Channels["wagent"]
	if ch == nil || ch.Type != "whatsappagent" || ch.TokenEnv != "WHATSAPP_AGENT_KEY" || ch.APIURL != wa.srv.URL ||
		len(ch.AllowFrom) != 1 || ch.AllowFrom[0] != "15550001" {
		t.Fatalf("channel = %+v", ch)
	}
	if n := model.count(); n != 1 {
		t.Fatalf("model calls = %d, want one tiny check", n)
	}
	// One poll that returns at once and passes no offset, so it skips nothing;
	// and no adapter state is saved.
	if q := wa.polls(); len(q) != 1 || !strings.Contains(q[0], "timeout=0") || strings.Contains(q[0], "offset") {
		t.Fatalf("polls = %v", q)
	}
	if _, err := os.Stat(filepath.Join(home, "channels", "whatsappagent-wagent.json")); err == nil {
		t.Fatal("setup saved poll state")
	}

	env, _ := os.ReadFile(filepath.Join(home, ".env"))
	if string(env) != "# mine\nOTHER=1\nOPENAI_API_KEY=sk-test\nWHATSAPP_AGENT_KEY=wa-key\n" {
		t.Fatalf(".env = %q", env)
	}
	for _, f := range []string{".env", "config.yaml"} {
		if info, _ := os.Stat(filepath.Join(home, f)); plat.HasUnixPermissions && info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v", f, info.Mode().Perm())
		}
	}
	if strings.Contains(out, "sk-test") || strings.Contains(out, "wa-key") {
		t.Fatalf("a key was printed:\n%s", out)
	}

	dir := filepath.Join(home, "agents", "habakkuk")
	if id, ok := agent.ReadIdentity(dir); !ok || id.Name != "Nova" {
		t.Fatalf("identity = %+v", id)
	}
	if got := profileName(dir); got != "Ada" {
		t.Fatalf("USER.md name = %q", got)
	}
	if !agent.NeedsOnboarding(dir) {
		t.Fatal("the welcome was lost: a named agent must still greet its user once")
	}
	if !strings.Contains(out, "your agent answers the message you sent it in WhatsApp") {
		t.Fatalf("next step missing:\n%s", out)
	}
	if r.served {
		t.Fatal("served without -serve")
	}
	raw, _ := os.ReadFile(filepath.Join(home, "config.yaml"))
	for _, want := range []string{"# Habakkuk settings, written by hk setup", "keys_env: [OPENAI_API_KEY]",
		`allow_from: ["15550001"] # you`, "\nchannels:", "\n\n# Your agent."} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("config.yaml lacks %q:\n%s", want, raw)
		}
	}
	if strings.Contains(string(raw), "sk-test") || strings.Contains(string(raw), "wa-key") {
		t.Fatal("a key reached config.yaml")
	}
}

// TestSetupRerunEditsInPlace adds a channel to an existing home: the
// model, the first channel, hand edits and comments stay; a backup is kept.
func TestSetupRerunEditsInPlace(t *testing.T) {
	home := t.TempDir()
	model, wa := newFakeModel(t), newFakeWA(t)
	wa.wait(waHello)
	var tgCalls []string
	tg := newFakeTelegram(t, &tgCalls)
	r := &setupRun{env: map[string]string{"OPENAI_API_KEY": "sk-test", "WHATSAPP_AGENT_KEY": "wa-key", "TELEGRAM_BOT_TOKEN": "123:tg"},
		ep: endpoints{whatsappAgent: wa.srv.URL, telegram: tg.URL}}
	if out, err := r.run(t, home, "-y", "-provider", "openai", "-base-url", model.srv.URL+"/v1", "-model", "gpt-test", "-channel", "whatsappagent"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	// A hand edit: a comment and a setting setup knows nothing about.
	path := filepath.Join(home, "config.yaml")
	raw, _ := os.ReadFile(path)
	edited := strings.Replace(string(raw), "agents:", "# my own note\nretention:\n  days: 30\n\nagents:", 1)
	edited = strings.Replace(edited, "    workspace: ~/habakkuk-workspace", "    workspace: ~/habakkuk-workspace\n    effort: high # thinking", 1)
	os.WriteFile(path, []byte(edited), 0o600)

	// Nothing to do: nothing changes.
	if out, err := r.run(t, home, "-y"); err != nil || !strings.Contains(out, "needs no change") {
		t.Fatalf("%v\n%s", err, out)
	}
	if now, _ := os.ReadFile(path); string(now) != edited {
		t.Fatalf("a run with nothing to do rewrote config.yaml:\n%s", now)
	}

	out, err := r.run(t, home, "channel", "telegram", "-y")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if bak, _ := os.ReadFile(path + ".bak"); string(bak) != edited {
		t.Fatal("config.yaml.bak is not the previous config")
	}
	cfg := mustLoad(t, home)
	if a := cfg.Agents["habakkuk"]; a.Model != "openai/gpt-test" || a.Effort != "high" {
		t.Fatalf("agent changed: %+v", a)
	}
	if cfg.Retention.Days != 30 || cfg.Channels["wagent"] == nil || cfg.Channels["wagent"].AllowFrom[0] != "15550001" {
		t.Fatalf("existing settings lost: retention %d, wagent %+v", cfg.Retention.Days, cfg.Channels["wagent"])
	}
	tch := cfg.Channels["telegram"]
	if tch == nil || tch.TokenEnv != "TELEGRAM_BOT_TOKEN" || len(tch.AllowFrom) != 0 {
		// Without a person to confirm, whoever wrote to the bot is not let in.
		t.Fatalf("telegram = %+v", tch)
	}
	now, _ := os.ReadFile(path)
	for _, want := range []string{"# my own note", "effort: high # thinking", "# Habakkuk settings"} {
		if !strings.Contains(string(now), want) {
			t.Errorf("lost %q:\n%s", want, now)
		}
	}
	// getMe, then a peek at updates without an offset, which confirms none.
	if len(tgCalls) != 2 || !strings.HasSuffix(tgCalls[0], "getMe {}") || strings.Contains(tgCalls[1], "offset") {
		t.Fatalf("telegram calls = %v", tgCalls)
	}
	if !strings.Contains(out, "@nova_bot") || !strings.Contains(out, "pairing code") {
		t.Fatalf("output:\n%s", out)
	}

	// Re-running the same channel with the same key does not poll again.
	before := len(wa.polls())
	if out, err := r.run(t, home, "channel", "whatsappagent", "-y"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(wa.polls()) != before {
		t.Fatal("an unchanged key was polled again (it would knock a running serve off)")
	}

	// A new model on the same provider.
	if out, err := r.run(t, home, "model", "-y", "-model", "gpt-other"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if a := mustLoad(t, home).Agents["habakkuk"]; a.Model != "openai/gpt-other" || a.Effort != "high" {
		t.Fatalf("agent = %+v", a)
	}
}

// TestSetupInteractive answers the prompts: the local server found by the
// probe, a WhatsApp agent key typed in, the default name.
func TestSetupInteractive(t *testing.T) {
	home := t.TempDir()
	model, wa := newFakeModel(t), newFakeWA(t)
	wa.wait(waHello)
	r := &setupRun{env: map[string]string{}, goos: "linux",
		ep: endpoints{whatsappAgent: wa.srv.URL, local: map[string]string{"ollama": model.srv.URL + "/v1"}},
		stdin: strings.Join([]string{
			"1",      // the local server
			"1",      // WhatsApp agent
			"wa-key", // its key
			"",       // name: Habakkuk
			"Ada",    // my name
			"n",      // no background service
			"y",      // start serve
		}, "\n") + "\n"}
	model.set(0, true) // a local server takes no key
	out, err := r.run(t, home)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	cfg := mustLoad(t, home)
	if a := cfg.Agents["habakkuk"]; a.Model != "ollama/llama-test" {
		t.Fatalf("agent = %+v\n%s", a, out)
	}
	if p := cfg.Providers["ollama"]; p.BaseURL != model.srv.URL+"/v1" || len(p.KeysEnv) != 1 || p.KeysEnv[0] != "OLLAMA_API_KEY" {
		t.Fatalf("provider = %+v", p)
	}
	if env := readDotEnv(filepath.Join(home, ".env")); env["WHATSAPP_AGENT_KEY"] != "wa-key" || len(env) != 1 {
		t.Fatalf(".env = %v", env)
	}
	if strings.Contains(out, "wa-key") {
		t.Fatal("the key was printed")
	}
	if _, ok := agent.ReadIdentity(filepath.Join(home, "agents", "habakkuk")); ok {
		t.Fatal("the default name needs no identity file")
	}
	if !agent.NeedsOnboarding(filepath.Join(home, "agents", "habakkuk")) {
		t.Fatal("writing the user's name cost the welcome")
	}
	for _, want := range []string{"Found on this computer: Ollama", "Step 3 of 3", "Your agent answers the message you sent it in WhatsApp now"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if !r.served {
		t.Fatal("serve was not started")
	}
}

func TestSetupFailuresWriteNothing(t *testing.T) {
	model, wa := newFakeModel(t), newFakeWA(t)
	base := []string{"-y", "-provider", "openai", "-base-url", model.srv.URL + "/v1", "-model", "gpt-test", "-timeout", "300ms"}
	for _, tc := range []struct {
		name  string
		env   map[string]string
		args  []string
		setup func()
		want  string
	}{
		{"wrong model key", map[string]string{"OPENAI_API_KEY": "sk-wrong-key"}, base, nil, "refused the key"},
		{"no model key", map[string]string{}, base, nil, "set OPENAI_API_KEY in the environment"},
		{"model too slow", map[string]string{"OPENAI_API_KEY": "sk-test"}, base,
			func() { model.set(2*time.Second, false) }, "no answer from OpenAI within 300ms"},
		{"wrong agent key", map[string]string{"OPENAI_API_KEY": "sk-test", "WHATSAPP_AGENT_KEY": "nope-nope"},
			append(base, "-channel", "whatsappagent"), nil, "WhatsApp refused the API key"},
		{"invalid agent key", map[string]string{"OPENAI_API_KEY": "sk-test", "WHATSAPP_AGENT_KEY": "wa-key"},
			append(base, "-channel", "whatsappagent"), func() { wa.fail(400, 100) }, "WhatsApp refused the API key"},
		{"unknown provider", map[string]string{}, []string{"-y", "-provider", "nosuch"}, nil, "unknown provider"},
		{"nothing found", map[string]string{}, []string{"-y"}, nil, "no model provider found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model.set(0, false)
			wa.fail(0, 0)
			if tc.setup != nil {
				tc.setup()
			}
			home := t.TempDir()
			r := &setupRun{env: tc.env, ep: endpoints{whatsappAgent: wa.srv.URL}}
			out, err := r.run(t, home, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q\n%s", err, tc.want, out)
			}
			for _, v := range tc.env {
				if strings.Contains(err.Error(), v) || strings.Contains(out, v) {
					t.Fatalf("a key was printed: %v\n%s", err, out)
				}
			}
			if entries, _ := os.ReadDir(home); len(entries) != 0 {
				t.Fatalf("a failed setup wrote %v", entries)
			}
		})
	}
}

func TestSetupFlagErrors(t *testing.T) {
	for _, args := range [][]string{
		{"-channel", "fax"}, {"-serve", "-service"}, {"-key-env", "bad name"}, {"bogus"}, {"channel", "a", "b"}, {"model", "x"},
	} {
		if _, err := newSetup(context.Background(), t.TempDir(), args, strings.NewReader(""), io.Discard); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
	// Input that ends early stops the wizard without writing.
	home := t.TempDir()
	r := &setupRun{env: map[string]string{}, stdin: ""}
	if _, err := r.run(t, home); err == nil || !strings.Contains(err.Error(), "input ended") {
		t.Fatalf("err = %v", err)
	}
}

func TestSetupServiceFiles(t *testing.T) {
	testutil.UnixOnly(t, "systemd units and launchd plists hold Unix paths")
	for _, goos := range []string{"linux", "darwin"} {
		home := t.TempDir()
		r := &setupRun{env: map[string]string{"PATH": "/usr/bin:/bin"}, goos: goos}
		out, err := r.run(t, home, "service")
		if err != nil {
			t.Fatalf("%s: %v", goos, err)
		}
		entries, _ := os.ReadDir(home)
		if len(entries) != 1 {
			t.Fatalf("%s: wrote %v", goos, entries)
		}
		data, _ := os.ReadFile(filepath.Join(home, entries[0].Name()))
		text := string(data)
		if !strings.Contains(text, "HABAKKUK_HOME") || !strings.Contains(text, home) || !strings.Contains(text, "/usr/bin:/bin") {
			t.Fatalf("%s:\n%s", goos, text)
		}
		switch goos {
		case "linux":
			if !strings.Contains(text, "ExecStart=") || !strings.Contains(out, "systemctl --user enable --now habakkuk") {
				t.Fatalf("unit:\n%s\n%s", text, out)
			}
		case "darwin":
			if err := xml.Unmarshal(data, new(struct{})); err != nil {
				t.Fatalf("plist is not XML: %v", err)
			}
			if !strings.Contains(out, "launchctl bootstrap gui/$(id -u)") {
				t.Fatal(out)
			}
		}
	}
	r := &setupRun{env: map[string]string{}, goos: "windows"}
	if _, err := r.run(t, t.TempDir(), "service"); err == nil {
		t.Fatal("windows got a service file")
	}
}

func TestSetProfileName(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct{ before, name, after string }{
		{"", "Ada", "## Basics\n\n- Name: Ada\n"},
		{"## Basics\n\n- Name: Ada\n- Lives in Lagos\n", "Ade", "## Basics\n\n- Name: Ade\n- Lives in Lagos\n"},
		{"## Basics\n\n- Lives in Lagos\n", "Ade", "## Basics\n\n- Name: Ade\n- Lives in Lagos\n"},
		{"## Work\n\n- Builds things\n", "Ade", "## Basics\n\n- Name: Ade\n\n## Work\n\n- Builds things\n"},
	} {
		os.WriteFile(filepath.Join(dir, "USER.md"), []byte(tc.before), 0o600)
		if err := setProfileName(dir, tc.name); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(filepath.Join(dir, "USER.md")); string(got) != tc.after {
			t.Errorf("from %q got %q, want %q", tc.before, got, tc.after)
		}
	}
}

func TestSpaceSections(t *testing.T) {
	in := "# head\n\nversion: 1\nproviders:\n  a: {}\n# about agents\nagents:\n  x: 1\n"
	want := "# head\n\nversion: 1\n\nproviders:\n  a: {}\n\n# about agents\nagents:\n  x: 1\n"
	if got := string(spaceSections([]byte(in))); got != want {
		t.Fatalf("got %q", got)
	}
}

func TestRedactHidesKeys(t *testing.T) {
	s := &setup{secrets: map[string]string{"A": "secret-value-1"}}
	err := s.redact(errors.New("bad secret-value-1 and other-secret"), "other-secret")
	if strings.Contains(err.Error(), "secret-value-1") || strings.Contains(err.Error(), "other-secret") {
		t.Fatal(err)
	}
}

func TestSetupPairsFastModel(t *testing.T) {
	for _, c := range []struct {
		choice modelChoice
		want   string
	}{
		{modelChoice{provider: "anthropic", model: "claude-opus-5-5"}, "anthropic/claude-haiku-5-5"},
		{modelChoice{provider: "deepseek", model: "deepseek-v4-pro"}, "deepseek/deepseek-flash"},
		{modelChoice{provider: "anthropic", model: "claude-sonnet-5-5"}, ""}, // the user's own pick
		{modelChoice{provider: "chatgpt", auth: "chatgpt", model: "gpt-5.6-sol"}, ""},
		{modelChoice{provider: "ollama", model: "llama"}, ""},
	} {
		if got := c.choice.fastRef(); got != c.want {
			t.Errorf("%s/%s: fast %q, want %q", c.choice.provider, c.choice.model, got, c.want)
		}
	}
}
