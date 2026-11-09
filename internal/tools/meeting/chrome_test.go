package meeting

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/tools/browser"
)

// TestChromeAttendsFixtures runs whole attendances in a real Chrome
// against the simulated platforms: launcher pages, name entry, muting,
// the lobby, the chat announcement, turning captions on through menus,
// reading captions that grow and get corrected, and the call ending.
func TestChromeAttendsFixtures(t *testing.T) {
	bin := os.Getenv("HABAKKUK_TEST_BROWSER")
	if _, err := browser.FindBinary(bin); err != nil {
		t.Skip("no Chrome or Chromium to test with:", err)
	}
	if testing.Short() {
		t.Skip("starts Chrome")
	}
	var mu sync.Mutex
	reports := map[string][]map[string]any{} // "<platform> <path>" -> bodies
	mux := http.NewServeMux()
	for _, path := range []string{"/joined", "/chat", "/left"} {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			var v map[string]any
			_ = json.Unmarshal(body, &v)
			mu.Lock()
			reports[r.URL.Query().Get("p")+" "+path] = append(reports[r.URL.Query().Get("p")+" "+path], v)
			mu.Unlock()
		})
	}
	srv := fixtureSite(t, mux)
	u, _ := url.Parse(srv.URL)
	guard := browser.NewGuard(nil, nil, []string{u.Host})

	script := []string{
		"Good morning everyone, let's review the budget for the launch.",
		"We decided to move the launch to March fifteenth.",
		"Bob will send the revised plan by Habakkuk.",
	}
	speakers := []string{"Alice Chen", "Bob Ortiz", "Alice Chen"}
	for i, p := range []Platform{GoogleMeet, Zoom, Teams} {
		t.Run(string(p), func(t *testing.T) {
			t.Parallel()
			st := NewStore(t.TempDir())
			var delivered []string
			var dmu sync.Mutex
			m := &Manager{
				Store: st, Cfg: (&config.Config{}).MeetingConfig(), Location: time.UTC,
				Launch: ChromeLauncher(bin, true, guard, nil), Tick: 100 * time.Millisecond, Settle: 2 * time.Second,
				Model: func(ctx context.Context, agent, system, prompt string) (string, error) {
					return "Summary: budget review.", nil
				},
				Deliver: func(ctx context.Context, id string, to json.RawMessage, text string) error {
					dmu.Lock()
					delivered = append(delivered, text)
					dmu.Unlock()
					return nil
				},
			}
			rec := &Record{
				ID: "mt_0000000" + string(rune('1'+i)), Agent: "habakkuk", Origin: chatAddr, URL: srv.URL + "/" + string(p),
				JoinURL: srv.URL + "/" + string(p), Platform: p, Name: "Habakkuk (notetaker)", LimitMS: 120_000,
				Status: StatusQueued, Created: time.Now(),
			}
			if err := st.Create(rec); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			job := &jobs.Job{ID: "meeting_" + rec.ID, Kind: Kind, Payload: json.RawMessage(`{"meeting":"` + rec.ID + `"}`)}
			if _, err := m.Handle(ctx, job, "w1"); err != nil {
				r, _ := st.Get(rec.ID)
				lines, _ := st.Lines(rec.ID)
				t.Fatalf("%v; record %+v; transcript %q", err, r, texts(lines))
			}
			rec, _ = st.Get(rec.ID)
			if rec.Status != StatusEnded || !strings.Contains(rec.Detail, "ended") || !rec.Captions || !rec.Announced {
				t.Fatalf("record: %+v", rec)
			}
			lines, _ := st.Lines(rec.ID)
			var want []string
			for j, s := range script {
				if p == Zoom {
					want = append(want, s) // Zoom's captions carry no names
				} else {
					want = append(want, speakers[j]+": "+s)
				}
			}
			if got := texts(lines); strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("transcript:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
			mu.Lock()
			joined, chat := reports[simTitle(p)+" /joined"], reports[simTitle(p)+" /chat"]
			mu.Unlock()
			if len(joined) != 1 || joined[0]["name"] != "Habakkuk (notetaker)" || joined[0]["mic"] != false || joined[0]["cam"] != false {
				t.Errorf("joined: %v", joined)
			}
			if len(chat) != 1 || chat[0]["text"] != "Habakkuk (notetaker) has joined to take notes." {
				t.Errorf("chat: %v", chat)
			}
			dmu.Lock()
			if len(delivered) != 1 || !strings.Contains(delivered[0], "Summary: budget review.") {
				t.Errorf("delivered: %q", delivered)
			}
			dmu.Unlock()
			if _, err := os.Stat(st.BrowserDir(rec.ID)); !os.IsNotExist(err) {
				t.Errorf("the browser profile was left behind: %v", err)
			}
		})
	}
}

// simTitle is the title the simulator reports under.
func simTitle(p Platform) string {
	return map[Platform]string{GoogleMeet: "Meet", Zoom: "Zoom", Teams: "Teams"}[p]
}

// TestChromeLeavesOnRequest checks the leave button path and a denied
// lobby in a real browser.
func TestChromeLeavesAndDenied(t *testing.T) {
	bin := os.Getenv("HABAKKUK_TEST_BROWSER")
	if _, err := browser.FindBinary(bin); err != nil {
		t.Skip("no Chrome or Chromium to test with:", err)
	}
	if testing.Short() {
		t.Skip("starts Chrome")
	}
	var mu sync.Mutex
	left := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/left", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		left++
		mu.Unlock()
	})
	mux.HandleFunc("/joined", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("/chat", func(w http.ResponseWriter, r *http.Request) {})
	srv := fixtureSite(t, mux)
	u, _ := url.Parse(srv.URL)
	guard := browser.NewGuard(nil, nil, []string{u.Host})
	cfg := (&config.Config{}).MeetingConfig()
	off := false
	cfg.Announce = &off

	for _, tc := range []struct {
		name, query string
		status      Status
		detail      string
	}{
		{"leave", "?stay", StatusEnded, "asked to leave"},
		{"denied", "?deny", StatusFailed, "denied access"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := NewStore(t.TempDir())
			m := &Manager{Store: st, Cfg: cfg, Location: time.UTC, Launch: ChromeLauncher(bin, true, guard, nil), Tick: 100 * time.Millisecond}
			rec := &Record{ID: "mt_00000009", Agent: "habakkuk", URL: srv.URL + "/teams" + tc.query, JoinURL: srv.URL + "/teams" + tc.query,
				Platform: Teams, Name: "Habakkuk (notetaker)", LimitMS: 120_000, Status: StatusQueued, Created: time.Now()}
			if err := st.Create(rec); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := m.Handle(ctx, &jobs.Job{ID: "j", Payload: json.RawMessage(`{"meeting":"mt_00000009"}`)}, "w1")
				done <- err
			}()
			if tc.name == "leave" {
				for {
					r, _ := st.Get(rec.ID)
					if lines, _ := st.Lines(rec.ID); r.Captions && len(lines) > 0 {
						break
					}
					select {
					case <-ctx.Done():
						t.Fatal("never read captions")
					case <-time.After(100 * time.Millisecond):
					}
				}
				if err := st.RequestLeave(rec.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			rec, _ = st.Get(rec.ID)
			if rec.Status != tc.status || !strings.Contains(rec.Detail, tc.detail) || rec.Announced {
				t.Fatalf("record: %+v", rec)
			}
			if tc.name == "leave" {
				mu.Lock()
				defer mu.Unlock()
				if left != 1 {
					t.Fatalf("pressed leave %d times", left)
				}
			}
		})
	}
}
