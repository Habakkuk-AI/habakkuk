package browser

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Guard decides which URLs the browser may load. Every request of every tab
// Habakkuk drives passes through it (CDP Fetch interception), so a page cannot
// reach a loopback or private address by redirect, frame, script or form
// either.
type Guard struct {
	allowed      []string // pages; empty allows any
	blocked      []string // pages and subresources
	allowPrivate []string // host patterns exempt from the private-address check

	// Lookup resolves a host name; nil uses the system resolver.
	Lookup func(ctx context.Context, host string) ([]netip.Addr, error)

	mu    sync.Mutex
	cache map[string]cachedAddrs
}

type cachedAddrs struct {
	addrs []netip.Addr
	until time.Time
}

func NewGuard(allowed, blocked, allowPrivate []string) *Guard {
	return &Guard{allowed: allowed, blocked: blocked, allowPrivate: allowPrivate, cache: map[string]cachedAddrs{}}
}

// Check returns nil when u may load. page is true for documents (top-level
// pages and frames), which the allowed list applies to; subresources only
// meet the blocked list and the private-address check.
func (g *Guard) Check(ctx context.Context, raw string, page bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%q is not a valid URL", raw)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "ws", "wss":
	case "about", "data", "blob":
		return nil // no network
	case "file":
		return fmt.Errorf("file URLs are not allowed; read local files with read_file")
	default:
		return fmt.Errorf("the %s: scheme is not allowed", u.Scheme)
	}
	host := strings.ToLower(u.Hostname())
	hostPort := host
	if u.Port() != "" {
		hostPort = net.JoinHostPort(host, u.Port())
	}
	for _, p := range g.blocked {
		if matchURL(p, raw, host, hostPort) {
			return fmt.Errorf("%s is blocked by the browser's blocked_urls", trimURL(raw))
		}
	}
	if page && len(g.allowed) > 0 {
		ok := false
		for _, p := range g.allowed {
			if matchURL(p, raw, host, hostPort) {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("%s is not in the browser's allowed_urls", trimURL(raw))
		}
	}
	g.mu.Lock() // tests widen the list while pages load
	exempt := g.allowPrivate
	g.mu.Unlock()
	for _, p := range exempt {
		if matchHost(p, host, hostPort) {
			return nil
		}
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return privateErr(raw, "loopback")
	}
	addrs, err := g.resolve(ctx, host)
	if err != nil {
		return nil // the browser cannot resolve it either
	}
	for _, a := range addrs {
		if kind := privateKind(a); kind != "" {
			return privateErr(raw, kind)
		}
	}
	return nil
}

func privateErr(raw, kind string) error {
	return fmt.Errorf("%s is a %s address, which the browser may not reach unless the host is listed in browser.allow_private", trimURL(raw), kind)
}

// Private reports whether raw points at a loopback, private or link-local
// address, whatever browser.allow_private says: channel turns need approval
// to open such a page even where the owner allowed the host.
func (g *Guard) Private(ctx context.Context, raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "ws", "wss":
	default:
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	addrs, err := g.resolve(ctx, host)
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if privateKind(a) != "" {
			return true
		}
	}
	return false
}

func trimURL(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

func (g *Guard) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if a, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return []netip.Addr{a}, nil
	}
	now := time.Now()
	g.mu.Lock()
	c, ok := g.cache[host]
	g.mu.Unlock()
	if ok && now.Before(c.until) {
		return c.addrs, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var addrs []netip.Addr
	var err error
	if g.Lookup != nil {
		addrs, err = g.Lookup(ctx, host)
	} else {
		var ips []net.IPAddr
		ips, err = net.DefaultResolver.LookupIPAddr(ctx, host)
		for _, ip := range ips {
			if a, ok := netip.AddrFromSlice(ip.IP); ok {
				addrs = append(addrs, a)
			}
		}
	}
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	if len(g.cache) > 4096 {
		clear(g.cache)
	}
	g.cache[host] = cachedAddrs{addrs: addrs, until: now.Add(30 * time.Second)}
	g.mu.Unlock()
	return addrs, nil
}

var (
	cgnat     = netip.MustParsePrefix("100.64.0.0/10")
	thisNet   = netip.MustParsePrefix("0.0.0.0/8")
	benchmark = netip.MustParsePrefix("198.18.0.0/15")
)

// privateKind names why a reaches no public host, or "" when it is public.
func privateKind(a netip.Addr) string {
	a = a.Unmap()
	switch {
	case a.IsLoopback():
		return "loopback"
	case a.IsPrivate(), cgnat.Contains(a), benchmark.Contains(a):
		return "private"
	case a.IsLinkLocalUnicast(), a.IsLinkLocalMulticast():
		return "link-local"
	case a.IsUnspecified(), thisNet.Contains(a):
		return "unspecified"
	case a.IsMulticast(), a.IsInterfaceLocalMulticast():
		return "multicast"
	}
	return ""
}

// matchURL matches a pattern against the whole URL when it contains "://",
// otherwise against the host.
func matchURL(pattern, raw, host, hostPort string) bool {
	if strings.Contains(pattern, "://") {
		return glob(strings.ToLower(pattern), strings.ToLower(raw))
	}
	return matchHost(pattern, host, hostPort)
}

// matchHost matches host, or host:port when the pattern names a port.
// "*.example.com" also matches example.com itself.
func matchHost(pattern, host, hostPort string) bool {
	pattern = strings.ToLower(pattern)
	target := host
	if _, err := netip.ParseAddr(strings.Trim(pattern, "[]")); err != nil && strings.Contains(pattern, ":") {
		target = hostPort // the pattern names a port
	}
	if glob(pattern, target) {
		return true
	}
	if rest, ok := strings.CutPrefix(pattern, "*."); ok {
		return glob(rest, target)
	}
	return false
}

// glob matches s against a pattern in which * stands for any run of
// characters (including none) and everything else is literal.
func glob(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for _, p := range parts[1 : len(parts)-1] {
		i := strings.Index(s, p)
		if i < 0 {
			return false
		}
		s = s[i+len(p):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}
