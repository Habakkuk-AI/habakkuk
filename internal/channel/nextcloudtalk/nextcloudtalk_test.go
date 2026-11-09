package nextcloudtalk

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

var secret = strings.Repeat("s3cret", 8)

type fakeTalk struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeTalk) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	json.Unmarshal(raw, &body)
	signed, _ := body["message"].(string)
	if signed == "" {
		signed, _ = body["reaction"].(string)
	}
	if r.Header.Get("OCS-APIRequest") != "true" ||
		r.Header.Get("X-Nextcloud-Talk-Bot-Signature") != Sign(secret, r.Header.Get("X-Nextcloud-Talk-Bot-Random"), []byte(signed)) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+strings.TrimPrefix(r.URL.Path, "/ocs/v2.php/apps/spreed/api/v1/bot/")+" "+string(raw))
	f.mu.Unlock()
	if strings.HasPrefix(r.URL.Path, "/ocs/v2.php/apps/spreed/api/v1/bot/gone/") {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

type inbox struct {
	mu   sync.Mutex
	msgs []channel.Message
}

func (i *inbox) Deliver(_ context.Context, m channel.Message) {
	i.mu.Lock()
	i.msgs = append(i.msgs, m)
	i.mu.Unlock()
}
func (i *inbox) Beat()            {}
func (i *inbox) Connected(string) {}

func hook(a *Adapter, sec, body string) int {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	rnd := strings.Repeat("r", 64)
	r.Header.Set("X-Nextcloud-Talk-Random", rnd)
	r.Header.Set("X-Nextcloud-Talk-Signature", Sign(sec, rnd, []byte(body)))
	r.Header.Set("X-Nextcloud-Talk-Backend", "https://cloud.example.com/")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w.Code
}

func create(actorType, actor, id, content string) string {
	c, _ := json.Marshal(content)
	return `{"type":"Create","actor":{"type":"` + actorType + `","id":"` + actor + `","name":"Ay"},
"object":{"type":"Note","id":"` + id + `","name":"message","content":` + string(c) + `,"mediaType":"text/markdown"},
"target":{"type":"Collection","id":"room1","name":"Team"}}`
}

func TestTalk(t *testing.T) {
	fake := &fakeTalk{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	a := New(srv.URL, secret, "")
	in := &inbox{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = a.Run(ctx, in) }()
	for a.listener.Inbox() == nil {
		time.Sleep(time.Millisecond)
	}

	dm := create("Person", "users/ay", "11", `{"message":"hello","parameters":[]}`)
	if code := hook(a, "wrong", dm); code != http.StatusUnauthorized {
		t.Fatalf("bad signature: %d", code)
	}
	for _, body := range []string{
		dm,
		create("Person", "users/ay", "12", `{"message":"@habakkuk ask {mention-user1} about {file}","parameters":{"mention-user1":{"type":"user","id":"bo","name":"Bo"},"file":{"type":"file","id":"9","name":"r.pdf"}}}`),
		create("Application", "bots/other", "13", `{"message":"from a bot","parameters":[]}`),
		`{"type":"Join","actor":{"type":"Application","id":"bots/habakkuk"},"object":{"type":"Collection","id":"room1"},"target":{"id":"room1"}}`,
	} {
		if code := hook(a, secret, body); code != http.StatusOK {
			t.Fatalf("webhook: %d", code)
		}
	}
	msgs := in.msgs
	if len(msgs) != 2 {
		t.Fatalf("delivered %d: %+v", len(msgs), msgs)
	}
	if m := msgs[0]; m.Conversation != "room1" || m.Sender != "ay" || m.Text != "hello" || m.Mentioned || m.ID != "11" {
		t.Fatalf("first = %+v", m)
	}
	if m := msgs[1]; !m.Mentioned || m.Text != "ask @Bo about [shared file: r.pdf]" {
		t.Fatalf("mention = %+v", m)
	}

	if err := a.Send(channel.WithSendKey(ctx, "job.0"), channel.Address{Conversation: "room1"}, "**hi**", "11"); err != nil {
		t.Fatal(err)
	}
	if err := a.React(ctx, channel.Address{Conversation: "room1"}, "11", "👀"); err != nil {
		t.Fatal(err)
	}
	if err := a.React(ctx, channel.Address{Conversation: "room1"}, "11", "✅"); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	calls := fake.calls
	fake.mu.Unlock()
	if len(calls) != 4 || !strings.HasPrefix(calls[0], `POST room1/message {"message":"**hi**","referenceId":"`) ||
		!strings.Contains(calls[0], `"replyTo":11`) ||
		calls[1] != `POST room1/reaction/11 {"reaction":"👀"}` || calls[2] != `DELETE room1/reaction/11 {"reaction":"👀"}` ||
		calls[3] != `POST room1/reaction/11 {"reaction":"✅"}` {
		t.Fatalf("calls:\n%s", strings.Join(calls, "\n"))
	}
	if err := a.Send(ctx, channel.Address{Conversation: "gone"}, "x", ""); !errors.As(err, new(channel.Undeliverable)) {
		t.Fatalf("gone room: %v", err)
	}
}

func TestShortSecretIsFatal(t *testing.T) {
	if err := New("https://cloud.example.com", "short", "").Run(context.Background(), &inbox{}); !errors.As(err, new(channel.Fatal)) {
		t.Fatalf("err = %v", err)
	}
}
