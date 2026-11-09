package sms

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

// Twilio's published example: token 12345 and these parameters.
func TestSignatureMatchesTwilioExample(t *testing.T) {
	form := url.Values{
		"CallSid": {"CA1234567890ABCDE"}, "Caller": {"+14158675309"}, "Digits": {"1234"},
		"From": {"+14158675309"}, "To": {"+18005551212"},
	}
	const u = "https://mycompany.com/myapp.php?foo=1&bar=2"
	if !Valid("12345", u, form, "RSOYDt4T1cUTdK1PDd93/VVr8B8=") {
		t.Fatalf("signature = %s", base64.StdEncoding.EncodeToString(sign("12345", u, form)))
	}
	form.Set("Digits", "1235")
	if Valid("12345", u, form, "RSOYDt4T1cUTdK1PDd93/VVr8B8=") {
		t.Fatal("a changed parameter still validated")
	}
}

type fakeTwilio struct {
	mu   sync.Mutex
	sent []url.Values
}

func (f *fakeTwilio) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user, pass, _ := r.BasicAuth()
	if user != "AC1" || pass != "tok" {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"code":20003,"message":"Authenticate","status":401}`)
		return
	}
	switch {
	case r.URL.Path == "/2010-04-01/Accounts/AC1.json":
		io.WriteString(w, `{"sid":"AC1","status":"active"}`)
	case r.URL.Path == "/2010-04-01/Accounts/AC1/Messages.json":
		r.ParseForm()
		if r.PostForm.Get("To") == "+15550000000" {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"code":21610,"message":"Attempt to send to unsubscribed recipient","status":400}`)
			return
		}
		f.mu.Lock()
		f.sent = append(f.sent, r.PostForm)
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"sid":"SM1"}`)
	case strings.HasPrefix(r.URL.Path, "/media/"):
		io.WriteString(w, "PNGDATA")
	default:
		http.NotFound(w, r)
	}
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

func start(t *testing.T, a *Adapter) *inbox {
	t.Helper()
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

func post(a *Adapter, form url.Values, sig string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Twilio-Signature", sig)
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w
}

func TestWebhookInSendOut(t *testing.T) {
	api := &fakeTwilio{}
	srv := httptest.NewServer(api)
	defer srv.Close()
	const hook = "https://habakkuk.example.com/channels/sms"
	a := New("AC1", "tok", "+15551112222", "", hook, srv.URL)

	form := url.Values{"From": {"+15553334444"}, "To": {"+15551112222"}, "Body": {"hello"}, "MessageSid": {"SM9"},
		"NumMedia": {"1"}, "MediaUrl0": {srv.URL + "/media/x"}, "MediaContentType0": {"image/png"}}
	sig := base64.StdEncoding.EncodeToString(sign("tok", hook, form))

	if w := post(a, form, sig); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("before Run: %d, want 503 so Twilio retries", w.Code)
	}
	in := start(t, a)
	if w := post(a, form, "bm9wZQ=="); w.Code != http.StatusForbidden {
		t.Fatalf("bad signature: %d", w.Code)
	}
	w := post(a, form, sig)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<Response>") {
		t.Fatalf("webhook: %d %s", w.Code, w.Body)
	}
	in.mu.Lock()
	msgs := in.msgs
	in.mu.Unlock()
	if len(msgs) != 1 {
		t.Fatalf("delivered %d", len(msgs))
	}
	m := msgs[0]
	if m.Conversation != "+15553334444" || m.Sender != "15553334444" || m.ID != "SM9" || m.Text != "hello" || !m.Direct || len(m.Attachments) != 1 {
		t.Fatalf("message = %+v", m)
	}
	data, err := m.Attachments[0].Fetch(context.Background())
	if err != nil || string(data) != "PNGDATA" || m.Attachments[0].Name != "mms-1.png" {
		t.Fatalf("media = %q %v %s", data, err, m.Attachments[0].Name)
	}

	to := channel.Address{Conversation: m.Conversation}
	if err := a.Send(context.Background(), to, a.Format("**Done** see [docs](https://x.io)"), m.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.SendFile(context.Background(), to, "chart.png", "image/png", []byte("IMG"), "chart"); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	sent := api.sent
	api.mu.Unlock()
	if len(sent) != 2 || sent[0].Get("Body") != "Done see docs (https://x.io)" || sent[0].Get("From") != "+15551112222" || sent[0].Get("To") != "+15553334444" {
		t.Fatalf("sent = %v", sent)
	}
	media := sent[1].Get("MediaUrl")
	tok, ok := strings.CutPrefix(media, hook+"/media/")
	if !ok || sent[1].Get("Body") != "chart" {
		t.Fatalf("mms = %v", sent[1])
	}
	r := httptest.NewRequest(http.MethodGet, "/media/"+tok, nil)
	rw := httptest.NewRecorder()
	a.ServeHTTP(rw, r)
	if rw.Code != 200 || rw.Body.String() != "IMG" || rw.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("media served: %d %q", rw.Code, rw.Body)
	}

	err = a.Send(context.Background(), channel.Address{Conversation: "+15550000000"}, "hi", "")
	if !errors.As(err, new(channel.Undeliverable)) {
		t.Fatalf("unsubscribed: %v, want Undeliverable", err)
	}
}

func TestBadCredentialsAreFatal(t *testing.T) {
	srv := httptest.NewServer(&fakeTwilio{})
	defer srv.Close()
	a := New("AC1", "wrong", "+15551112222", "", "https://x/channels/sms", srv.URL)
	err := a.Run(context.Background(), &inbox{up: make(chan string, 1)})
	if !errors.As(err, new(channel.Fatal)) {
		t.Fatalf("err = %v, want Fatal", err)
	}
}
