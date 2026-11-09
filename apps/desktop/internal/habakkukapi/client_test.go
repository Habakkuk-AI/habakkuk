package habakkukapi_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/fakehabakkuk"
	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/habakkukapi"
)

func TestReadEvents(t *testing.T) {
	stream := "retry: 3000\n\n: keepalive\n\nid: 1\nevent: message\ndata: {\"a\":1}\n\nevent: typing\ndata: {}\n\ndata: line1\ndata: line2\n\nid: 9\n\n"
	var got []habakkukapi.Event
	err := habakkukapi.ReadEvents(strings.NewReader(stream), func(e habakkukapi.Event) { got = append(got, e) })
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v", err)
	}
	want := []habakkukapi.Event{
		{ID: "1", Name: "message", Data: `{"a":1}`},
		{Name: "typing", Data: "{}"},
		{Name: "message", Data: "line1\nline2"},
	}
	if len(got) != len(want) {
		t.Fatalf("events = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestClient(t *testing.T) {
	f := fakehabakkuk.New("tok")
	defer f.Close()
	ctx := context.Background()
	c := &habakkukapi.Client{BaseURL: f.URL + "/", Token: "tok"}

	info, err := c.App(ctx)
	if err != nil || info.Default != "habakkuk" || info.Account != "web" {
		t.Fatalf("App = %+v, %v", info, err)
	}
	f.AddApproval(habakkukapi.Approval{ID: "a/1", Tool: "exec"})
	list, err := c.Approvals(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("Approvals = %+v, %v", list, err)
	}
	if err := c.Answer(ctx, "a/1", true); err != nil {
		t.Fatal(err)
	}
	if ok, answered := f.Answer("a/1"); !ok || !answered {
		t.Fatal("the answer did not arrive")
	}
	if err := c.Answer(ctx, "a/1", false); err == nil || !strings.Contains(err.Error(), "not waiting") {
		t.Fatalf("second answer = %v", err)
	}
	if err := c.Send(ctx, "quick-1", "habakkuk", "hello"); err != nil {
		t.Fatal(err)
	}
	if s := f.Sent(); len(s) != 1 || s[0]["conversation"] != "quick-1" || s[0]["text"] != "hello" {
		t.Fatalf("sent = %+v", s)
	}
	if f.LastHeader().Get("Content-Type") != "application/json" {
		t.Fatal("changes must be sent as JSON")
	}

	bad := &habakkukapi.Client{BaseURL: f.URL, Token: "nope"}
	if _, err := bad.Overview(ctx); !habakkukapi.IsUnauthorized(err) {
		t.Fatalf("wrong token: %v", err)
	}

	// Follow replays, then streams, and reports where it got to.
	f.Push("c", "user", "q", time.Now())
	f.Push("c", "assistant", "a", time.Now())
	fctx, cancel := context.WithCancel(ctx)
	var seen []string
	done := make(chan int64)
	go func() {
		last, _ := c.Follow(fctx, "c", 1, func(m habakkukapi.Message) {
			seen = append(seen, m.Text)
			if len(seen) == 2 {
				cancel()
			}
		})
		done <- last
	}()
	time.Sleep(50 * time.Millisecond)
	f.Push("c", "assistant", "b", time.Now())
	select {
	case last := <-done:
		if last != 3 || strings.Join(seen, ",") != "a,b" {
			t.Fatalf("last %d, seen %v", last, seen)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Follow did not return")
	}
}
