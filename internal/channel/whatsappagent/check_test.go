package whatsappagent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckPeeksWithoutConsuming(t *testing.T) {
	f := &fakePlatform{polls: []string{`{"entry":[{"changes":[{"field":"messages","value":{"messages":[
		{"from":"user:111","id":"a","timestamp":"1","type":"text","text":{"body":"hi"}},
		{"from":"user:222","id":"b","timestamp":"2","type":"text","text":{"body":"yo"}},
		{"from":"user:111","id":"c","timestamp":"3","type":"text","text":{"body":"again"}}]}}]}],"next_offset":9}`, ""}}
	f.srv = httptest.NewServer(f)
	defer f.srv.Close()
	state := filepath.Join(t.TempDir(), "state.json")
	a := New(Config{Token: "key", APIURL: f.srv.URL, StatePath: state})
	senders, err := a.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(senders, ",") != "111,222" {
		t.Fatalf("senders = %v", senders)
	}
	if q := f.calls[0].query; !strings.Contains(q, "timeout=0") || strings.Contains(q, "offset") {
		t.Fatalf("query = %q", q)
	}
	if _, err := os.Stat(state); err == nil {
		t.Fatal("Check saved state")
	}
	if senders, err := a.Check(context.Background()); err != nil || len(senders) != 0 {
		t.Fatalf("empty poll: %v %v", senders, err)
	}
}

func TestCheckErrors(t *testing.T) {
	f := &fakePlatform{pollCode: http.StatusConflict}
	f.srv = httptest.NewServer(f)
	defer f.srv.Close()
	if _, err := New(Config{Token: "key", APIURL: f.srv.URL}).Check(context.Background()); !errors.Is(err, ErrKeyInUse) {
		t.Fatalf("conflict: %v", err)
	}
	if _, err := New(Config{Token: "wrong", APIURL: f.srv.URL}).Check(context.Background()); !errors.Is(err, ErrBadKey) {
		t.Fatalf("bad key: %v", err)
	}
	if _, err := New(Config{APIURL: f.srv.URL}).Check(context.Background()); err == nil {
		t.Fatal("no key passed")
	}
}
