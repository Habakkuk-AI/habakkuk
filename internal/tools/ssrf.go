package tools

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// ErrPrivateURL is returned for a fetch that would reach a loopback,
// private, link-local or metadata address.
var ErrPrivateURL = errors.New("private address")

var (
	cgnatNet     = netip.MustParsePrefix("100.64.0.0/10") // carrier NAT; also Alibaba's metadata service
	thisNetwork  = netip.MustParsePrefix("0.0.0.0/8")     // "this network"
	benchmarkNet = netip.MustParsePrefix("198.18.0.0/15") // device benchmarking
	ietfNet      = netip.MustParsePrefix("192.0.0.0/24")  // IETF protocol assignments
	nat64Net     = netip.MustParsePrefix("64:ff9b::/96")  // NAT64: an IPv4 address inside IPv6
	sixToFour    = netip.MustParsePrefix("2002::/16")     // 6to4: an IPv4 address inside IPv6
	metadataV6   = netip.MustParseAddr("fd00:ec2::254")   // AWS metadata over IPv6
	metadataV4   = netip.MustParseAddr("169.254.169.254") // AWS, GCP, Azure, ...
	metadataHost = []string{"metadata.google.internal", "metadata", "instance-data", "metadata.azure.internal"}
)

// PrivateKind names why an address is not a public host ("loopback",
// "private", "link-local", "metadata", ...), or "" when it is public.
func PrivateKind(a netip.Addr) string {
	a = a.Unmap()
	if a.Is6() && (nat64Net.Contains(a) || sixToFour.Contains(a)) {
		b := a.As16()
		var v4 [4]byte
		if nat64Net.Contains(a) {
			copy(v4[:], b[12:16])
		} else {
			copy(v4[:], b[2:6])
		}
		if k := PrivateKind(netip.AddrFrom4(v4)); k != "" {
			return k
		}
	}
	switch {
	case a == metadataV4 || a == metadataV6:
		return "cloud metadata"
	case a.IsLoopback():
		return "loopback"
	case a.IsPrivate(), cgnatNet.Contains(a), benchmarkNet.Contains(a), ietfNet.Contains(a):
		return "private"
	case a.IsLinkLocalUnicast(), a.IsLinkLocalMulticast():
		return "link-local"
	case a.IsUnspecified(), thisNetwork.Contains(a):
		return "unspecified"
	case a.IsMulticast(), a.IsInterfaceLocalMulticast():
		return "multicast"
	}
	return ""
}

func privateURLError(host, kind string) error {
	return fmt.Errorf("%w: %s is a %s address; fetch_url only reaches public hosts unless tools.allow_private_urls is set", ErrPrivateURL, host, kind)
}

// CheckURL refuses URLs that name a private host outright. The dialer
// checks again after DNS resolution and on every redirect, which is what
// stops a public name that resolves to a private address.
func CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("url must start with http:// or https://")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return errors.New("url has no host")
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return privateURLError(host, "loopback")
	}
	for _, m := range metadataHost {
		if host == m {
			return privateURLError(host, "cloud metadata")
		}
	}
	if a, err := netip.ParseAddr(host); err == nil {
		if k := PrivateKind(a); k != "" {
			return privateURLError(host, k)
		}
	}
	return nil
}

// guardControl runs on every connection the guarded client makes, after
// name resolution, so neither DNS rebinding nor a redirect can reach a
// private address.
func guardControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("%w: unresolved address %s", ErrPrivateURL, host)
	}
	if k := PrivateKind(a); k != "" {
		return privateURLError(host, k)
	}
	return nil
}

// GuardedClient is an HTTP client that only connects to public addresses.
// It ignores proxy settings, since through a proxy the target's address
// cannot be checked.
func GuardedClient(timeout time.Duration) *http.Client {
	d := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second, Control: guardControl}
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           d.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{Timeout: timeout, Transport: tr, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return CheckURL(req.URL.String())
	}}
}
