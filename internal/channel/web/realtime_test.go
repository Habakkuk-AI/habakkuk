package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// fakeRealtime is the provider's client secret endpoint.
type fakeRealtime struct {
	ts     *httptest.Server
	got    chan map[string]any
	status int
}

func newFakeRealtime(t *testing.T) *fakeRealtime {
	f := &fakeRealtime{got: make(chan map[string]any, 4), status: http.StatusOK}
	f.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/realtime/client_secrets" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-live-key" || r.Header.Get("Content-Type") != "application/json" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":{"message":"bad key"}}`)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.got <- body
		if f.status != http.StatusOK {
			w.WriteHeader(f.status)
			io.WriteString(w, `{"error":{"message":"model not available"}}`)
			return
		}
		io.WriteString(w, `{"value":"ek_test123","expires_at":1767225600,"session":{"type":"realtime"}}`)
	}))
	t.Cleanup(f.ts.Close)
	return f
}

func liveRig(t *testing.T) (*dashRig, *fakeRealtime) {
	r := newDashRig(t)
	f := newFakeRealtime(t)
	c := &config.Config{Realtime: &config.Realtime{BaseURL: f.ts.URL + "/v1"}}
	if err := applyVoice(c); err != nil {
		t.Fatal(err)
	}
	r.s.Dash.Config.Realtime = c.Realtime
	r.s.Dash.Secret = func(name string) string {
		if name == "OPENAI_API_KEY" {
			return "sk-live-key"
		}
		return ""
	}
	return r, f
}

// applyVoice fills Live mode's defaults the way config loading does.
func applyVoice(c *config.Config) error {
	data := "version: 1\nproviders: {p: {type: openai, keys_env: [K]}}\nagents: {habakkuk: {model: p/m}}\nrealtime:\n  base_url: " +
		c.Realtime.BaseURL + "\n"
	got, err := config.Parse([]byte(data))
	if err != nil {
		return err
	}
	c.Realtime = got.Realtime
	return nil
}

func TestRealtimeSessionMintsAClientSecret(t *testing.T) {
	r := newDashRig(t)
	r.send(t, "POST", "/api/realtime/session", `{"conversation":"c1"}`, http.StatusNotImplemented)
	if r.get(t, "/api/app")["realtime"] != false {
		t.Fatal("app info says realtime without config")
	}

	r, f := liveRig(t)
	if r.get(t, "/api/app")["realtime"] != true {
		t.Fatal("app info misses realtime")
	}
	r.send(t, "POST", "/api/realtime/session", `{"conversation":"bad id!"}`, http.StatusBadRequest)
	r.send(t, "POST", "/api/realtime/session", `{"conversation":"c1","agent":"nobody"}`, http.StatusBadRequest)
	out := r.send(t, "POST", "/api/realtime/session", `{"conversation":"c1","agent":"ops"}`, http.StatusOK)
	if out["client_secret"] != "ek_test123" || out["expires_at"] != float64(1767225600) ||
		out["calls_url"] != f.ts.URL+"/v1/realtime/calls" || out["model"] != "gpt-realtime-2.1" || out["voice"] != "marin" {
		t.Fatalf("session: %v", out)
	}
	body := <-f.got
	sess, _ := body["session"].(map[string]any)
	if sess["type"] != "realtime" || sess["model"] != "gpt-realtime-2.1" || sess["tool_choice"] != "auto" {
		t.Fatalf("session config: %v", sess)
	}
	if !strings.Contains(sess["instructions"].(string), "ask_habakkuk") {
		t.Fatalf("instructions: %v", sess["instructions"])
	}
	audio, _ := sess["audio"].(map[string]any)
	in, _ := audio["input"].(map[string]any)
	outA, _ := audio["output"].(map[string]any)
	if outA["voice"] != "marin" || in["transcription"].(map[string]any)["model"] != "gpt-transcribe" {
		t.Fatalf("audio: %v", audio)
	}
	tl, _ := sess["tools"].([]any)
	if len(tl) != 1 {
		t.Fatalf("tools: %v", tl)
	}
	tool := tl[0].(map[string]any)
	params, _ := tool["parameters"].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "ask_habakkuk" || params["required"].([]any)[0] != "request" {
		t.Fatalf("tool: %v", tool)
	}
	if exp := body["expires_after"].(map[string]any); exp["seconds"] != float64(600) || exp["anchor"] != "created_at" {
		t.Fatalf("expires_after: %v", exp)
	}

	// The page may post its offer to the provider, and nowhere else.
	res, err := http.Get(r.ts.URL + "/app/")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "connect-src 'self' "+f.ts.URL+";") {
		t.Fatalf("csp: %s", csp)
	}

	f.status = http.StatusBadRequest
	if out := r.send(t, "POST", "/api/realtime/session", `{"conversation":"c1"}`, http.StatusBadGateway); !strings.Contains(out["error"].(string), "model not available") {
		t.Fatalf("provider error: %v", out)
	}
	r.s.Dash.Secret = func(string) string { return "" }
	r.send(t, "POST", "/api/realtime/session", `{"conversation":"c1"}`, http.StatusServiceUnavailable)
}

func TestRealtimeToolRunsATurnWithTheTranscript(t *testing.T) {
	r, _ := liveRig(t)
	st := r.open(t, "/api/chat/v1/events")
	defer st.cancel()
	r.send(t, "POST", "/api/realtime/transcript",
		`{"conversation":"v1","lines":[{"role":"user","text":"hi there"},{"role":"assistant","text":"Hello! What can I do?"},{"role":"user","text":"  "}]}`,
		http.StatusOK)
	r.send(t, "POST", "/api/realtime/transcript", `{"conversation":"v1","lines":[{"role":"system","text":"x"}]}`, http.StatusBadRequest)
	if e := st.next(t); e.Role != "user" || e.Text != "(voice) hi there" {
		t.Fatalf("logged: %+v", e)
	}
	if e := st.next(t); e.Role != "assistant" || e.Text != "(voice) Hello! What can I do?" {
		t.Fatalf("logged: %+v", e)
	}

	out := r.send(t, "POST", "/api/realtime/tool",
		`{"conversation":"v1","agent":"ops","call_id":"call_1","request":"check the build"}`, http.StatusOK)
	reply, _ := out["output"].(string)
	if !strings.HasPrefix(reply, "echo: ") || !strings.Contains(reply, "User: hi there") ||
		!strings.Contains(reply, "Voice assistant: Hello! What can I do?") || !strings.HasSuffix(reply, "check the build") {
		t.Fatalf("output: %q", reply)
	}
	if e := st.next(t); e.Role != "user" || e.Text != "(voice) check the build" {
		t.Fatalf("logged request: %+v", e)
	}
	if e := st.next(t); e.Role != "assistant" || e.Text != reply {
		t.Fatalf("logged reply: %+v", e)
	}
	// The turn ran in the conversation's session, with the chosen agent.
	c, err := r.s.Gateway.Store.Conversation(context.Background(), channel.Address{Account: "web", Conversation: "v1"})
	if err != nil || c == nil || c.Agent != "ops" {
		t.Fatalf("conversation: %+v %v", c, err)
	}
	// The lines went to the agent once: the next call carries none.
	out = r.send(t, "POST", "/api/realtime/tool", `{"conversation":"v1","call_id":"call_2","request":"thanks"}`, http.StatusOK)
	if got := out["output"].(string); strings.Contains(got, "hi there") || !strings.HasSuffix(got, "thanks") {
		t.Fatalf("second output: %q", got)
	}
	// A resent call attaches to the first one's turn.
	calls := r.model.count()
	out = r.send(t, "POST", "/api/realtime/tool", `{"conversation":"v1","call_id":"call_2","request":"thanks"}`, http.StatusOK)
	if r.model.count() != calls || !strings.HasSuffix(out["output"].(string), "thanks") {
		t.Fatalf("resent call ran again: %d -> %d, %v", calls, r.model.count(), out)
	}
	r.send(t, "POST", "/api/realtime/tool", `{"conversation":"v1","request":"  "}`, http.StatusBadRequest)
}

func TestRealtimeToolAsksForApprovals(t *testing.T) {
	r, _ := liveRig(t)
	ctx := context.Background()
	g := r.s.Gateway
	sess, err := g.Runner.NewSession(ctx, "habakkuk")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Store.Bind(ctx, channel.Address{Account: "web", Conversation: "ap"}, "habakkuk", sess.ID); err != nil {
		t.Fatal(err)
	}
	answer := make(chan bool, 1)
	go func() {
		ok, _ := g.ApproverFor(sess.ID).Approve(ctx, tools.ApprovalRequest{Tool: "exec", Summary: "rm -rf build/"})
		answer <- ok
	}()
	waitFor(t, "approval", func() bool { _, ok := r.s.approvalFor("ap"); return ok })
	out := r.send(t, "POST", "/api/realtime/tool", `{"conversation":"ap","request":"what's the weather"}`, http.StatusOK)
	if got := out["output"].(string); !strings.Contains(got, "needs approval") || !strings.Contains(got, "rm -rf build/") {
		t.Fatalf("output: %q", got)
	}
	out = r.send(t, "POST", "/api/realtime/tool", `{"conversation":"ap","request":"Yes."}`, http.StatusOK)
	if out["output"] != "Approved." {
		t.Fatalf("answer: %v", out)
	}
	select {
	case ok := <-answer:
		if !ok {
			t.Fatal("denied")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("approver still waiting")
	}
}

func TestRealtimeEndNotesTheTranscriptInTheSession(t *testing.T) {
	r, _ := liveRig(t)
	ctx := context.Background()
	r.send(t, "POST", "/api/realtime/end", `{"conversation":"e1"}`, http.StatusOK) // nothing said: no-op
	r.send(t, "POST", "/api/realtime/transcript",
		`{"conversation":"e1","lines":[{"role":"user","text":"remind me what we said"},{"role":"assistant","text":"Sure."}]}`, http.StatusOK)
	out := r.send(t, "POST", "/api/realtime/end", `{"conversation":"e1","agent":"ops"}`, http.StatusOK)
	sid, _ := out["session"].(string)
	if out["noted"] != float64(2) || sid == "" {
		t.Fatalf("end: %v", out)
	}
	recs, err := r.s.Gateway.Runner.Sessions.Messages(ctx, sid)
	if err != nil || len(recs) != 2 {
		t.Fatalf("messages: %d %v", len(recs), err)
	}
	if recs[0].Message.Role != llm.User || !strings.Contains(recs[0].Message.Text(), "User: remind me what we said\nVoice assistant: Sure.") ||
		recs[1].Message.Role != llm.Assistant {
		t.Fatalf("note: %+v", recs)
	}
	c, _ := r.s.Gateway.Store.Conversation(ctx, channel.Address{Account: "web", Conversation: "e1"})
	if c == nil || c.SessionID != sid || c.Agent != "ops" {
		t.Fatalf("conversation: %+v", c)
	}
	// A typed message afterwards continues the same session.
	r.chat(t, "e1", "and now?")
	waitFor(t, "turn", func() bool {
		recs, _ := r.s.Gateway.Runner.Sessions.Messages(ctx, sid)
		return len(recs) == 4
	})
}

// Live mode's requests are speech, which could be anyone's: their turns are
// not typed, so approval rules never answer for them. A message typed in
// the dashboard's chat is.
func TestLiveTurnsAreNotTyped(t *testing.T) {
	r, _ := liveRig(t)
	ctx := context.Background()
	payload := func(turn string) agent.TurnPayload {
		t.Helper()
		j, err := r.s.Gateway.Queue.Get(ctx, turn)
		if err != nil {
			t.Fatal(err)
		}
		var p agent.TurnPayload
		if err := j.Decode(&p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	r.send(t, "POST", "/api/realtime/tool", `{"conversation":"v9","call_id":"call_9","request":"check the build"}`, http.StatusOK)
	if p := payload(channel.TurnID(channel.Message{Account: "web", Conversation: "v9", ID: "rt-call_9"})); p.Typed {
		t.Fatalf("a Live mode turn is typed: %+v", p)
	}
	out := r.chat(t, "t9", "hello")
	if p := payload(out["turn"].(string)); !p.Typed {
		t.Fatalf("a dashboard chat message is not typed: %+v", p)
	}
}
