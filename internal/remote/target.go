package remote

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Kind is a remote's transport.
type Kind int

const (
	SSH  Kind = iota + 1 // any command, over the system ssh client
	HTTP                 // status and chat, over the server's HTTP API
)

func (k Kind) String() string {
	switch k {
	case SSH:
		return "ssh"
	case HTTP:
		return "http"
	}
	return "unknown"
}

// Target is a parsed remote target.
type Target struct {
	Kind Kind
	User string   // ssh: may be empty (ssh's config decides)
	Host string   // ssh: a name, an address or a Host alias from ~/.ssh/config
	Port int      // ssh: 0 for the default
	URL  *url.URL // http: the base URL, without a trailing slash
}

// ParseTarget reads ssh://[user@]host[:port], a bare [user@]host (taken as
// ssh), https://host[:port][/path], or http:// to a loopback address (a
// tunnel; a token must not cross a network in the clear).
func ParseTarget(s string) (Target, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Target{}, errors.New("empty target")
	}
	if !strings.Contains(s, "://") {
		s = "ssh://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return Target{}, fmt.Errorf("target %q: %w", s, err)
	}
	switch u.Scheme {
	case "ssh":
		return parseSSH(u)
	case "https", "http":
		return parseHTTP(u)
	}
	return Target{}, fmt.Errorf("target %q: use ssh://user@host[:port] or https://host", s)
}

func parseSSH(u *url.URL) (Target, error) {
	t := Target{Kind: SSH, Host: u.Hostname()}
	if u.User != nil {
		if _, ok := u.User.Password(); ok {
			return t, errors.New("ssh target: no passwords in the target; use a key (-identity) or ssh-agent")
		}
		t.User = u.User.Username()
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return t, fmt.Errorf("ssh target %q: only ssh://user@host[:port]", u.Redacted())
	}
	if err := sshWord("host", t.Host); err != nil {
		return t, err
	}
	if t.User != "" {
		if err := sshWord("user", t.User); err != nil {
			return t, err
		}
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return t, fmt.Errorf("ssh target: bad port %q", p)
		}
		t.Port = n
	}
	return t, nil
}

// sshWord rejects what ssh would read as an option or what does not belong
// in a user or host name.
func sshWord(what, v string) error {
	switch {
	case v == "":
		return fmt.Errorf("ssh target: no %s", what)
	case strings.HasPrefix(v, "-"):
		return fmt.Errorf("ssh target: %s %q starts with '-'", what, v)
	case strings.ContainsAny(v, " \t\r\n\x00'\"`$;&|<>\\@"):
		return fmt.Errorf("ssh target: %s %q has a character ssh does not accept", what, v)
	}
	return nil
}

func parseHTTP(u *url.URL) (Target, error) {
	if u.Host == "" {
		return Target{}, fmt.Errorf("target %q: no host", u.Redacted())
	}
	if u.User != nil {
		return Target{}, errors.New("http target: no user or password in the URL; put the token in a variable and add -token-env NAME")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return Target{}, fmt.Errorf("http target %q: no query or fragment", u.Redacted())
	}
	if u.Scheme == "http" && !loopback(u.Hostname()) {
		return Target{}, fmt.Errorf("http target %q: plain http only to a loopback address (a tunnel); use https", u.Redacted())
	}
	base := *u
	base.Path = strings.TrimRight(base.Path, "/")
	base.RawPath = ""
	return Target{Kind: HTTP, URL: &base}, nil
}

func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Dest is the destination argument for ssh: [user@]host.
func (t Target) Dest() string {
	if t.User == "" {
		return t.Host
	}
	return t.User + "@" + t.Host
}

// String is the canonical form stored in the registry.
func (t Target) String() string {
	switch t.Kind {
	case SSH:
		u := url.URL{Scheme: "ssh", Host: t.Host}
		if strings.Contains(t.Host, ":") {
			u.Host = "[" + t.Host + "]"
		}
		if t.Port != 0 {
			u.Host = net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
		}
		if t.User != "" {
			u.User = url.User(t.User)
		}
		return u.String()
	case HTTP:
		return t.URL.String()
	}
	return ""
}
