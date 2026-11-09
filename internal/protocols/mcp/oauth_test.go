package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/llm/auth"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// fakeAS is an OAuth authorization server with dynamic registration, the
// authorization code flow with PKCE, refresh tokens and the device flow.
type fakeAS struct {
	t   *testing.T
	srv *httptest.Server
	mcp string // the protected resource

	mu        sync.Mutex
	clients   map[string]bool
	codes     map[string]string // code -> code_challenge
	refresh   map[string]bool
	valid     map[string]bool // access tokens the MCP server accepts
	n         int
	devicePol int // device polls answered with authorization_pending so far
	grants    []string
}

func newFakeAS(t *testing.T) *fakeAS {
	as := &fakeAS{t: t, clients: map[string]bool{}, codes: map[string]string{}, refresh: map[string]bool{}, valid: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{
			"issuer": as.srv.URL, "authorization_endpoint": as.srv.URL + "/authorize", "token_endpoint": as.srv.URL + "/token",
			"registration_endpoint": as.srv.URL + "/register", "device_authorization_endpoint": as.srv.URL + "/device",
			"response_types_supported": []string{"code"}, "code_challenge_methods_supported": []string{"S256"},
			"scopes_supported": []string{"mcp:tools", "offline_access"},
		})
	})
	mux.HandleFunc("POST /register", func(w http.ResponseWriter, r *http.Request) {
		var meta map[string]any
		_ = json.NewDecoder(r.Body).Decode(&meta)
		if meta["token_endpoint_auth_method"] != "none" || len(meta["redirect_uris"].([]any)) != 1 {
			t.Errorf("registration %v", meta)
		}
		as.mu.Lock()
		as.n++
		id := fmt.Sprintf("client-%d", as.n)
		as.clients[id] = true
		as.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		writeTestJSON(w, map[string]any{"client_id": id, "redirect_uris": meta["redirect_uris"], "token_endpoint_auth_method": "none"})
	})
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		as.mu.Lock()
		known := as.clients[q.Get("client_id")]
		as.mu.Unlock()
		if !known || q.Get("code_challenge_method") != "S256" || q.Get("resource") != as.mcp ||
			!strings.Contains(q.Get("scope"), "offline_access") || !strings.HasPrefix(q.Get("redirect_uri"), "http://127.0.0.1:") {
			t.Errorf("authorize %v", q)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		code := as.issue(func(c string) { as.codes[c] = q.Get("code_challenge") })
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+code+"&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("POST /device", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("resource") != as.mcp {
			t.Errorf("device resource %q", r.Form.Get("resource"))
		}
		writeTestJSON(w, map[string]any{"device_code": "dev-1", "user_code": "ABCD-1234",
			"verification_uri": as.srv.URL + "/activate", "expires_in": 600, "interval": 1})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f := r.Form
		as.mu.Lock()
		defer as.mu.Unlock()
		as.grants = append(as.grants, f.Get("grant_type"))
		if f.Get("resource") != as.mcp {
			t.Errorf("token resource %q", f.Get("resource"))
		}
		fail := func(code string) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
		}
		switch f.Get("grant_type") {
		case "authorization_code":
			challenge, ok := as.codes[f.Get("code")]
			sum := sha256.Sum256([]byte(f.Get("code_verifier")))
			if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
				fail("invalid_grant")
				return
			}
			delete(as.codes, f.Get("code"))
		case "refresh_token":
			if !as.refresh[f.Get("refresh_token")] {
				fail("invalid_grant")
				return
			}
			delete(as.refresh, f.Get("refresh_token")) // rotated
		case "urn:ietf:params:oauth:grant-type:device_code":
			if as.devicePol < 2 {
				as.devicePol++
				fail("authorization_pending")
				return
			}
		default:
			fail("unsupported_grant_type")
			return
		}
		as.n++
		access, rt := fmt.Sprintf("at-%d", as.n), fmt.Sprintf("rt-%d", as.n)
		as.valid[access], as.refresh[rt] = true, true
		writeTestJSON(w, map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": 3600, "refresh_token": rt})
	})
	as.srv = httptest.NewServer(mux)
	t.Cleanup(as.srv.Close)
	return as
}

func (as *fakeAS) issue(store func(string)) string {
	as.mu.Lock()
	defer as.mu.Unlock()
	as.n++
	c := fmt.Sprintf("code-%d", as.n)
	store(c)
	return c
}

// revoke makes the MCP server refuse every token issued so far.
func (as *fakeAS) revoke() {
	as.mu.Lock()
	defer as.mu.Unlock()
	clear(as.valid)
}

func writeTestJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// protectedMCP serves the test MCP server behind a bearer check, with RFC
// 9728 metadata pointing at as.
func protectedMCP(t *testing.T, as *fakeAS) string {
	srv := testServer()
	h := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, nil)
	var base string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{"resource": base + "/mcp", "authorization_servers": []string{as.srv.URL},
			"scopes_supported": []string{"mcp:tools"}})
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		as.mu.Lock()
		ok := as.valid[tok]
		as.mu.Unlock()
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+base+`/.well-known/oauth-protected-resource/mcp", scope="mcp:tools"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	base = ts.URL
	as.mcp = base + "/mcp"
	return as.mcp
}

// browser follows the authorization URL like a browser whose user signs in
// at once: the server redirects to Habakkuk's loopback listener.
func browser(t *testing.T) func(string) error {
	return func(u string) error {
		go func() {
			resp, err := http.Get(u)
			if err != nil {
				t.Errorf("browser: %v", err)
				return
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("browser: callback answered %d", resp.StatusCode)
			}
		}()
		return nil
	}
}

func TestOAuthLoginAndCallsEndToEnd(t *testing.T) {
	as := newFakeAS(t)
	mcpURL := protectedMCP(t, as)
	store := auth.NewStore(t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Without a login the server's tools say how to sign in.
	cfg := &config.MCPServer{URL: mcpURL, Auth: "oauth", Risk: "network"}
	reg := tools.NewRegistry()
	m := New(map[string]*config.MCPServer{"remote": cfg}, reg, func(string) string { return "" }, nil)
	m.Tokens = store
	m.StartTimeout = 5 * time.Second
	m.Start(ctx)
	if st := m.Status()[0]; st.State != "error" || !strings.Contains(st.Error, "habakkuk mcp login remote") {
		t.Fatalf("before login: %+v", st)
	}
	m.Close()

	var said []string
	e, err := store.LoginMCP(ctx, auth.MCPLogin{Server: "remote", URL: mcpURL, Open: browser(t),
		Say: func(s string) { said = append(said, s) }})
	if err != nil {
		t.Fatalf("login: %v (%v)", err, said)
	}
	if e.ClientID == "" || e.RefreshToken == "" || e.Resource != mcpURL || e.TokenURL != as.srv.URL+"/token" ||
		!strings.Contains(e.Scope, "mcp:tools") || !strings.Contains(e.Scope, "offline_access") {
		t.Fatalf("entry %+v", e)
	}
	if !strings.Contains(strings.Join(said, "\n"), as.srv.URL+"/authorize?") {
		t.Fatalf("no URL printed: %v", said)
	}

	h := &harness{reg: tools.NewRegistry()}
	h.m = New(map[string]*config.MCPServer{"t": cfg}, h.reg, func(string) string { return "" }, nil)
	// The login is stored under the config name; this manager calls the
	// server "t", so copy it there.
	if err := store.Put(ctx, auth.MCPLoginName("t"), e); err != nil {
		t.Fatal(err)
	}
	h.m.Tokens = store
	t.Cleanup(h.m.Close)
	h.m.Start(ctx)
	if res := h.run(t, tools.ModeWorkspace, "mcp__t__echo", map[string]any{"text": "hi"}); res.IsError || res.Text != "hi" {
		t.Fatalf("call: %+v", res)
	}

	// The server stops taking the token: the transport's retry refreshes it.
	as.revoke()
	if res := h.run(t, tools.ModeWorkspace, "mcp__t__echo", map[string]any{"text": "again"}); res.IsError || res.Text != "again" {
		t.Fatalf("call after revoke: %+v", res)
	}
	stored, _ := store.List()
	if stored[auth.MCPLoginName("t")].RefreshToken == e.RefreshToken {
		t.Fatal("refresh token was not rotated in auth.json")
	}
	if got := strings.Join(as.grants, ","); got != "authorization_code,refresh_token" {
		t.Fatalf("grants %s", got)
	}
}

func TestOAuthDeviceLoginAndExpiredRefresh(t *testing.T) {
	as := newFakeAS(t)
	mcpURL := protectedMCP(t, as)
	store := auth.NewStore(t.TempDir())
	store.Sleep = func(ctx context.Context, d time.Duration) error { return ctx.Err() }
	ctx := context.Background()
	var said []string
	e, err := store.LoginMCP(ctx, auth.MCPLogin{Server: "remote", URL: mcpURL, Auto: true,
		Say: func(s string) { said = append(said, s) }})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(said, "\n"), "ABCD-1234") || e.AccessToken == "" {
		t.Fatalf("device login: %v %+v", said, e)
	}
	// A refused refresh token means signing in again, named in the error.
	as.mu.Lock()
	clear(as.refresh)
	as.mu.Unlock()
	if err := store.Invalidate(ctx, auth.MCPLoginName("remote"), e.AccessToken); err != nil {
		t.Fatal(err)
	}
	_, err = store.MCPToken(ctx, "remote")
	if err == nil || !strings.Contains(err.Error(), "habakkuk mcp login remote") {
		t.Fatalf("expired refresh: %v", err)
	}
	if _, err := store.Delete(ctx, auth.MCPLoginName("remote")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MCPToken(ctx, "remote"); err == nil || !strings.Contains(err.Error(), "habakkuk mcp login remote") {
		t.Fatalf("logged out: %v", err)
	}
}

func TestOAuthLoginRefusesUnprotectedServer(t *testing.T) {
	srv := testServer()
	ts := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, nil))
	defer ts.Close()
	store := auth.NewStore(t.TempDir())
	_, err := store.LoginMCP(context.Background(), auth.MCPLogin{Server: "x", URL: ts.URL + "/mcp"})
	if err == nil || !strings.Contains(err.Error(), "may not need auth: oauth") {
		t.Fatalf("got %v", err)
	}
}
