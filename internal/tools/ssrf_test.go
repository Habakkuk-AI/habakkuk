package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

func TestPrivateKind(t *testing.T) {
	for addr, want := range map[string]string{
		"127.0.0.1":            "loopback",
		"::1":                  "loopback",
		"10.1.2.3":             "private",
		"172.16.0.1":           "private",
		"192.168.1.1":          "private",
		"100.100.100.200":      "private", // CGNAT; Alibaba metadata
		"169.254.169.254":      "cloud metadata",
		"fd00:ec2::254":        "cloud metadata",
		"169.254.10.10":        "link-local",
		"fe80::1":              "link-local",
		"0.0.0.0":              "unspecified",
		"::ffff:127.0.0.1":     "loopback",
		"64:ff9b::a9fe:a9fe":   "cloud metadata", // NAT64 of 169.254.169.254
		"2002:7f00:1::":        "loopback",       // 6to4 of 127.0.0.1
		"fc00::1":              "private",
		"8.8.8.8":              "",
		"2606:4700:4700::1111": "",
		"93.184.216.34":        "",
	} {
		if got := PrivateKind(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s: got %q, want %q", addr, got, want)
		}
	}
}

func TestCheckURL(t *testing.T) {
	for _, u := range []string{
		"http://localhost:8080/",
		"http://app.localhost/",
		"http://127.0.0.1/",
		"http://[::1]:9000/x",
		"http://169.254.169.254/latest/meta-data/",
		"http://metadata.google.internal/computeMetadata/v1/",
		"http://10.0.0.5/admin",
		"http://0.0.0.0:22",
	} {
		if err := CheckURL(u); !errors.Is(err, ErrPrivateURL) {
			t.Errorf("%s: %v", u, err)
		}
	}
	for _, u := range []string{"https://example.com/", "http://93.184.216.34/"} {
		if err := CheckURL(u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	if err := CheckURL("file:///etc/passwd"); err == nil {
		t.Error("file URL allowed")
	}
}

// The guarded client refuses at connect time, so a public-looking URL that
// resolves (or redirects) to a private address cannot get through.
func TestGuardedClientRefusesPrivateDial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("secret")) }))
	defer srv.Close()
	_, err := GuardedClient(5 * time.Second).Get(srv.URL)
	if err == nil || !errors.Is(err, ErrPrivateURL) {
		t.Fatalf("got %v, want a private address error", err)
	}
}

func TestFetchURLGuard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("internal data"))
	}))
	defer srv.Close()
	r := NewRegistry()
	RegisterBuiltins(r)
	in, _ := json.Marshal(map[string]string{"url": srv.URL})
	call := llm.Block{Type: llm.ToolUse, ToolUseID: "f1", ToolName: "fetch_url", Input: in}

	env := &Env{Workspace: t.TempDir(), Mode: ModeFull}
	res := r.Execute(context.Background(), env, call)
	if !res.IsError || !strings.Contains(res.Text, "tools.allow_private_urls") {
		t.Fatalf("guarded fetch: %+v", res)
	}

	env.AllowPrivateURLs = true
	res = r.Execute(context.Background(), env, call)
	if res.IsError || !strings.Contains(res.Text, "internal data") {
		t.Fatalf("allowed fetch: %+v", res)
	}
}

// A redirect from a public page to a private address is refused too.
func TestGuardedClientChecksRedirects(t *testing.T) {
	c := GuardedClient(5 * time.Second)
	req, _ := http.NewRequest(http.MethodGet, "http://169.254.169.254/latest/", nil)
	if err := c.CheckRedirect(req, []*http.Request{{}}); !errors.Is(err, ErrPrivateURL) {
		t.Fatalf("redirect to metadata: %v", err)
	}
}
