package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/events"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/core/session"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// echo answers "echo: <text>"; a message containing "hold" waits until
// the turn is canceled.
type echo struct{}

func (echo) Send(ctx context.Context, _ []llm.Target, req llm.Request, on func(llm.Event)) (*llm.Response, error) {
	text := req.Messages[len(req.Messages)-1].Text()
	if strings.Contains(text, "hold") {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	_, msg, _ := strings.Cut(text, "\n") // drop the time stamp line
	msg = strings.TrimPrefix(msg, headerPrefix)
	return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: "echo: " + msg}}},
		StopReason: llm.StopEnd}, nil
}

const token = "secret-token"

type memContexts struct {
	mu sync.Mutex
	m  map[string][2]string
}

func (c *memContexts) APISession(_ context.Context, key string) (string, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.m[key]
	return v[0], v[1], nil
}

func (c *memContexts) BindAPISession(_ context.Context, key, agent, sessionID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = [2]string{agent, sessionID}
	return nil
}

func (c *memContexts) TouchAPISession(context.Context, string) error { return nil }

func newServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	control, err := store.Open(ctx, filepath.Join(dir, "control.db"), jobs.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	transcripts, err := store.Open(ctx, filepath.Join(dir, "transcripts.db"), session.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close(); transcripts.Close() })
	q := jobs.NewQueue(control)
	spec := &agent.Spec{Name: "habakkuk", Chain: []llm.Target{{Endpoint: "fake", Model: "m"}}, MaxTokens: 1000,
		MaxIterations: 10, Toolsets: []string{"core"}, Mode: tools.ModeWorkspace, Workspace: dir, Dir: dir}
	ops := *spec
	ops.Name = "ops"
	runner := &agent.Runner{
		Agents:   map[string]*agent.Spec{"habakkuk": spec, "ops": &ops},
		Sessions: session.NewStore(transcripts), Queue: q, Models: echo{}, Tools: tools.NewRegistry(), Bus: events.NewBus(), Log: quiet,
		Env: func(s *agent.Spec, _ string) *tools.Env { return &tools.Env{Workspace: s.Workspace, Mode: s.Mode} },
	}
	pool := &jobs.Pool{Queue: q, Handlers: map[string]jobs.Handler{agent.KindTurn: runner.HandleTurn},
		Concurrency: 2, Lease: 2 * time.Second, Poll: 10 * time.Millisecond, Log: quiet}
	runner.Wake = pool.Wake
	pctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); pool.Run(pctx) }()
	srv := &Server{Runner: runner, Agents: map[string]string{"habakkuk": "", "ops": "Runs operations."}, Name: "Habakkuk",
		Version: "test", Token: token, Contexts: &memContexts{m: map[string][2]string{}}, Log: quiet}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { ts.Close(); cancel(); wg.Wait() })
	return srv, ts
}

func post(t *testing.T, url, tok string, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, out
}

func getCard(t *testing.T, url, version string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if version != "" {
		req.Header.Set("A2A-Version", version)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("card %s: %d", url, resp.StatusCode)
	}
	var card map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&card)
	return card
}

// path digs into decoded JSON: keys for objects, ints for arrays.
func path(v any, keys ...any) any {
	for _, k := range keys {
		switch k := k.(type) {
		case string:
			m, _ := v.(map[string]any)
			v = m[k]
		case int:
			a, _ := v.([]any)
			if k >= len(a) {
				return nil
			}
			v = a[k]
		}
	}
	return v
}

func TestAgentCards(t *testing.T) {
	_, ts := newServer(t)
	c := getCard(t, ts.URL+CardPath, "")
	if c["protocolVersion"] != "0.3.0" || c["url"] != ts.URL+"/a2a" || c["preferredTransport"] != "JSONRPC" ||
		path(c, "capabilities", "streaming") != true || path(c, "securitySchemes", "bearer", "scheme") != "bearer" ||
		path(c, "security", 0, "bearer") == nil || len(c["skills"].([]any)) != 2 || path(c, "skills", 1, "id") != "ops" ||
		path(c, "skills", 1, "description") != "Runs operations." || path(c, "skills", 0, "tags") == nil {
		t.Fatalf("0.3 card: %v", c)
	}
	c = getCard(t, ts.URL+CardPath, "1.0")
	if _, has := c["url"]; has || c["protocolVersion"] != nil {
		t.Fatalf("1.0 card has 0.3 fields: %v", c)
	}
	if path(c, "supportedInterfaces", 0, "protocolBinding") != "JSONRPC" || path(c, "supportedInterfaces", 0, "protocolVersion") != "1.0" ||
		path(c, "securitySchemes", "bearer", "httpAuthSecurityScheme", "scheme") != "Bearer" ||
		path(c, "securityRequirements", 0, "schemes", "bearer") == nil {
		t.Fatalf("1.0 card: %v", c)
	}
	c = getCard(t, ts.URL+"/a2a/ops"+CardPath, "")
	if c["name"] != "ops" || c["url"] != ts.URL+"/a2a/ops" || len(c["skills"].([]any)) != 1 {
		t.Fatalf("agent card: %v", c)
	}
}

func TestMessageSendV03Shapes(t *testing.T) {
	_, ts := newServer(t)
	body := `{"jsonrpc":"2.0","id":7,"method":"message/send","params":{"message":{"kind":"message","messageId":"m-1","role":"user",
		"parts":[{"kind":"text","text":"hello there"}]}}}`
	if status, out := post(t, ts.URL+"/a2a", "wrong", body); status != http.StatusUnauthorized || out["error"] == nil {
		t.Fatalf("bad token: %d %v", status, out)
	}
	_, out := post(t, ts.URL+"/a2a", token, body)
	res, _ := out["result"].(map[string]any)
	if out["jsonrpc"] != "2.0" || out["id"] != float64(7) || res["kind"] != "task" || path(res, "status", "state") != "completed" ||
		path(res, "artifacts", 0, "artifactId") != "reply" || path(res, "artifacts", 0, "parts", 0, "kind") != "text" ||
		!strings.Contains(path(res, "artifacts", 0, "parts", 0, "text").(string), "hello there") ||
		path(res, "history", 0, "role") != "user" || path(res, "history", 1, "role") != "agent" || path(res, "history", 1, "kind") != "message" {
		t.Fatalf("result %v", out)
	}
	ctxID, taskID := res["contextId"].(string), res["id"].(string)

	// The same messageId again is the same task.
	_, again := post(t, ts.URL+"/a2a", token, body)
	if path(again, "result", "id") != taskID {
		t.Fatalf("resend made a new task: %v", again)
	}
	// Continue the conversation in the context.
	_, out = post(t, ts.URL+"/a2a", token, `{"jsonrpc":"2.0","id":"x","method":"message/send","params":{"message":{"kind":"message",
		"messageId":"m-2","role":"user","contextId":"`+ctxID+`","parts":[{"kind":"text","text":"second"},{"kind":"data","data":{"n":1}}]}}}`)
	if path(out, "result", "contextId") != ctxID || path(out, "result", "id") == taskID ||
		!strings.Contains(path(out, "result", "artifacts", 0, "parts", 0, "text").(string), `"n":1`) {
		t.Fatalf("continue: %v", out)
	}

	// tasks/get, then errors.
	_, out = post(t, ts.URL+"/a2a", token, `{"jsonrpc":"2.0","id":1,"method":"tasks/get","params":{"id":"`+taskID+`","historyLength":1}}`)
	if path(out, "result", "status", "state") != "completed" || len(path(out, "result", "history").([]any)) != 1 {
		t.Fatalf("get: %v", out)
	}
	for _, c := range []struct {
		body string
		code float64
	}{
		{`{"jsonrpc":"2.0","id":1,"method":"tasks/get","params":{"id":"turn_nope"}}`, CodeTaskNotFound},
		{`{"jsonrpc":"2.0","id":1,"method":"tasks/cancel","params":{"id":"` + taskID + `"}}`, CodeTaskNotCancelable},
		{`{"jsonrpc":"2.0","id":1,"method":"tasks/pushNotificationConfig/set","params":{}}`, CodePushNotSupported},
		{`{"jsonrpc":"2.0","id":1,"method":"nope","params":{}}`, CodeMethodNotFound},
		{`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"message":{"messageId":"m","role":"user","parts":[{"kind":"file","file":{"bytes":"AAAA","mimeType":"image/png"}}]}}}`, CodeContentType},
		{`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"message":{"messageId":"m","role":"user","contextId":"nope","parts":[{"kind":"text","text":"x"}]}}}`, CodeInvalidParams},
		{`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"message":{"messageId":"m","role":"user","taskId":"` + taskID + `","parts":[{"kind":"text","text":"x"}]}}}`, CodeUnsupported},
		{`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"message":{"messageId":"m","role":"user","parts":[{"kind":"text","text":"x"}]},"metadata":{"agent":"ghost"}}}`, CodeInvalidParams},
		{`not json`, CodeParse},
	} {
		_, out := post(t, ts.URL+"/a2a", token, c.body)
		if path(out, "error", "code") != c.code {
			t.Errorf("%s: %v", c.body, out)
		}
	}
}

func TestSendMessageV10ShapesAndCancel(t *testing.T) {
	srv, ts := newServer(t)
	_, out := post(t, ts.URL+"/a2a/ops", token, `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"v1-1",
		"role":"ROLE_USER","parts":[{"text":"hi ops"}]}}}`)
	task := path(out, "result", "task")
	if task == nil || path(task, "kind") != nil || path(task, "status", "state") != "TASK_STATE_COMPLETED" ||
		path(task, "artifacts", 0, "parts", 0, "kind") != nil || !strings.Contains(path(task, "artifacts", 0, "parts", 0, "text").(string), "hi ops") ||
		path(task, "history", 1, "role") != "ROLE_AGENT" {
		t.Fatalf("1.0 send: %v", out)
	}
	j, _ := srv.Runner.Queue.Get(context.Background(), path(task, "id").(string))
	if j.Agent != "ops" {
		t.Fatalf("ran as %s", j.Agent)
	}

	// returnImmediately leaves the task running; CancelTask stops it.
	_, out = post(t, ts.URL+"/a2a", token, `{"jsonrpc":"2.0","id":2,"method":"SendMessage","params":{"message":{"messageId":"v1-2",
		"role":"ROLE_USER","parts":[{"text":"hold on"}]},"configuration":{"returnImmediately":true}}}`)
	id, _ := path(out, "result", "task", "id").(string)
	if st := path(out, "result", "task", "status", "state"); id == "" || (st != "TASK_STATE_SUBMITTED" && st != "TASK_STATE_WORKING") {
		t.Fatalf("returnImmediately: %v", out)
	}
	_, out = post(t, ts.URL+"/a2a", token, `{"jsonrpc":"2.0","id":3,"method":"CancelTask","params":{"id":"`+id+`"}}`)
	if path(out, "result", "status", "state") != "TASK_STATE_CANCELED" {
		t.Fatalf("cancel: %v", out)
	}
	_, out = post(t, ts.URL+"/a2a", token, `{"jsonrpc":"2.0","id":4,"method":"GetTask","params":{"id":"`+id+`"}}`)
	if path(out, "result", "status", "state") != "TASK_STATE_CANCELED" {
		t.Fatalf("get after cancel: %v", out)
	}

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/a2a", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"x"}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("A2A-Version", "9.9")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var verr map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&verr)
	resp.Body.Close()
	if path(verr, "error", "code") != float64(CodeVersionUnsupported) {
		t.Fatalf("version: %v", verr)
	}
}

func TestStreamAndClient(t *testing.T) {
	_, ts := newServer(t)
	ctx := context.Background()
	cl := &Client{Header: http.Header{"Authorization": {"Bearer " + token}}}
	card, err := cl.Card(ctx, ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	if card.Protocol != V10 || card.Endpoint != ts.URL+"/a2a" || len(card.Skills) != 2 || !card.Bearer && len(card.Schemes) != 1 {
		t.Fatalf("card %+v", card)
	}
	for _, v := range []Version{V10, V03} {
		c := *card
		c.Protocol = v
		ev, err := cl.Send(ctx, &c, &Message{Role: "user", Parts: []Part{TextPart("ping " + v.String())}}, true)
		if err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		if ev.Task == nil || ev.Task.State != StateCompleted || !strings.Contains(ev.Task.Text(), "ping "+v.String()) {
			t.Fatalf("%s: %+v", v, ev.Task)
		}
		got, err := cl.GetTask(ctx, &c, ev.Task.ID)
		if err != nil || got.Text() != ev.Task.Text() {
			t.Fatalf("%s get: %+v %v", v, got, err)
		}

		var kinds []string
		var final *StatusUpdate
		err = cl.Stream(ctx, &c, &Message{Role: "user", Parts: []Part{TextPart("stream " + v.String())}}, func(e Event) {
			switch {
			case e.Task != nil:
				kinds = append(kinds, "task")
			case e.Status != nil:
				kinds = append(kinds, "status:"+e.Status.State)
				final = e.Status
			case e.Artifact != nil:
				kinds = append(kinds, "artifact")
				if !e.Artifact.LastChunk || !strings.Contains(partsText(e.Artifact.Artifact.Parts), "stream "+v.String()) {
					t.Errorf("artifact %+v", e.Artifact)
				}
			}
		})
		if err != nil {
			t.Fatalf("%s stream: %v", v, err)
		}
		want := []string{"task", "status:working", "artifact", "status:completed"}
		if !slices.Equal(kinds, want) && !slices.Equal(kinds, []string{"task", "artifact", "status:completed"}) {
			t.Fatalf("%s stream events %v", v, kinds)
		}
		if v == V03 && !final.Final {
			t.Fatalf("0.3 final status not marked final")
		}
	}
}

func TestStreamWireFormat(t *testing.T) {
	_, ts := newServer(t)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/a2a", strings.NewReader(`{"jsonrpc":"2.0","id":"s1","method":"message/stream",
		"params":{"message":{"kind":"message","messageId":"st-1","role":"user","parts":[{"kind":"text","text":"streamed"}]}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("content type %s", resp.Header.Get("Content-Type"))
	}
	data, _ := io.ReadAll(resp.Body)
	var last map[string]any
	n := 0
	for _, chunk := range bytes.Split(data, []byte("\n\n")) {
		line, ok := bytes.CutPrefix(chunk, []byte("data: "))
		if !ok {
			continue
		}
		n++
		if err := json.Unmarshal(line, &last); err != nil {
			t.Fatal(err)
		}
		if last["id"] != "s1" || last["jsonrpc"] != "2.0" {
			t.Fatalf("event %s", line)
		}
	}
	if n < 3 || path(last, "result", "kind") != "status-update" || path(last, "result", "final") != true ||
		path(last, "result", "status", "state") != "completed" {
		t.Fatalf("last event %v (%d events)\n%s", last, n, data)
	}
}

func TestToolsTalkToPeer(t *testing.T) {
	_, ts := newServer(t)
	reg := tools.NewRegistry()
	RegisterTools(reg, &Peers{
		Peers:  map[string]*config.A2APeer{"buddy": {URL: ts.URL, Headers: map[string]string{"Authorization": "Bearer ${BUDDY_TOKEN}"}, Timeout: 10 * time.Second}},
		Secret: func(n string) string { return map[string]string{"BUDDY_TOKEN": token}[n] },
		Poll:   20 * time.Millisecond,
	})
	run := func(env *tools.Env, name string, in any) llm.Block {
		b, _ := json.Marshal(in)
		return reg.Execute(context.Background(), env, llm.Block{Type: llm.ToolUse, ToolUseID: "1", ToolName: name, Input: b})
	}
	env := &tools.Env{Mode: tools.ModeWorkspace}
	if res := run(env, "a2a_discover", map[string]string{"to": "buddy"}); res.IsError || !strings.Contains(res.Text, "Skills:") ||
		!strings.Contains(res.Text, "ops (ops): Runs operations.") {
		t.Fatalf("discover: %+v", res)
	}
	res := run(env, "a2a_send", map[string]any{"to": "buddy", "message": "how are you", "metadata": map[string]string{"agent": "ops"}})
	if res.IsError || !strings.Contains(res.Text, "echo: how are you") || !strings.Contains(res.Text, "state completed") {
		t.Fatalf("send: %+v", res)
	}
	_, ctxID, _ := strings.Cut(res.Text, "context_id ")
	ctxID = strings.TrimSuffix(ctxID, "]")
	res = run(env, "a2a_send", map[string]any{"to": "buddy", "message": "again", "context_id": ctxID})
	if res.IsError || !strings.Contains(res.Text, "context_id "+ctxID) {
		t.Fatalf("continue: %+v", res)
	}
	// A URL that is not a peer: private addresses are refused, and public
	// ones need approval.
	if res := run(env, "a2a_send", map[string]any{"to": ts.URL, "message": "x"}); !res.IsError || !strings.Contains(res.Text, "loopback") {
		t.Fatalf("private url: %+v", res)
	}
	if res := run(env, "a2a_send", map[string]any{"to": "ghost", "message": "x"}); !res.IsError || !strings.Contains(res.Text, "buddy") {
		t.Fatalf("unknown peer: %+v", res)
	}
	if res := run(&tools.Env{Mode: tools.ModeWorkspace, AllowPrivateURLs: true}, "a2a_send", map[string]any{"to": ts.URL, "message": "x"}); !res.IsError ||
		!strings.Contains(res.Text, "needs approval") {
		t.Fatalf("unapproved url: %+v", res)
	}
}

func TestDecodeEventShapes(t *testing.T) {
	for _, raw := range []string{
		`{"kind":"message","messageId":"a","role":"agent","parts":[{"kind":"text","text":"hey"}]}`,
		`{"message":{"messageId":"a","role":"ROLE_AGENT","parts":[{"text":"hey"}]}}`,
	} {
		ev, err := DecodeEvent(json.RawMessage(raw))
		if err != nil || ev.Message == nil || ev.Message.Text() != "hey" || ev.Message.Role != "agent" {
			t.Fatalf("%s: %+v %v", raw, ev, err)
		}
	}
	ev, err := DecodeEvent(json.RawMessage(`{"statusUpdate":{"taskId":"t","contextId":"c","status":{"state":"TASK_STATE_INPUT_REQUIRED"}}}`))
	if err != nil || ev.Status == nil || ev.Status.State != StateInputRequired || !Interrupted(ev.Status.State) {
		t.Fatalf("%+v %v", ev, err)
	}
	p, err := DecodeMessage(json.RawMessage(`{"messageId":"m","role":"user","parts":[{"url":"https://x/y.png","mediaType":"image/png","filename":"y.png"},
		{"kind":"file","file":{"uri":"https://x/z","name":"z"}},{"raw":"aGk=","mediaType":"text/plain"}]}`))
	if err != nil || p.Parts[0].URL != "https://x/y.png" || p.Parts[1].Filename != "z" || string(p.Parts[2].Raw) != "hi" {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestOnlyA2AContexts(t *testing.T) {
	srv, ts := newServer(t)
	// A session from another channel (a chat) cannot be continued over A2A.
	sess, err := srv.Runner.NewSession(context.Background(), "habakkuk")
	if err != nil {
		t.Fatal(err)
	}
	_, out := post(t, ts.URL+"/a2a", token, `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"message":{"messageId":"o-1",
		"role":"user","contextId":"`+sess.ID+`","parts":[{"kind":"text","text":"what did we discuss?"}]}}}`)
	if path(out, "error", "code") != float64(CodeInvalidParams) {
		t.Fatalf("chat session continued: %v", out)
	}
}
