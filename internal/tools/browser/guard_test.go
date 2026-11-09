package browser

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
)

func fakeDNS(records map[string]string) func(context.Context, string) ([]netip.Addr, error) {
	return func(_ context.Context, host string) ([]netip.Addr, error) {
		a, ok := records[host]
		if !ok {
			return nil, errors.New("no such host")
		}
		var out []netip.Addr
		for _, s := range strings.Split(a, ",") {
			out = append(out, netip.MustParseAddr(s))
		}
		return out, nil
	}
}

func TestGuard(t *testing.T) {
	dns := fakeDNS(map[string]string{
		"example.com":        "93.184.215.14",
		"docs.example.com":   "93.184.215.15",
		"intranet.corp":      "10.1.2.3",
		"rebind.example.net": "93.184.215.16,127.0.0.1",
		"metadata.cloud":     "169.254.169.254",
		"cgnat.example":      "100.64.1.1",
		"v6.example":         "2606:4700::1",
		"v6local.example":    "fd00::1",
		"mapped.example":     "::ffff:192.168.1.1",
		"tracker.ads.com":    "93.184.215.17",
	})
	cases := []struct {
		name                        string
		allowed, blocked, allowPriv []string
		url                         string
		page                        bool
		want                        string // substring of the error; "" means allowed
	}{
		{name: "public page", url: "https://example.com/a", page: true},
		{name: "public subresource", url: "https://example.com/x.js"},
		{name: "unresolvable", url: "https://nowhere.invalid/"}, // the browser fails it itself
		{name: "about", url: "about:blank", page: true},
		{name: "data", url: "data:text/html,hi", page: true},
		{name: "file", url: "file:///etc/passwd", page: true, want: "file URLs"},
		{name: "javascript", url: "javascript:alert(1)", page: true, want: "javascript: scheme"},
		{name: "chrome", url: "chrome://settings", page: true, want: "chrome: scheme"},
		{name: "loopback ip", url: "http://127.0.0.1:8080/", page: true, want: "loopback"},
		{name: "loopback v6", url: "http://[::1]/", want: "loopback"},
		{name: "localhost", url: "http://localhost:3000/", page: true, want: "loopback"},
		{name: "sub.localhost", url: "http://app.localhost/", want: "loopback"},
		{name: "private dns", url: "http://intranet.corp/", page: true, want: "private"},
		{name: "any private answer", url: "http://rebind.example.net/", want: "loopback"},
		{name: "metadata", url: "http://metadata.cloud/latest", want: "link-local"},
		{name: "metadata ip", url: "http://169.254.169.254/", want: "link-local"},
		{name: "cgnat", url: "http://cgnat.example/", want: "private"},
		{name: "unspecified", url: "http://0.0.0.0/", want: "unspecified"},
		{name: "public v6", url: "http://v6.example/"},
		{name: "ula v6", url: "http://v6local.example/", want: "private"},
		{name: "v4-mapped v6", url: "http://mapped.example/", want: "private"},
		{name: "websocket", url: "ws://127.0.0.1:9222/devtools", want: "loopback"},

		{name: "allow private host:port", allowPriv: []string{"localhost:3000"}, url: "http://localhost:3000/x", page: true},
		{name: "allow private other port", allowPriv: []string{"localhost:3000"}, url: "http://localhost:4000/", want: "loopback"},
		{name: "allow private host", allowPriv: []string{"intranet.corp"}, url: "http://intranet.corp:8443/"},
		{name: "allow private wildcard", allowPriv: []string{"*.corp"}, url: "http://intranet.corp/"},
		{name: "allow private all", allowPriv: []string{"*"}, url: "http://10.0.0.1/"},
		{name: "allow private ip", allowPriv: []string{"127.0.0.1:*"}, url: "http://127.0.0.1:5555/"},

		{name: "blocked host", blocked: []string{"*.ads.com"}, url: "https://tracker.ads.com/p.gif", want: "blocked_urls"},
		{name: "blocked bare domain by wildcard", blocked: []string{"*.example.com"}, url: "https://example.com/", page: true, want: "blocked_urls"},
		{name: "blocked url pattern", blocked: []string{"https://example.com/admin*"}, url: "https://example.com/admin/users", page: true, want: "blocked_urls"},
		{name: "blocked url pattern other path", blocked: []string{"https://example.com/admin*"}, url: "https://example.com/home", page: true},
		{name: "blocked beats allow private", blocked: []string{"localhost:*"}, allowPriv: []string{"*"}, url: "http://localhost:1/", want: "blocked_urls"},

		{name: "allowlist page", allowed: []string{"*.example.com"}, url: "https://docs.example.com/", page: true},
		{name: "allowlist miss", allowed: []string{"docs.example.com"}, url: "https://example.com/", page: true, want: "allowed_urls"},
		{name: "allowlist ignores subresources", allowed: []string{"docs.example.com"}, url: "https://example.com/app.js"},
		{name: "allowlist still checks private", allowed: []string{"*"}, url: "http://intranet.corp/", page: true, want: "private"},
		{name: "allowlist url pattern", allowed: []string{"https://example.com/docs/*"}, url: "https://EXAMPLE.com/docs/a", page: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := NewGuard(c.allowed, c.blocked, c.allowPriv)
			g.Lookup = dns
			err := g.Check(context.Background(), c.url, c.page)
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case c.want != "" && err == nil:
				t.Fatalf("allowed, want an error with %q", c.want)
			case c.want != "" && !strings.Contains(err.Error(), c.want):
				t.Fatalf("error %q, want %q", err, c.want)
			}
		})
	}
}

func TestGlob(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"*", "", true},
		{"a*", "abc", true},
		{"*c", "abc", true},
		{"a*c", "ac", true},
		{"a*b*c", "aXbYc", true},
		{"a*b*b", "ab", false},
		{"a*ab", "ab", false},
		{"abc", "abcd", false},
		{"*.example.com", "a.b.example.com", true},
	}
	for _, c := range cases {
		if got := glob(c.pattern, c.s); got != c.want {
			t.Errorf("glob(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}

func TestGuardCachesLookups(t *testing.T) {
	n := 0
	g := NewGuard(nil, nil, nil)
	g.Lookup = func(context.Context, string) ([]netip.Addr, error) {
		n++
		return []netip.Addr{netip.MustParseAddr("93.184.215.14")}, nil
	}
	for range 3 {
		if err := g.Check(context.Background(), "https://example.com/", true); err != nil {
			t.Fatal(err)
		}
	}
	if n != 1 {
		t.Fatalf("%d lookups, want 1", n)
	}
}
