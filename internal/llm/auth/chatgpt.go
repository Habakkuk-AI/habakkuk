package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ChatGPT sign-in uses OpenAI's public OAuth client for command-line
// sign-in and its device-code flow: the user opens a page, enters a short
// code, and the client polls until the sign-in completes. The access token
// is a JWT; it is refreshed with the refresh token shortly before it
// expires.
const (
	chatgptClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	chatgptIssuer      = "https://auth.openai.com"
	chatgptDevicePage  = "/codex/device"
	chatgptRedirectURI = "/deviceauth/callback"
	chatgptCodeTTL     = 15 * time.Minute
)

func (s *Store) issuer() string {
	if s.ChatGPTIssuer != "" {
		return strings.TrimRight(s.ChatGPTIssuer, "/")
	}
	return chatgptIssuer
}

func (s *Store) postJSON(ctx context.Context, u string, body any) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return s.client().Do(req)
}

func (s *Store) loginChatGPT(ctx context.Context, show func(DeviceCode)) (Entry, error) {
	resp, err := s.postJSON(ctx, s.issuer()+"/api/accounts/deviceauth/usercode", map[string]string{"client_id": chatgptClientID})
	if err != nil {
		return Entry{}, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return Entry{}, errors.New("chatgpt: device sign-in is not available for this account or server")
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return Entry{}, fmt.Errorf("chatgpt: request device code: %w", readHTTPError(resp))
	}
	var code struct {
		DeviceAuthID string          `json:"device_auth_id"`
		UserCode     string          `json:"user_code"`
		UserCodeAlt  string          `json:"usercode"`
		Interval     json.RawMessage `json:"interval"` // a number, or a number in a string
	}
	err = decodeJSON(resp, &code)
	resp.Body.Close()
	if err != nil {
		return Entry{}, fmt.Errorf("chatgpt: device code response: %w", err)
	}
	if code.UserCode == "" {
		code.UserCode = code.UserCodeAlt
	}
	if code.DeviceAuthID == "" || code.UserCode == "" {
		return Entry{}, errors.New("chatgpt: device code response is missing its code")
	}
	interval := parseInterval(code.Interval, 5*time.Second)
	show(DeviceCode{URL: s.issuer() + chatgptDevicePage, Code: code.UserCode, ExpiresIn: chatgptCodeTTL})

	deadline := s.now().Add(chatgptCodeTTL)
	var grant struct {
		AuthorizationCode string `json:"authorization_code"`
		CodeVerifier      string `json:"code_verifier"`
	}
	for {
		if s.now().After(deadline) {
			return Entry{}, errors.New("chatgpt: the code expired before sign-in finished; run the login again")
		}
		if err := s.sleep(ctx, interval); err != nil {
			return Entry{}, err
		}
		resp, err := s.postJSON(ctx, s.issuer()+"/api/accounts/deviceauth/token",
			map[string]string{"device_auth_id": code.DeviceAuthID, "user_code": code.UserCode})
		if err != nil {
			return Entry{}, err
		}
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
			resp.Body.Close() // still waiting for the user
			continue
		}
		if resp.StatusCode != http.StatusOK {
			defer resp.Body.Close()
			return Entry{}, fmt.Errorf("chatgpt: poll sign-in: %w", readHTTPError(resp))
		}
		err = decodeJSON(resp, &grant)
		resp.Body.Close()
		if err != nil {
			return Entry{}, fmt.Errorf("chatgpt: sign-in response: %w", err)
		}
		break
	}

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {chatgptClientID},
		"code":          {grant.AuthorizationCode},
		"redirect_uri":  {s.issuer() + chatgptRedirectURI},
		"code_verifier": {grant.CodeVerifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.issuer()+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return Entry{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err = s.client().Do(req)
	if err != nil {
		return Entry{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Entry{}, fmt.Errorf("chatgpt: token exchange: %w", readHTTPError(resp))
	}
	var tok chatgptTokens
	if err := decodeJSON(resp, &tok); err != nil {
		return Entry{}, fmt.Errorf("chatgpt: token response: %w", err)
	}
	if tok.AccessToken == "" || tok.RefreshToken == "" {
		return Entry{}, errors.New("chatgpt: token response is missing tokens")
	}
	return s.chatgptEntry(Entry{Kind: ChatGPT}, tok), nil
}

type chatgptTokens struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

// chatgptEntry merges a token response into a login: fields the response
// leaves out keep their old values.
func (s *Store) chatgptEntry(e Entry, tok chatgptTokens) Entry {
	if tok.IDToken != "" {
		e.IDToken = tok.IDToken
	}
	if tok.AccessToken != "" {
		e.AccessToken = tok.AccessToken
	}
	if tok.RefreshToken != "" {
		e.RefreshToken = tok.RefreshToken
	}
	switch exp := jwtExpiry(e.AccessToken); {
	case !exp.IsZero():
		e.ExpiresAt = exp
	case tok.ExpiresIn > 0:
		e.ExpiresAt = s.now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	default:
		e.ExpiresAt = s.now().Add(time.Hour)
	}
	for _, t := range []string{e.IDToken, e.AccessToken} {
		claims := jwtClaims(t)
		if a, ok := claims["https://api.openai.com/auth"].(map[string]any); ok && e.AccountID == "" {
			e.AccountID, _ = a["chatgpt_account_id"].(string)
		}
		if email, ok := claims["email"].(string); ok && e.Email == "" {
			e.Email = email
		}
	}
	return e
}

func (s *Store) refreshChatGPT(ctx context.Context, e Entry) (Entry, error) {
	if e.RefreshToken == "" {
		return e, ErrLoginExpired
	}
	resp, err := s.postJSON(ctx, s.issuer()+"/oauth/token", map[string]string{
		"grant_type": "refresh_token", "client_id": chatgptClientID, "refresh_token": e.RefreshToken,
	})
	if err != nil {
		return e, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized {
		return e, fmt.Errorf("chatgpt: %w: %v", ErrLoginExpired, readHTTPError(resp))
	}
	if resp.StatusCode != http.StatusOK {
		return e, fmt.Errorf("chatgpt: refresh: %w", readHTTPError(resp))
	}
	var tok chatgptTokens
	if err := decodeJSON(resp, &tok); err != nil {
		return e, fmt.Errorf("chatgpt: refresh response: %w", err)
	}
	if tok.AccessToken == "" {
		return e, errors.New("chatgpt: refresh response has no access token")
	}
	return s.chatgptEntry(e, tok), nil
}

// jwtClaims decodes a JWT's payload without checking its signature: the
// token came straight from the issuer over TLS and is only read for its
// expiry and account id.
func jwtClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(data, &claims) != nil {
		return nil
	}
	return claims
}

func jwtExpiry(token string) time.Time {
	if exp, ok := jwtClaims(token)["exp"].(float64); ok && exp > 0 {
		return time.Unix(int64(exp), 0)
	}
	return time.Time{}
}

func parseInterval(raw json.RawMessage, def time.Duration) time.Duration {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return def
}
