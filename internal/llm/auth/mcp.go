package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// MCP OAuth follows the MCP authorization spec: the server's 401 names its
// protected resource metadata (RFC 9728), which names the authorization
// server, whose metadata (RFC 8414) gives the endpoints. Habakkuk registers
// itself dynamically (RFC 7591) unless the config names a client, signs in
// with the authorization code flow and PKCE through a loopback redirect
// (RFC 8252), or the device flow (RFC 8628) when there is no browser, and
// sends the server's URL as the resource indicator (RFC 8707). Discovery
// and registration use the MCP Go SDK's oauthex helpers; the SDK's own
// AuthorizationCodeHandler keeps tokens in memory only, so Habakkuk stores
// the client and tokens in auth.json and refreshes them under the file lock
// like any other login.

// MCPLoginName is the auth.json key of an MCP server's login.
func MCPLoginName(server string) string { return MCP + ":" + server }

// MCPLogin describes an OAuth sign-in to a remote MCP server.
type MCPLogin struct {
	Server       string   // config name; the login is stored as MCPLoginName(Server)
	URL          string   // the MCP endpoint
	ClientID     string   // a pre-registered client; empty registers one dynamically
	ClientSecret string   // that client's secret, if any
	Scopes       []string // scopes to request; empty takes what the server asks for
	Port         int      // loopback port for the redirect; 0 picks a free one
	// Device uses the device flow instead of a browser redirect. With
	// Auto, the device flow is used only when the authorization server
	// offers it.
	Device bool
	Auto   bool
	// Open opens a URL in a browser; nil (or an error) leaves the user to
	// open the printed URL.
	Open func(string) error
	// Say prints a line for the user.
	Say func(string)
}

// ErrNoBrowserFlow means the sign-in needs a browser on this machine: the
// authorization server offers no device flow.
var ErrNoBrowserFlow = errors.New("the authorization server offers no device sign-in")

// mcpServerMeta is what discovery found.
type mcpServerMeta struct {
	resource string
	scopes   []string
	asm      *oauthex.AuthServerMeta
	device   string // device_authorization_endpoint, if offered
}

func (l *MCPLogin) say(format string, args ...any) {
	if l.Say != nil {
		l.Say(fmt.Sprintf(format, args...))
	}
}

// LoginMCP runs the sign-in and stores the login.
func (s *Store) LoginMCP(ctx context.Context, l MCPLogin) (Entry, error) {
	meta, err := s.discoverMCP(ctx, l.URL)
	if err != nil {
		return Entry{}, err
	}
	scopes := l.Scopes
	if len(scopes) == 0 {
		scopes = meta.scopes
	}
	if !slices.Contains(scopes, "offline_access") && slices.Contains(meta.asm.ScopesSupported, "offline_access") {
		scopes = append(slices.Clone(scopes), "offline_access") // ask for a refresh token
	}
	device := l.Device
	if l.Auto && meta.device != "" {
		device = true
	}
	if device && meta.device == "" {
		return Entry{}, fmt.Errorf("%w (%s)", ErrNoBrowserFlow, meta.asm.Issuer)
	}
	if !device && l.Auto {
		return Entry{}, fmt.Errorf("%w (%s)", ErrNoBrowserFlow, meta.asm.Issuer)
	}

	var ln net.Listener
	redirect := ""
	if !device {
		ln, err = net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(l.Port)))
		if err != nil {
			return Entry{}, fmt.Errorf("listen for the sign-in redirect: %w", err)
		}
		defer func() { _ = ln.Close() }()
		redirect = fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port)
	}
	cl, err := s.mcpClient(ctx, l, meta, redirect, device)
	if err != nil {
		return Entry{}, err
	}
	var tok tokenResponse
	if device {
		tok, err = s.mcpDevice(ctx, &l, meta, cl, scopes)
	} else {
		tok, err = s.mcpBrowser(ctx, &l, meta, cl, scopes, ln, redirect)
	}
	if err != nil {
		return Entry{}, err
	}
	e := Entry{Kind: MCP, TokenURL: meta.asm.TokenEndpoint, ClientID: cl.id, ClientSecret: cl.secret,
		TokenAuth: cl.auth, Resource: meta.resource, Scope: strings.Join(scopes, " "), Issuer: meta.asm.Issuer}
	e = s.applyToken(e, tok)
	if e.Scope == "" {
		e.Scope = tok.Scope
	}
	if err := s.Put(ctx, MCPLoginName(l.Server), e); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// discoverMCP asks the server for a token-less initialize and follows its
// 401 to the protected resource and authorization server metadata.
func (s *Store) discoverMCP(ctx context.Context, server string) (*mcpServerMeta, error) {
	u, err := url.Parse(server)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%q is not an http(s) URL", server)
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25",` +
		`"capabilities":{},"clientInfo":{"name":"habakkuk","version":"0.4"}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach %s: %w", server, err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	challenges, _ := oauthex.ParseWWWAuthenticate(resp.Header.Values("WWW-Authenticate"))
	var metaURL string
	var scopes []string
	for _, c := range challenges {
		if c.Scheme != "bearer" {
			continue
		}
		if v := c.Params["resource_metadata"]; v != "" && metaURL == "" {
			metaURL = v
		}
		if v := c.Params["scope"]; v != "" && scopes == nil {
			scopes = strings.Fields(v)
		}
	}
	asked := resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden

	meta := &mcpServerMeta{scopes: scopes}
	var prm *oauthex.ProtectedResourceMetadata
	for _, c := range resourceMetadataURLs(metaURL, server) {
		p, err := oauthex.GetProtectedResourceMetadata(ctx, c[0], c[1], s.client())
		if err == nil && p != nil {
			prm = p
			break
		}
	}
	switch {
	case prm != nil && len(prm.AuthorizationServers) == 0:
		return nil, errors.New("the server's protected resource metadata names no authorization server")
	case prm != nil:
		meta.resource = prm.Resource
		if meta.scopes == nil {
			meta.scopes = prm.ScopesSupported
		}
	case !asked:
		return nil, fmt.Errorf("%s answered without asking for a sign-in (HTTP %d) and publishes no OAuth metadata; it may not need auth: oauth", server, resp.StatusCode)
	default:
		// Before RFC 9728 metadata, the MCP server's origin was the
		// authorization server (MCP 2025-03-26).
		root := *u
		root.Path, root.RawQuery, root.Fragment = "", "", ""
		prm = &oauthex.ProtectedResourceMetadata{AuthorizationServers: []string{root.String()}}
		meta.resource = server
	}
	issuer := prm.AuthorizationServers[0]
	asm, err := sdkauth.GetAuthServerMetadata(ctx, issuer, s.client())
	if err != nil {
		return nil, fmt.Errorf("authorization server metadata: %w", err)
	}
	if asm == nil {
		base := strings.TrimRight(issuer, "/")
		asm = &oauthex.AuthServerMeta{Issuer: issuer, AuthorizationEndpoint: base + "/authorize",
			TokenEndpoint: base + "/token", RegistrationEndpoint: base + "/register"}
	} else {
		meta.device = s.deviceEndpoint(ctx, issuer)
	}
	meta.asm = asm
	return meta, nil
}

// resourceMetadataURLs lists where protected resource metadata may be, each
// with the resource it must name: the URL from the challenge, then the
// well-known location for the endpoint's path, then the origin's.
func resourceMetadataURLs(fromHeader, resource string) [][2]string {
	var out [][2]string
	if fromHeader != "" {
		out = append(out, [2]string{fromHeader, resource})
	}
	u, err := url.Parse(resource)
	if err != nil {
		return out
	}
	m := *u
	m.RawQuery, m.Fragment = "", ""
	if p := strings.Trim(u.Path, "/"); p != "" {
		m.Path = "/.well-known/oauth-protected-resource/" + p
		out = append(out, [2]string{m.String(), resource})
	}
	m.Path = "/.well-known/oauth-protected-resource"
	root := *u
	root.Path, root.RawQuery, root.Fragment = "", "", ""
	return append(out, [2]string{m.String(), root.String()})
}

// deviceEndpoint reads device_authorization_endpoint, which the SDK's
// metadata type leaves out, from the authorization server's metadata.
func (s *Store) deviceEndpoint(ctx context.Context, issuer string) string {
	u, err := url.Parse(issuer)
	if err != nil {
		return ""
	}
	p := strings.Trim(u.Path, "/")
	var candidates []string
	for _, wk := range []string{"oauth-authorization-server", "openid-configuration"} {
		c := *u
		c.Path = "/.well-known/" + wk
		if p != "" {
			c.Path += "/" + p
		}
		candidates = append(candidates, c.String())
	}
	if p != "" {
		c := *u
		c.Path = "/" + p + "/.well-known/openid-configuration"
		candidates = append(candidates, c.String())
	}
	for _, c := range candidates {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c, nil)
		if err != nil {
			continue
		}
		resp, err := s.client().Do(req)
		if err != nil {
			continue
		}
		var m struct {
			Issuer string `json:"issuer"`
			Device string `json:"device_authorization_endpoint"`
		}
		ok := resp.StatusCode == http.StatusOK && decodeJSON(resp, &m) == nil
		resp.Body.Close()
		if ok && strings.TrimRight(m.Issuer, "/") == strings.TrimRight(issuer, "/") {
			if d, err := url.Parse(m.Device); err == nil && (d.Scheme == "https" || isLoopbackHost(d.Hostname())) {
				return m.Device
			}
			return ""
		}
	}
	return ""
}

func isLoopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// oauthClient is the client Habakkuk signs in as.
type oauthClient struct {
	id, secret string
	auth       string // none | post | basic
}

func (s *Store) mcpClient(ctx context.Context, l MCPLogin, meta *mcpServerMeta, redirect string, device bool) (oauthClient, error) {
	if l.ClientID != "" {
		c := oauthClient{id: l.ClientID, secret: l.ClientSecret, auth: "none"}
		if c.secret != "" {
			c.auth = "basic" // every server must take it (RFC 6749 2.3.1)
			if slices.Contains(meta.asm.TokenEndpointAuthMethodsSupported, "client_secret_post") {
				c.auth = "post"
			}
		}
		return c, nil
	}
	if meta.asm.RegistrationEndpoint == "" {
		return oauthClient{}, fmt.Errorf("%s does not offer dynamic client registration: register an OAuth client with it "+
			"(redirect URI http://127.0.0.1:PORT/callback with callback_port PORT) and set client_id (and client_secret_env) on the server", meta.asm.Issuer)
	}
	grants := []string{"authorization_code", "refresh_token"}
	if device {
		grants = append(grants, deviceGrant)
		redirect = "http://127.0.0.1/callback" // required by most servers, unused by the device flow
	}
	reg, err := oauthex.RegisterClient(ctx, meta.asm.RegistrationEndpoint, &oauthex.ClientRegistrationMetadata{
		ClientName: "Habakkuk", ClientURI: "https://github.com/habakkuk-ai/habakkuk", RedirectURIs: []string{redirect},
		GrantTypes: grants, ResponseTypes: []string{"code"}, TokenEndpointAuthMethod: "none", ApplicationType: "native",
	}, s.client())
	if err != nil {
		return oauthClient{}, fmt.Errorf("register with %s: %w", meta.asm.Issuer, err)
	}
	c := oauthClient{id: reg.ClientID, secret: reg.ClientSecret, auth: "none"}
	switch {
	case reg.ClientSecret == "":
	case reg.TokenEndpointAuthMethod == "client_secret_post":
		c.auth = "post"
	default:
		c.auth = "basic"
	}
	return c, nil
}

const deviceGrant = "urn:ietf:params:oauth:grant-type:device_code"

// randomURLSafe returns n random bytes, base64url without padding.
func randomURLSafe(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// mcpBrowser is the authorization code flow with PKCE: the user signs in in
// a browser, which is redirected to Habakkuk's loopback listener.
func (s *Store) mcpBrowser(ctx context.Context, l *MCPLogin, meta *mcpServerMeta, cl oauthClient, scopes []string,
	ln net.Listener, redirect string) (tokenResponse, error) {
	verifier := randomURLSafe(32)
	sum := sha256.Sum256([]byte(verifier))
	state := randomURLSafe(16)
	q := url.Values{
		"response_type": {"code"}, "client_id": {cl.id}, "redirect_uri": {redirect},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
		"state": {state}, "resource": {meta.resource},
	}
	if len(scopes) > 0 {
		q.Set("scope", strings.Join(scopes, " "))
	}
	authURL := meta.asm.AuthorizationEndpoint
	if strings.Contains(authURL, "?") {
		authURL += "&" + q.Encode()
	} else {
		authURL += "?" + q.Encode()
	}

	type result struct {
		code string
		err  error
	}
	done := make(chan result, 1)
	finish := func(r result) {
		select {
		case done <- r:
		default:
		}
	}
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		v := r.URL.Query()
		var res result
		switch {
		case v.Get("state") != state:
			res.err = errors.New("the sign-in answered with the wrong state; start again")
		case v.Get("error") != "":
			res.err = fmt.Errorf("sign-in refused: %s %s", v.Get("error"), v.Get("error_description"))
		case v.Get("iss") != "" && strings.TrimRight(v.Get("iss"), "/") != strings.TrimRight(meta.asm.Issuer, "/"):
			res.err = fmt.Errorf("the sign-in came back from issuer %q, not %q", v.Get("iss"), meta.asm.Issuer)
		case meta.asm.AuthorizationResponseIssParameterSupported && v.Get("iss") == "":
			res.err = errors.New("the authorization server promised an iss parameter and sent none")
		case v.Get("code") == "":
			res.err = errors.New("the sign-in redirect carried no code")
		default:
			res.code = v.Get("code")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		msg := "Signed in. You can close this tab and return to Habakkuk."
		if res.err != nil {
			msg = "Sign-in failed: " + res.err.Error()
		}
		fmt.Fprintf(w, "<!doctype html><title>Habakkuk</title><p>%s</p>", html.EscapeString(msg))
		finish(res)
	})}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	l.say("Open this URL to sign in to %s:\n\n  %s\n", l.Server, authURL)
	if l.Open != nil {
		if err := l.Open(authURL); err == nil {
			l.say("(opened in your browser; waiting for the sign-in to finish)")
		}
	}
	var res result
	select {
	case res = <-done:
	case <-ctx.Done():
		return tokenResponse{}, ctx.Err()
	case <-time.After(10 * time.Minute):
		return tokenResponse{}, errors.New("timed out waiting for the browser sign-in")
	}
	if res.err != nil {
		return tokenResponse{}, res.err
	}
	return s.tokenRequest(ctx, meta.asm.TokenEndpoint, cl, url.Values{
		"grant_type": {"authorization_code"}, "code": {res.code}, "redirect_uri": {redirect},
		"code_verifier": {verifier}, "resource": {meta.resource},
	})
}

// mcpDevice is the device flow: the user enters a code on another device.
func (s *Store) mcpDevice(ctx context.Context, l *MCPLogin, meta *mcpServerMeta, cl oauthClient, scopes []string) (tokenResponse, error) {
	form := url.Values{"client_id": {cl.id}, "resource": {meta.resource}}
	if len(scopes) > 0 {
		form.Set("scope", strings.Join(scopes, " "))
	}
	resp, err := s.postClientForm(ctx, meta.device, form, oauthClient{id: cl.id, secret: cl.secret, auth: cl.auth})
	if err != nil {
		return tokenResponse{}, err
	}
	var dc struct {
		DeviceCode       string `json:"device_code"`
		UserCode         string `json:"user_code"`
		Verification     string `json:"verification_uri"`
		VerificationFull string `json:"verification_uri_complete"`
		ExpiresIn        int    `json:"expires_in"`
		Interval         int    `json:"interval"`
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return tokenResponse{}, fmt.Errorf("device authorization: %w", readHTTPError(resp))
	}
	err = decodeJSON(resp, &dc)
	resp.Body.Close()
	if err != nil || dc.DeviceCode == "" || dc.UserCode == "" || dc.Verification == "" {
		return tokenResponse{}, errors.New("device authorization: incomplete response")
	}
	if dc.VerificationFull != "" {
		l.say("Open %s on any device to sign in to %s (code %s).", dc.VerificationFull, l.Server, dc.UserCode)
	} else {
		l.say("Open %s on any device and enter the code %s to sign in to %s.", dc.Verification, dc.UserCode, l.Server)
	}
	interval := time.Duration(max(dc.Interval, 5)) * time.Second
	ttl := time.Duration(dc.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	deadline := s.now().Add(ttl)
	for s.now().Before(deadline) {
		if err := s.sleep(ctx, interval); err != nil {
			return tokenResponse{}, err
		}
		tok, err := s.tokenRequest(ctx, meta.asm.TokenEndpoint, cl, url.Values{
			"grant_type": {deviceGrant}, "device_code": {dc.DeviceCode}, "resource": {meta.resource},
		})
		var oe *oauthError
		switch {
		case err == nil:
			return tok, nil
		case errors.As(err, &oe) && oe.Code == "authorization_pending":
		case errors.As(err, &oe) && oe.Code == "slow_down":
			interval += 5 * time.Second
		default:
			return tokenResponse{}, err
		}
	}
	return tokenResponse{}, errors.New("the device code expired before the sign-in finished")
}

// tokenResponse is an OAuth token endpoint answer (RFC 6749 5.1).
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
}

// oauthError is an OAuth error response (RFC 6749 5.2).
type oauthError struct {
	Status      int
	Code        string `json:"error"`
	Description string `json:"error_description"`
}

func (e *oauthError) Error() string {
	if e.Description != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Description)
	}
	return fmt.Sprintf("%s (HTTP %d)", e.Code, e.Status)
}

func (s *Store) postClientForm(ctx context.Context, endpoint string, form url.Values, cl oauthClient) (*http.Response, error) {
	if cl.auth != "basic" {
		form.Set("client_id", cl.id)
	}
	if cl.auth == "post" {
		form.Set("client_secret", cl.secret)
	}
	for k, v := range form {
		if len(v) == 1 && v[0] == "" {
			delete(form, k)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBufferString(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if cl.auth == "basic" {
		req.SetBasicAuth(url.QueryEscape(cl.id), url.QueryEscape(cl.secret))
	}
	return s.client().Do(req)
}

// tokenRequest posts to the token endpoint and reads the token or the
// OAuth error.
func (s *Store) tokenRequest(ctx context.Context, endpoint string, cl oauthClient, form url.Values) (tokenResponse, error) {
	resp, err := s.postClientForm(ctx, endpoint, form, cl)
	if err != nil {
		return tokenResponse{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return tokenResponse{}, err
	}
	if resp.StatusCode != http.StatusOK {
		oe := &oauthError{Status: resp.StatusCode}
		if json.Unmarshal(data, oe) != nil || oe.Code == "" {
			return tokenResponse{}, &httpError{Status: resp.StatusCode, Body: string(data[:min(len(data), 4096)])}
		}
		return tokenResponse{}, oe
	}
	var tok tokenResponse
	if err := json.Unmarshal(data, &tok); err != nil {
		return tokenResponse{}, fmt.Errorf("token response: %w", err)
	}
	if tok.AccessToken == "" {
		return tokenResponse{}, errors.New("token response has no access token")
	}
	if tok.TokenType != "" && !strings.EqualFold(tok.TokenType, "bearer") {
		return tokenResponse{}, fmt.Errorf("token type %q is not supported (want Bearer)", tok.TokenType)
	}
	return tok, nil
}

// applyToken puts a token response into an entry. A refresh that returns no
// new refresh token keeps the old one.
func (s *Store) applyToken(e Entry, tok tokenResponse) Entry {
	e.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		e.RefreshToken = tok.RefreshToken
	}
	e.ExpiresAt = time.Time{}
	if tok.ExpiresIn > 0 {
		e.ExpiresAt = s.now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	}
	if tok.Scope != "" {
		e.Scope = tok.Scope
	}
	return e
}

func (s *Store) refreshMCP(ctx context.Context, e Entry) (Entry, error) {
	if e.RefreshToken == "" || e.TokenURL == "" {
		return e, fmt.Errorf("mcp: %w: the token expired and the server gave no refresh token", ErrLoginExpired)
	}
	tok, err := s.tokenRequest(ctx, e.TokenURL, oauthClient{id: e.ClientID, secret: e.ClientSecret, auth: e.TokenAuth},
		url.Values{"grant_type": {"refresh_token"}, "refresh_token": {e.RefreshToken}, "resource": {e.Resource}})
	var oe *oauthError
	if errors.As(err, &oe) && (oe.Code == "invalid_grant" || oe.Code == "invalid_client" || oe.Code == "unauthorized_client") {
		return e, fmt.Errorf("mcp: %w: %v", ErrLoginExpired, err)
	}
	if err != nil {
		return e, fmt.Errorf("mcp: refresh: %w", err)
	}
	return s.applyToken(e, tok), nil
}

// Invalidate marks a login's access token as used up when it is still the
// one given, so the next Get refreshes it. A server that refuses a token
// before its stated expiry calls for this.
func (s *Store) Invalidate(ctx context.Context, name, token string) error {
	return s.update(ctx, func(f *file) error {
		e, ok := f.Logins[name]
		if !ok || e.AccessToken != token {
			return nil
		}
		e.ExpiresAt = s.now().Add(-time.Minute)
		f.Logins[name] = e
		return nil
	})
}

// MCPToken returns a valid access token for an MCP server's login.
func (s *Store) MCPToken(ctx context.Context, server string) (string, error) {
	return s.Token(ctx, MCPLoginName(server))
}

// MCPInvalidate marks an MCP server's token as refused (see Invalidate).
func (s *Store) MCPInvalidate(ctx context.Context, server, token string) error {
	return s.Invalidate(ctx, MCPLoginName(server), token)
}
