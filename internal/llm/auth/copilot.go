package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// GitHub Copilot sign-in is GitHub's OAuth device flow. The GitHub token it
// yields is long-lived and kept as the refresh credential; each Copilot API
// token (about 30 minutes) is exchanged from it on demand.
const (
	copilotClientID = "Iv1.b507a08c87ecfe98"
	githubURL       = "https://github.com"
	githubAPIURL    = "https://api.github.com"
	// CopilotDefaultBaseURL is used until a token exchange names the
	// account's own endpoint.
	CopilotDefaultBaseURL = "https://api.githubcopilot.com"
)

// The Copilot API only serves clients that identify as a supported editor
// integration. These are the values documented by open-source clients.
var copilotEditorHeaders = map[string]string{
	"Editor-Version":         "vscode/1.107.0",
	"Editor-Plugin-Version":  "copilot-chat/0.35.0",
	"Copilot-Integration-Id": "vscode-chat",
}

func (s *Store) github() string {
	if s.GitHubURL != "" {
		return strings.TrimRight(s.GitHubURL, "/")
	}
	return githubURL
}

func (s *Store) githubAPI() string {
	if s.GitHubAPIURL != "" {
		return strings.TrimRight(s.GitHubAPIURL, "/")
	}
	return githubAPIURL
}

func (s *Store) postForm(ctx context.Context, u string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	return s.client().Do(req)
}

func (s *Store) loginCopilot(ctx context.Context, show func(DeviceCode)) (Entry, error) {
	resp, err := s.postForm(ctx, s.github()+"/login/device/code", url.Values{"client_id": {copilotClientID}, "scope": {"read:user"}})
	if err != nil {
		return Entry{}, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return Entry{}, fmt.Errorf("copilot: request device code: %w", readHTTPError(resp))
	}
	var code struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
	}
	err = decodeJSON(resp, &code)
	resp.Body.Close()
	if err != nil {
		return Entry{}, fmt.Errorf("copilot: device code response: %w", err)
	}
	if code.DeviceCode == "" || code.UserCode == "" {
		return Entry{}, errors.New("copilot: device code response is missing its code")
	}
	interval := time.Duration(max(code.Interval, 5)) * time.Second
	ttl := time.Duration(max(code.ExpiresIn, 60)) * time.Second
	show(DeviceCode{URL: code.VerificationURI, Code: code.UserCode, ExpiresIn: ttl})

	deadline := s.now().Add(ttl)
	var ghToken string
	for ghToken == "" {
		if s.now().After(deadline) {
			return Entry{}, errors.New("copilot: the code expired before sign-in finished; run the login again")
		}
		if err := s.sleep(ctx, interval); err != nil {
			return Entry{}, err
		}
		resp, err := s.postForm(ctx, s.github()+"/login/oauth/access_token", url.Values{
			"client_id": {copilotClientID}, "device_code": {code.DeviceCode},
			"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"},
		})
		if err != nil {
			return Entry{}, err
		}
		var poll struct {
			AccessToken string `json:"access_token"`
			Error       string `json:"error"`
			Description string `json:"error_description"`
			Interval    int    `json:"interval"`
		}
		err = decodeJSON(resp, &poll)
		resp.Body.Close()
		if err != nil {
			return Entry{}, fmt.Errorf("copilot: poll sign-in: %w", err)
		}
		switch poll.Error {
		case "":
			ghToken = poll.AccessToken
			if ghToken == "" {
				return Entry{}, errors.New("copilot: sign-in response has no token")
			}
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
			if poll.Interval > 0 {
				interval = time.Duration(poll.Interval) * time.Second
			}
		case "expired_token":
			return Entry{}, errors.New("copilot: the code expired before sign-in finished; run the login again")
		case "access_denied":
			return Entry{}, errors.New("copilot: sign-in was cancelled")
		default:
			return Entry{}, fmt.Errorf("copilot: sign-in failed: %s %s", poll.Error, poll.Description)
		}
	}
	return s.refreshCopilot(ctx, Entry{Kind: Copilot, RefreshToken: ghToken})
}

// refreshCopilot exchanges the GitHub token for a Copilot API token.
func (s *Store) refreshCopilot(ctx context.Context, e Entry) (Entry, error) {
	if e.RefreshToken == "" {
		return e, ErrLoginExpired
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.githubAPI()+"/copilot_internal/v2/token", nil)
	if err != nil {
		return e, err
	}
	req.Header.Set("Authorization", "token "+e.RefreshToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2025-04-01")
	for k, v := range copilotEditorHeaders {
		req.Header.Set(k, v)
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return e, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return e, fmt.Errorf("copilot: %w: %v", ErrLoginExpired, readHTTPError(resp))
	case http.StatusForbidden, http.StatusNotFound:
		return e, fmt.Errorf("copilot: this GitHub account has no Copilot access: %w", readHTTPError(resp))
	default:
		return e, fmt.Errorf("copilot: token exchange: %w", readHTTPError(resp))
	}
	var tok struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
		RefreshIn int64  `json:"refresh_in"`
		Endpoints struct {
			API string `json:"api"`
		} `json:"endpoints"`
	}
	if err := decodeJSON(resp, &tok); err != nil {
		return e, fmt.Errorf("copilot: token response: %w", err)
	}
	if tok.Token == "" {
		return e, errors.New("copilot: token response has no token")
	}
	e.AccessToken = tok.Token
	switch {
	case tok.RefreshIn > 0:
		// Refresh when GitHub suggests: the margin is added back by fresh().
		e.ExpiresAt = s.now().Add(time.Duration(tok.RefreshIn)*time.Second + refreshMargin)
	case tok.ExpiresAt > 0:
		e.ExpiresAt = time.Unix(tok.ExpiresAt, 0)
	default:
		e.ExpiresAt = s.now().Add(25 * time.Minute)
	}
	if tok.ExpiresAt > 0 && time.Unix(tok.ExpiresAt, 0).Before(e.ExpiresAt) {
		e.ExpiresAt = time.Unix(tok.ExpiresAt, 0)
	}
	if tok.Endpoints.API != "" {
		e.BaseURL = strings.TrimRight(tok.Endpoints.API, "/")
	}
	return e, nil
}

// CopilotHeaders are the headers each Copilot API request carries: the
// editor identity, whether the user or the agent started the request, and
// the flag Copilot needs before it accepts images.
func CopilotHeaders(req llm.Request) map[string]string {
	h := map[string]string{"Openai-Intent": "conversation-edits", "X-Initiator": "user"}
	for k, v := range copilotEditorHeaders {
		h[k] = v
	}
	if n := len(req.Messages); n > 0 {
		last := req.Messages[n-1]
		if last.Role != llm.User || len(last.Blocks) > 0 && last.Blocks[0].Type == llm.ToolResult {
			h["X-Initiator"] = "agent"
		}
	}
	for _, m := range req.Messages {
		for _, b := range m.Blocks {
			if b.Type == llm.Image {
				h["Copilot-Vision-Request"] = "true"
			}
		}
	}
	return h
}
