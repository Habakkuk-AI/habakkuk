package googlechat

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/channel/jwt"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type fakeGoogle struct {
	srv     *httptest.Server
	signer  *rsa.PrivateKey // Google's key for event tokens
	saKey   *rsa.PrivateKey // the service account's key
	mu      sync.Mutex
	posts   []map[string]any
	paths   []string
	granted int
}

func newFake(t *testing.T) *fakeGoogle {
	f := &fakeGoogle{}
	f.signer, _ = rsa.GenerateKey(rand.Reader, 2048)
	f.saKey, _ = rsa.GenerateKey(rand.Reader, 2048)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /certs", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{jwt.JWK("g1", &f.signer.PublicKey)}})
	})
	saKeys := &jwt.Keys{Source: jwt.JWKS}
	mux.HandleFunc("GET /sa-keys", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{jwt.JWK("sa1", &f.saKey.PublicKey)}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		claims, _, err := jwt.Verify(r.Context(), r.PostForm.Get("assertion"), saKeys, time.Now())
		if err != nil || claims.String("scope") != botScope || claims.String("iss") != "habakkuk@p.iam.gserviceaccount.com" {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		f.mu.Lock()
		f.granted++
		f.mu.Unlock()
		io.WriteString(w, `{"access_token":"ya29.tok","expires_in":3600}`)
	})
	mux.HandleFunc("POST /v1/spaces/{space}/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ya29.tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.PathValue("space") == "GONE" {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":{"status":"NOT_FOUND","message":"no space"}}`)
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.posts = append(f.posts, body)
		f.paths = append(f.paths, r.URL.RequestURI())
		f.mu.Unlock()
		io.WriteString(w, `{"name":"spaces/S/messages/new"}`)
	})
	mux.HandleFunc("GET /v1/media/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ya29.tok" || r.URL.Query().Get("alt") != "media" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		io.WriteString(w, "IMAGE:"+strings.TrimPrefix(r.URL.Path, "/v1/media/"))
	})
	f.srv = httptest.NewServer(mux)
	saKeys.URL = f.srv.URL + "/sa-keys"
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGoogle) credentials() string {
	der, _ := x509.MarshalPKCS8PrivateKey(f.saKey)
	b, _ := json.Marshal(map[string]string{
		"type": "service_account", "client_email": "habakkuk@p.iam.gserviceaccount.com", "private_key_id": "sa1",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"token_uri":   f.srv.URL + "/token",
	})
	return string(b)
}

func (f *fakeGoogle) idToken(claims map[string]any) string {
	claims["exp"] = time.Now().Add(time.Hour).Unix()
	tok, _ := jwt.Sign(claims, f.signer, "g1")
	return tok
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

func newAdapter(t *testing.T, f *fakeGoogle, audience string) (*Adapter, *inbox) {
	a := New(audience, f.credentials(), f.srv.URL, quiet)
	a.Keys = &jwt.Keys{URL: f.srv.URL + "/certs", Source: jwt.JWKS}
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
	return a, in
}

func deliver(a *Adapter, tok string, ev string) int {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(ev))
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w.Code
}

const roomEvent = `{"type":"MESSAGE","space":{"name":"spaces/S","type":"ROOM","spaceType":"SPACE"},
"message":{"name":"spaces/S/messages/M1","text":"@Habakkuk summarize the doc","argumentText":" summarize the doc",
"sender":{"name":"users/42","displayName":"Ay","email":"Ay@Example.com","type":"HUMAN"},
"thread":{"name":"spaces/S/threads/T1"},
"annotations":[{"type":"USER_MENTION","userMention":{"user":{"name":"users/app","type":"BOT"}}}],
"attachment":[{"contentName":"pic.png","contentType":"image/png","attachmentDataRef":{"resourceName":"res-1"}}]}}`

const addonDM = `{"commonEventObject":{"hostApp":"CHAT"},"chat":{"messagePayload":{
"space":{"name":"spaces/D","spaceType":"DIRECT_MESSAGE"},
"message":{"name":"spaces/D/messages/M2","text":"hi","sender":{"name":"users/42","type":"HUMAN"},"thread":{"name":"spaces/D/threads/T2"}}}}}`

func TestEndpointURLAudience(t *testing.T) {
	f := newFake(t)
	const aud = "https://habakkuk.example.com/channels/gchat"
	a, in := newAdapter(t, f, aud)
	good := f.idToken(map[string]any{"iss": "https://accounts.google.com", "aud": aud, "email": chatIssuer, "email_verified": true})
	for name, tok := range map[string]string{
		"other audience": f.idToken(map[string]any{"iss": "https://accounts.google.com", "aud": "https://evil", "email": chatIssuer, "email_verified": true}),
		"other account":  f.idToken(map[string]any{"iss": "https://accounts.google.com", "aud": aud, "email": "someone@gmail.com", "email_verified": true}),
		"unverified":     f.idToken(map[string]any{"iss": "https://accounts.google.com", "aud": aud, "email": chatIssuer}),
		"no token":       "",
	} {
		if code := deliver(a, tok, roomEvent); code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", name, code)
		}
	}
	if code := deliver(a, good, roomEvent); code != http.StatusOK {
		t.Fatalf("room event: %d", code)
	}
	if code := deliver(a, good, addonDM); code != http.StatusOK {
		t.Fatalf("add-on event: %d", code)
	}
	if code := deliver(a, good, `{"type":"ADDED_TO_SPACE","space":{"name":"spaces/S"}}`); code != http.StatusOK {
		t.Fatalf("added event: %d", code)
	}
	in.mu.Lock()
	msgs := in.msgs
	in.mu.Unlock()
	if len(msgs) != 2 {
		t.Fatalf("delivered %d: %+v", len(msgs), msgs)
	}
	m := msgs[0]
	if m.Conversation != "spaces/S" || m.Thread != "spaces/S/threads/T1" || m.ID != "spaces/S/messages/M1" || m.Direct ||
		!m.Mentioned || m.Text != "summarize the doc" || m.Sender != "users/42" || m.SenderAlt != "ay@example.com" {
		t.Fatalf("room message = %+v", m)
	}
	data, err := m.Attachments[0].Fetch(context.Background())
	if err != nil || string(data) != "IMAGE:res-1" {
		t.Fatalf("attachment = %q %v", data, err)
	}
	if d := msgs[1]; d.Conversation != "spaces/D" || !d.Direct || d.Text != "hi" {
		t.Fatalf("dm = %+v", d)
	}

	if err := a.Send(context.Background(), channel.Address{Conversation: "spaces/S", Thread: "spaces/S/threads/T1"},
		a.Format("**Done**, see [it](https://x.io)"), m.ID); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	post, path, granted := f.posts[0], f.paths[0], f.granted
	f.mu.Unlock()
	if post["text"] != "*Done*, see <https://x.io|it>" || post["thread"].(map[string]any)["name"] != "spaces/S/threads/T1" ||
		path != "/v1/spaces/S/messages?messageReplyOption=REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD" {
		t.Fatalf("post %v to %s", post, path)
	}
	if granted != 1 {
		t.Fatalf("token fetched %d times, want 1 (cached)", granted)
	}
	err = a.Send(context.Background(), channel.Address{Conversation: "spaces/GONE"}, "hi", "")
	if !errors.As(err, new(channel.Undeliverable)) {
		t.Fatalf("gone space: %v", err)
	}
}

func TestProjectNumberAudience(t *testing.T) {
	f := newFake(t)
	a, in := newAdapter(t, f, "123456789")
	if code := deliver(a, f.idToken(map[string]any{"iss": "https://accounts.google.com", "aud": "123456789"}), roomEvent); code != http.StatusUnauthorized {
		t.Fatalf("wrong issuer for project-number tokens: %d", code)
	}
	if code := deliver(a, f.idToken(map[string]any{"iss": chatIssuer, "aud": "123456789"}), roomEvent); code != http.StatusOK {
		t.Fatalf("chat token: %d", code)
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if len(in.msgs) != 1 {
		t.Fatalf("delivered %d", len(in.msgs))
	}
}

func TestBadCredentialsAreFatal(t *testing.T) {
	a := New("123", "{}", "", quiet)
	if err := a.Run(context.Background(), &inbox{up: make(chan string, 1)}); !errors.As(err, new(channel.Fatal)) {
		t.Fatalf("err = %v, want Fatal", err)
	}
}
