package msteams

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/channel/jwt"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type posted struct {
	path, auth string
	act        activity
}

type fakeTeams struct {
	srv     *httptest.Server
	key     *rsa.PrivateKey
	mu      sync.Mutex
	posts   []posted
	uploads []string
}

func newFake(t *testing.T) *fakeTeams {
	f := &fakeTeams{}
	f.key, _ = rsa.GenerateKey(rand.Reader, 2048)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{jwt.JWK("k1", &f.key.PublicKey, "msteams")}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.PostForm.Get("client_secret") != "secret" || r.PostForm.Get("scope") != botScope {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":"invalid_client","error_description":"bad secret"}`)
			return
		}
		io.WriteString(w, `{"access_token":"app-token","expires_in":3600}`)
	})
	mux.HandleFunc("POST /v3/conversations/", func(w http.ResponseWriter, r *http.Request) {
		var act activity
		json.NewDecoder(r.Body).Decode(&act)
		f.mu.Lock()
		f.posts = append(f.posts, posted{path: r.URL.EscapedPath(), auth: r.Header.Get("Authorization"), act: act})
		f.mu.Unlock()
		io.WriteString(w, `{"id":"1"}`)
	})
	mux.HandleFunc("PUT /upload", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.uploads = append(f.uploads, r.Header.Get("Content-Range")+" "+string(b))
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeTeams) token(aud, serviceURL string) string {
	tok, _ := jwt.Sign(map[string]any{"iss": issuer, "aud": aud, "serviceurl": serviceURL,
		"exp": time.Now().Add(time.Hour).Unix()}, f.key, "k1")
	return tok
}

func (f *fakeTeams) sent() []posted {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]posted(nil), f.posts...)
}

type inbox struct {
	mu   sync.Mutex
	msgs []channel.Message
	up   chan string
}

func (i *inbox) Deliver(_ context.Context, m channel.Message) {
	i.mu.Lock()
	i.msgs = append(i.msgs, m)
	i.mu.Unlock()
}
func (i *inbox) Beat() {}
func (i *inbox) Connected(d string) {
	select {
	case i.up <- d:
	default:
	}
}
func (i *inbox) all() []channel.Message {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]channel.Message(nil), i.msgs...)
}

func newAdapter(t *testing.T, f *fakeTeams, secret string) *Adapter {
	a := New("app-1", secret, "", filepath.Join(t.TempDir(), "msteams.json"), quiet)
	a.Keys = &jwt.Keys{URL: f.srv.URL + "/keys", Source: jwt.JWKS}
	a.TokenURL = f.srv.URL + "/token"
	return a
}

func start(t *testing.T, a *Adapter) *inbox {
	in := &inbox{up: make(chan string, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errc := make(chan error, 1)
	go func() { errc <- a.Run(ctx, in) }()
	select {
	case <-in.up:
	case err := <-errc:
		t.Fatalf("Run: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("not connected")
	}
	return in
}

func deliver(a *Adapter, tok string, act map[string]any) int {
	body, _ := json.Marshal(act)
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w.Code
}

func TestActivitiesInRepliesOut(t *testing.T) {
	f := newFake(t)
	a := newAdapter(t, f, "secret")
	in := start(t, a)
	svc := f.srv.URL + "/"
	dm := map[string]any{
		"type": "message", "id": "m1", "channelId": "msteams", "serviceUrl": svc, "text": "hello there",
		"from":         map[string]any{"id": "29:user", "name": "Sam", "aadObjectId": "aad-sam"},
		"recipient":    map[string]any{"id": "28:app-1", "name": "Habakkuk"},
		"conversation": map[string]any{"id": "a:dm1", "conversationType": "personal"},
	}
	if code := deliver(a, "", dm); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
	if code := deliver(a, f.token("other-app", svc), dm); code != http.StatusUnauthorized {
		t.Fatalf("other audience: %d", code)
	}
	if code := deliver(a, f.token("app-1", "https://evil.example/"), dm); code != http.StatusUnauthorized {
		t.Fatalf("service URL mismatch: %d", code)
	}
	webchat := map[string]any{}
	for k, v := range dm {
		webchat[k] = v
	}
	webchat["channelId"] = "webchat"
	if code := deliver(a, f.token("app-1", svc), webchat); code != http.StatusUnauthorized {
		t.Fatalf("key not endorsed for the channel: %d", code)
	}
	if code := deliver(a, f.token("app-1", svc), dm); code != http.StatusOK {
		t.Fatalf("valid activity: %d", code)
	}
	channelMsg := map[string]any{
		"type": "message", "id": "m2", "channelId": "msteams", "serviceUrl": svc,
		"text":         "<at>Habakkuk</at> what's on today?",
		"from":         map[string]any{"id": "29:bo", "name": "Bo"},
		"recipient":    map[string]any{"id": "28:app-1"},
		"conversation": map[string]any{"id": "19:chan@thread.tacv2;messageid=m2", "conversationType": "channel", "isGroup": true},
		"entities":     []any{map[string]any{"type": "mention", "text": "<at>Habakkuk</at>", "mentioned": map[string]any{"id": "28:app-1"}}},
	}
	if code := deliver(a, f.token("app-1", svc), channelMsg); code != http.StatusOK {
		t.Fatalf("channel activity: %d", code)
	}
	msgs := in.all()
	if len(msgs) != 2 {
		t.Fatalf("delivered %d", len(msgs))
	}
	if m := msgs[0]; m.Conversation != "a:dm1" || m.Sender != "aad-sam" || m.SenderAlt != "29:user" || !m.Direct || m.Text != "hello there" {
		t.Fatalf("dm = %+v", m)
	}
	if m := msgs[1]; m.Direct || !m.Mentioned || m.Text != "what's on today?" || m.Sender != "29:bo" {
		t.Fatalf("channel = %+v", m)
	}

	ctx := context.Background()
	if err := a.Send(ctx, channel.Address{Conversation: "19:chan@thread.tacv2;messageid=m2"}, "**Nothing**", "m2"); err != nil {
		t.Fatal(err)
	}
	if err := a.Typing(ctx, channel.Address{Conversation: "a:dm1"}); err != nil {
		t.Fatal(err)
	}
	posts := f.sent()
	if len(posts) != 2 {
		t.Fatalf("posted %d", len(posts))
	}
	if p := posts[0]; p.path != "/v3/conversations/19:chan@thread.tacv2%3Bmessageid=m2/activities/m2" ||
		p.auth != "Bearer app-token" || p.act.Text != "**Nothing**" || p.act.TextFormat != "markdown" {
		t.Fatalf("reply = %+v", p)
	}
	if posts[1].act.Type != "typing" {
		t.Fatalf("typing = %+v", posts[1])
	}

	// A fresh adapter (a restart) still knows where the conversation lives.
	b := newAdapter(t, f, "secret")
	b.StatePath = a.StatePath
	if err := b.Send(ctx, channel.Address{Conversation: "a:dm1"}, "after restart", ""); err != nil {
		t.Fatal(err)
	}
	err := b.Send(ctx, channel.Address{Conversation: "a:never-seen"}, "hi", "")
	if !errors.As(err, new(channel.Undeliverable)) {
		t.Fatalf("unknown conversation: %v", err)
	}
}

func TestFileConsentFlow(t *testing.T) {
	f := newFake(t)
	a := newAdapter(t, f, "secret")
	start(t, a)
	svc := f.srv.URL + "/"
	a.rememberService("a:dm1", svc)
	ctx := context.Background()
	if err := a.SendFile(ctx, channel.Address{Conversation: "a:dm1"}, "report.pdf", "application/pdf", []byte("PDF!"), "Q3"); err != nil {
		t.Fatal(err)
	}
	card := f.sent()[0].act.Attachments[0]
	var content struct {
		AcceptContext struct{ ID string } `json:"acceptContext"`
		SizeInBytes   int                 `json:"sizeInBytes"`
	}
	json.Unmarshal(card.Content, &content)
	if card.ContentType != "application/vnd.microsoft.teams.card.file.consent" || content.SizeInBytes != 4 || content.AcceptContext.ID == "" {
		t.Fatalf("consent card = %+v", card)
	}
	invoke := map[string]any{
		"type": "invoke", "name": "fileConsent/invoke", "id": "i1", "channelId": "msteams", "serviceUrl": svc,
		"from": map[string]any{"id": "29:user"}, "recipient": map[string]any{"id": "28:app-1"},
		"conversation": map[string]any{"id": "a:dm1", "conversationType": "personal"},
		"value": map[string]any{"type": "fileUpload", "action": "accept", "context": map[string]any{"id": content.AcceptContext.ID},
			"uploadInfo": map[string]any{"name": "report.pdf", "uploadUrl": f.srv.URL + "/upload", "contentUrl": "https://onedrive/report.pdf",
				"uniqueId": "u1", "fileType": "pdf"}},
	}
	if code := deliver(a, f.token("app-1", svc), invoke); code != http.StatusOK {
		t.Fatalf("invoke: %d", code)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(f.sent()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	f.mu.Lock()
	uploads := f.uploads
	f.mu.Unlock()
	if len(uploads) != 1 || uploads[0] != "bytes 0-3/4 PDF!" {
		t.Fatalf("uploads = %q", uploads)
	}
	if info := f.sent()[1].act.Attachments[0]; info.ContentType != "application/vnd.microsoft.teams.card.file.info" || info.ContentURL != "https://onedrive/report.pdf" {
		t.Fatalf("file card = %+v", info)
	}
	err := a.SendFile(ctx, channel.Address{Conversation: "19:chan@thread.tacv2"}, "r.pdf", "application/pdf", []byte("x"), "")
	if !errors.As(err, new(channel.Undeliverable)) {
		t.Fatalf("file to a channel: %v", err)
	}
}

func TestBadSecretIsFatal(t *testing.T) {
	f := newFake(t)
	err := newAdapter(t, f, "wrong").Run(context.Background(), &inbox{up: make(chan string, 1)})
	if !errors.As(err, new(channel.Fatal)) {
		t.Fatalf("err = %v, want Fatal", err)
	}
}
