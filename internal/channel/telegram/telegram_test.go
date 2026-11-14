package telegram

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

type fakeAPI struct {
	mu      sync.Mutex
	updates []string // JSON update objects served once
	sent    []map[string]any
	files   []map[string]string // multipart uploads: form fields plus method, field, name, data
	token   string
}

// upload records a multipart file send. A photo whose bytes are "WIDE" is
// refused like an image with extreme dimensions.
func (f *fakeAPI) upload(w http.ResponseWriter, r *http.Request, method string) {
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: bad form"}`))
		return
	}
	got := map[string]string{"method": method}
	for k, v := range r.MultipartForm.Value {
		got[k] = v[0]
	}
	for field, hs := range r.MultipartForm.File {
		fh, _ := hs[0].Open()
		data, _ := io.ReadAll(fh)
		fh.Close()
		got["field"], got["name"], got["data"] = field, hs[0].Filename, string(data)
	}
	switch {
	case got["parse_mode"] == "HTML" && strings.Contains(got["caption"], "<bad"):
		w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities"}`))
		return
	case method == "sendPhoto" && got["data"] == "WIDE":
		w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: PHOTO_INVALID_DIMENSIONS"}`))
		return
	}
	f.mu.Lock()
	f.files = append(f.files, got)
	f.mu.Unlock()
	w.Write([]byte(`{"ok":true,"result":{}}`))
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/file/bot"+f.token+"/") {
		w.Write([]byte("JPEGDATA:" + strings.TrimPrefix(r.URL.Path, "/file/bot"+f.token+"/photos/")))
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/bot"+f.token+"/") {
		w.Write([]byte(`{"ok":false,"error_code":401,"description":"Unauthorized"}`))
		return
	}
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		f.upload(w, r, method)
		return
	}
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch method {
	case "getMe":
		w.Write([]byte(`{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"Habakkuk","username":"habakkuk_bot"}}`))
	case "getFile":
		w.Write([]byte(`{"ok":true,"result":{"file_path":"photos/` + body["file_id"].(string) + `"}}`))
	case "getUpdates":
		ups := "[" + strings.Join(f.updates, ",") + "]"
		f.updates = nil
		w.Write([]byte(`{"ok":true,"result":` + ups + `}`))
	case "sendMessage":
		if body["parse_mode"] == "HTML" && strings.Contains(body["text"].(string), "<bad") {
			w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities"}`))
			return
		}
		if body["chat_id"] == float64(404) {
			w.Write([]byte(`{"ok":false,"error_code":403,"description":"Forbidden: bot was blocked by the user"}`))
			return
		}
		f.sent = append(f.sent, body)
		w.Write([]byte(`{"ok":true,"result":{}}`))
	default:
		w.Write([]byte(`{"ok":true,"result":true}`))
	}
}

type inbox struct {
	mu   sync.Mutex
	msgs []channel.Message
	conn string
}

func (i *inbox) Deliver(_ context.Context, m channel.Message) {
	i.mu.Lock()
	i.msgs = append(i.msgs, m)
	i.mu.Unlock()
}
func (i *inbox) Beat()              {}
func (i *inbox) Connected(d string) { i.mu.Lock(); i.conn = d; i.mu.Unlock() }

func TestRunReceivesMessages(t *testing.T) {
	api := &fakeAPI{token: "T", updates: []string{
		`{"update_id":1,"message":{"message_id":10,"from":{"id":7,"first_name":"Sam"},"chat":{"id":7,"type":"private"},"text":"hello"}}`,
		`{"update_id":2,"message":{"message_id":11,"from":{"id":8,"first_name":"Bo"},"chat":{"id":-5,"type":"supergroup"},"text":"hey @Habakkuk_Bot what's up"}}`,
		`{"update_id":3,"message":{"message_id":12,"from":{"id":8,"first_name":"Bo"},"chat":{"id":-5,"type":"group"},"text":"just chatting"}}`,
		`{"update_id":4,"message":{"message_id":13,"from":{"id":8,"first_name":"Bo"},"chat":{"id":-5,"type":"group"},"photo":[{"file_id":"small","file_size":10},{"file_id":"big","file_size":20}],"caption":"look","reply_to_message":{"message_id":3,"from":{"id":99,"is_bot":true},"chat":{"id":-5,"type":"group"}}}}`,
		`{"update_id":5,"message":{"message_id":14,"from":{"id":50,"is_bot":true,"first_name":"Other"},"chat":{"id":-5,"type":"group"},"text":"bot noise"}}`,
	}}
	srv := httptest.NewServer(api)
	defer srv.Close()
	a := New("T", srv.URL)
	in := &inbox{}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	a.Run(ctx, in)
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.conn != "@habakkuk_bot" || len(in.msgs) != 4 {
		t.Fatalf("connected=%q msgs=%+v", in.conn, in.msgs)
	}
	if m := in.msgs[0]; !m.Direct || m.Sender != "7" || m.Text != "hello" || m.ID != "10" {
		t.Fatalf("dm: %+v", m)
	}
	if m := in.msgs[1]; m.Direct || !m.Mentioned || m.Text != "hey  what's up" {
		t.Fatalf("mention: %+v", m)
	}
	if m := in.msgs[2]; m.Mentioned {
		t.Fatalf("plain group message marked as mention: %+v", m)
	}
	m := in.msgs[3]
	if !m.Mentioned || m.Text != "look" || len(m.Attachments) != 1 || m.Attachments[0].MediaType != "image/jpeg" {
		t.Fatalf("reply with photo: %+v", m)
	}
	data, err := m.Attachments[0].Fetch(context.Background())
	if err != nil || string(data) != "JPEGDATA:big" {
		t.Fatalf("download: %q %v", data, err)
	}
}

func TestSend(t *testing.T) {
	api := &fakeAPI{token: "T"}
	srv := httptest.NewServer(api)
	defer srv.Close()
	a := New("T", srv.URL)
	ctx := context.Background()
	if err := a.Send(ctx, channel.Address{Conversation: "7", Thread: "3"}, "<b>hi</b>", "10"); err != nil {
		t.Fatal(err)
	}
	if err := a.Send(ctx, channel.Address{Conversation: "7"}, "<bad markup", ""); err != nil {
		t.Fatal(err)
	}
	if err := a.Send(ctx, channel.Address{Conversation: "404"}, "x", ""); !errors.As(err, new(channel.Undeliverable)) {
		t.Fatalf("blocked chat: %v", err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	first := api.sent[0]
	if first["parse_mode"] != "HTML" || first["message_thread_id"] != float64(3) || first["reply_parameters"] == nil {
		t.Fatalf("first send: %v", first)
	}
	if second := api.sent[1]; second["parse_mode"] != nil || second["text"] != "<bad markup" {
		t.Fatalf("fallback send: %v", second)
	}
}

func TestSendFileMultipart(t *testing.T) {
	api := &fakeAPI{token: "T"}
	srv := httptest.NewServer(api)
	defer srv.Close()
	a := New("T", srv.URL)
	ctx := context.Background()
	to := channel.Address{Conversation: "7", Thread: "3"}
	if err := a.SendFile(ctx, to, "chart.png", "image/png", []byte("PNGDATA"), "<b>Q3</b>"); err != nil {
		t.Fatal(err)
	}
	if err := a.SendFile(ctx, channel.Address{Conversation: "7"}, "report.pdf", "application/pdf", []byte("%PDF"), ""); err != nil {
		t.Fatal(err)
	}
	if err := a.SendFile(ctx, channel.Address{Conversation: "7"}, "wide.jpg", "image/jpeg", []byte("WIDE"), "<bad caption"); err != nil {
		t.Fatal(err)
	}
	if err := a.SendFile(ctx, channel.Address{Conversation: "x"}, "a.txt", "text/plain", []byte("a"), ""); !errors.As(err, new(channel.Undeliverable)) {
		t.Fatalf("bad chat id: %v", err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.files) != 3 {
		t.Fatalf("uploads: %v", api.files)
	}
	photo := api.files[0]
	if photo["method"] != "sendPhoto" || photo["field"] != "photo" || photo["name"] != "chart.png" || photo["data"] != "PNGDATA" ||
		photo["chat_id"] != "7" || photo["message_thread_id"] != "3" || photo["caption"] != "<b>Q3</b>" || photo["parse_mode"] != "HTML" {
		t.Fatalf("photo: %v", photo)
	}
	if doc := api.files[1]; doc["method"] != "sendDocument" || doc["field"] != "document" || doc["caption"] != "" {
		t.Fatalf("document: %v", doc)
	}
	// The refused photo went again as a document, with the caption as plain text.
	if wide := api.files[2]; wide["method"] != "sendDocument" || wide["parse_mode"] != "" || wide["caption"] != "<bad caption" {
		t.Fatalf("fallback: %v", wide)
	}
}

func TestBadTokenIsFatalAndNotLogged(t *testing.T) {
	srv := httptest.NewServer(&fakeAPI{token: "right"})
	defer srv.Close()
	err := New("wrong-secret", srv.URL).Run(context.Background(), &inbox{})
	if !errors.As(err, new(channel.Fatal)) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "wrong-secret") {
		t.Fatal("token leaked into the error")
	}
	err = New("tok-secret", "http://127.0.0.1:1").call(context.Background(), "getMe", struct{}{}, nil)
	if err == nil || strings.Contains(err.Error(), "tok-secret") {
		t.Fatalf("network error leaked the token: %v", err)
	}
}
