package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/platform"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

func jwt(claims map[string]any) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]string{"alg": "none"}) + "." + enc(claims) + ".sig"
}

func noSleep(context.Context, time.Duration) error { return nil }

func newStore(t *testing.T) *Store {
	t.Helper()
	return &Store{Path: filepath.Join(t.TempDir(), "auth.json"), Sleep: noSleep}
}

func TestChatGPTDeviceLogin(t *testing.T) {
	exp := time.Now().Add(time.Hour).Unix()
	access := jwt(map[string]any{"exp": exp})
	id := jwt(map[string]any{"email": "me@example.com", "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct_1"}})
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if body["client_id"] != chatgptClientID {
				t.Errorf("client id %q", body["client_id"])
			}
			io.WriteString(w, `{"device_auth_id":"dev1","user_code":"ABCD-1234","interval":"5"}`)
		case "/api/accounts/deviceauth/token":
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if body["device_auth_id"] != "dev1" || body["user_code"] != "ABCD-1234" {
				t.Errorf("poll body %v", body)
			}
			polls++
			if polls < 3 {
				w.WriteHeader(http.StatusForbidden) // pending
				return
			}
			io.WriteString(w, `{"authorization_code":"code1","code_challenge":"x","code_verifier":"ver1"}`)
		case "/oauth/token":
			r.ParseForm()
			if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.Form.Get("grant_type") != "authorization_code" ||
				r.Form.Get("code") != "code1" || r.Form.Get("code_verifier") != "ver1" ||
				r.Form.Get("redirect_uri") != "http://"+r.Host+"/deviceauth/callback" || r.Form.Get("client_id") != chatgptClientID {
				t.Errorf("exchange form %v", r.Form)
			}
			json.NewEncoder(w).Encode(map[string]any{"id_token": id, "access_token": access, "refresh_token": "rt1", "expires_in": 3600})
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	st := newStore(t)
	st.ChatGPTIssuer = srv.URL
	var shown DeviceCode
	e, err := st.Login(context.Background(), ChatGPT, ChatGPT, func(d DeviceCode) { shown = d })
	if err != nil {
		t.Fatal(err)
	}
	if shown.URL != srv.URL+"/codex/device" || shown.Code != "ABCD-1234" {
		t.Errorf("shown %+v", shown)
	}
	if e.AccessToken != access || e.RefreshToken != "rt1" || e.AccountID != "acct_1" || e.Email != "me@example.com" || e.ExpiresAt.Unix() != exp {
		t.Errorf("entry %+v", e)
	}
	info, err := os.Stat(st.Path)
	if err != nil {
		t.Fatal(err)
	}
	if platform.HasUnixPermissions && info.Mode().Perm() != 0o600 {
		t.Errorf("auth.json mode %v", info.Mode().Perm())
	}
	tok, err := st.Token(context.Background(), ChatGPT)
	if err != nil || tok != access {
		t.Fatalf("token %q, %v", tok, err)
	}
}

func TestChatGPTRefresh(t *testing.T) {
	var calls atomic.Int32
	next := jwt(map[string]any{"exp": time.Now().Add(time.Hour).Unix()})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path != "/oauth/token" || body["grant_type"] != "refresh_token" || body["refresh_token"] != "rt1" {
			t.Errorf("refresh %s %v", r.URL.Path, body)
		}
		time.Sleep(20 * time.Millisecond) // widen the race window
		json.NewEncoder(w).Encode(map[string]any{"access_token": next, "refresh_token": "rt2"})
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "auth.json")
	seed := &Store{Path: path}
	stale := Entry{Kind: ChatGPT, AccessToken: "old", RefreshToken: "rt1", AccountID: "acct_1", ExpiresAt: time.Now().Add(time.Minute)}
	if err := seed.Put(context.Background(), ChatGPT, stale); err != nil {
		t.Fatal(err)
	}
	// Several stores (as if separate processes) refresh at once: the lock
	// and re-read mean the single-use refresh token is spent once.
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st := &Store{Path: path, ChatGPTIssuer: srv.URL}
			tok, err := st.Token(context.Background(), ChatGPT)
			if err != nil || tok != next {
				t.Errorf("token %q, %v", tok, err)
			}
		}()
	}
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Fatalf("refreshed %d times, want 1", n)
	}
	list, _ := seed.List()
	if e := list[ChatGPT]; e.RefreshToken != "rt2" || e.AccountID != "acct_1" {
		t.Fatalf("stored %+v", e)
	}
}

func TestRefreshRejectedNeedsNewLogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		io.WriteString(w, `{"error":"invalid_grant","error_description":"refresh_token_reused"}`)
	}))
	defer srv.Close()
	st := newStore(t)
	st.ChatGPTIssuer = srv.URL
	st.Put(context.Background(), ChatGPT, Entry{Kind: ChatGPT, AccessToken: "old", RefreshToken: "rt", ExpiresAt: time.Now()})
	_, err := st.Token(context.Background(), ChatGPT)
	if !errors.Is(err, ErrLoginExpired) || !strings.Contains(err.Error(), "hk auth login chatgpt") {
		t.Fatalf("err = %v", err)
	}
	if _, err := newStore(t).Token(context.Background(), "chatgpt"); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("missing login: %v", err)
	}
}

func TestCopilotDeviceLoginAndExchange(t *testing.T) {
	polls, exchanges := 0, 0
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		switch r.URL.Path {
		case "/login/device/code":
			if r.Form.Get("client_id") != copilotClientID || r.Form.Get("scope") != "read:user" {
				t.Errorf("device code form %v", r.Form)
			}
			io.WriteString(w, `{"device_code":"dc1","user_code":"WXYZ-0000","verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}`)
		case "/login/oauth/access_token":
			if r.Form.Get("device_code") != "dc1" || r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
				t.Errorf("poll form %v", r.Form)
			}
			polls++
			switch polls {
			case 1:
				io.WriteString(w, `{"error":"authorization_pending"}`)
			case 2:
				io.WriteString(w, `{"error":"slow_down","interval":10}`)
			default:
				io.WriteString(w, `{"access_token":"gho_abc","token_type":"bearer","scope":"read:user"}`)
			}
		case "/copilot_internal/v2/token":
			exchanges++
			if r.Header.Get("Authorization") != "token gho_abc" || r.Header.Get("Copilot-Integration-Id") == "" {
				t.Errorf("exchange headers %v", r.Header)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"token": "tid=1;exp=2", "expires_at": time.Now().Add(30 * time.Minute).Unix(), "refresh_in": 1500,
				"endpoints": map[string]string{"api": "https://api.individual.githubcopilot.com/"},
			})
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	}))
	defer gh.Close()
	var slept []time.Duration
	st := newStore(t)
	st.GitHubURL, st.GitHubAPIURL = gh.URL, gh.URL
	st.Sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	var shown DeviceCode
	e, err := st.Login(context.Background(), Copilot, Copilot, func(d DeviceCode) { shown = d })
	if err != nil {
		t.Fatal(err)
	}
	if shown.Code != "WXYZ-0000" || shown.URL != "https://github.com/login/device" {
		t.Errorf("shown %+v", shown)
	}
	if len(slept) != 3 || slept[0] != 5*time.Second || slept[2] != 10*time.Second {
		t.Errorf("poll waits %v", slept)
	}
	if e.RefreshToken != "gho_abc" || e.AccessToken != "tid=1;exp=2" || e.BaseURL != "https://api.individual.githubcopilot.com" {
		t.Errorf("entry %+v", e)
	}
	// Fresh token: no exchange. Expiring token: one exchange.
	if _, err := st.Token(context.Background(), Copilot); err != nil || exchanges != 1 {
		t.Fatalf("fresh token exchanged again: %d, %v", exchanges, err)
	}
	st.Now = func() time.Time { return time.Now().Add(29 * time.Minute) }
	if _, err := st.Token(context.Background(), Copilot); err != nil || exchanges != 2 {
		t.Fatalf("expiring token not refreshed: %d, %v", exchanges, err)
	}
}

func TestCopilotExchangeErrors(t *testing.T) {
	status := 401
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
	defer gh.Close()
	st := newStore(t)
	st.GitHubAPIURL = gh.URL
	st.Put(context.Background(), Copilot, Entry{Kind: Copilot, RefreshToken: "gho", AccessToken: "x", ExpiresAt: time.Now()})
	if _, err := st.Token(context.Background(), Copilot); !errors.Is(err, ErrLoginExpired) {
		t.Fatalf("401: %v", err)
	}
	status = 403
	if _, err := st.Token(context.Background(), Copilot); err == nil || !strings.Contains(err.Error(), "no Copilot access") {
		t.Fatalf("403: %v", err)
	}
}

func TestCopilotHeaders(t *testing.T) {
	h := CopilotHeaders(llm.Request{Messages: []llm.Message{llm.UserText("hi")}})
	if h["X-Initiator"] != "user" || h["Copilot-Vision-Request"] != "" || h["Copilot-Integration-Id"] == "" {
		t.Errorf("user turn: %v", h)
	}
	h = CopilotHeaders(llm.Request{Messages: []llm.Message{
		{Role: llm.User, Blocks: []llm.Block{{Type: llm.Image}}},
		{Role: llm.User, Blocks: []llm.Block{{Type: llm.ToolResult}}},
	}})
	if h["X-Initiator"] != "agent" || h["Copilot-Vision-Request"] != "true" {
		t.Errorf("agent turn with image: %v", h)
	}
}

func TestLogoutAndList(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	st.Put(ctx, "chatgpt", Entry{Kind: ChatGPT, AccessToken: "a"})
	st.Put(ctx, "copilot", Entry{Kind: Copilot, AccessToken: "b"})
	names, _ := st.Names()
	if strings.Join(names, ",") != "chatgpt,copilot" {
		t.Fatalf("names %v", names)
	}
	if found, err := st.Delete(ctx, "chatgpt"); !found || err != nil {
		t.Fatalf("delete: %v %v", found, err)
	}
	if found, _ := st.Delete(ctx, "chatgpt"); found {
		t.Fatal("deleted twice")
	}
	data, _ := os.ReadFile(st.Path)
	if strings.Contains(string(data), `"a"`) {
		t.Fatal("logged-out token still on disk")
	}
}

func TestParseInterval(t *testing.T) {
	for in, want := range map[string]time.Duration{`"7"`: 7 * time.Second, `3`: 3 * time.Second, `""`: 5 * time.Second, ``: 5 * time.Second} {
		if got := parseInterval(json.RawMessage(in), 5*time.Second); got != want {
			t.Errorf("%s: %v", in, got)
		}
	}
}

// The model router treats only a gone login as an outage; it learns which
// errors those are through LoginGone.
func TestGoneLoginsTellTheRouter(t *testing.T) {
	for _, err := range []error{ErrNotLoggedIn, ErrLoginExpired, fmt.Errorf("chatgpt: %w: 401", ErrLoginExpired)} {
		if !llm.LoginGone(err) {
			t.Errorf("LoginGone(%v) = false", err)
		}
	}
	if llm.LoginGone(errors.New("chatgpt: refresh: 503 Service Unavailable")) {
		t.Error("a passing refresh failure counted as a gone login")
	}
}
