package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"golang.org/x/oauth2"
)

// Tokens supplies OAuth access tokens for servers with auth: oauth. The app
// backs it with auth.json (habakkuk mcp login), whose refreshes hold the file
// lock, so several Habakkuk processes share one login.
type Tokens interface {
	// MCPToken returns a valid access token for the server, refreshing it
	// first when it is about to expire.
	MCPToken(ctx context.Context, server string) (string, error)
	// MCPInvalidate tells the store the server refused this token, so the
	// next MCPToken refreshes it.
	MCPInvalidate(ctx context.Context, server, token string) error
}

// errNoTokens is the error of an oauth server when the app supplied no
// token store (tests, or a manager built without one).
var errNoTokens = errors.New("no OAuth token store")

// oauthHandler is the SDK transport's OAuthHandler for one server. It never
// runs a browser flow itself: a server that refuses the token after a
// refresh needs `habakkuk mcp login`.
type oauthHandler struct {
	s *server

	mu        sync.Mutex
	last      string    // the token most recently handed out
	refreshed time.Time // when Authorize last invalidated a token
}

var _ sdkauth.OAuthHandler = (*oauthHandler)(nil)

func (h *oauthHandler) TokenSource(context.Context) (oauth2.TokenSource, error) {
	return h, nil
}

// Token implements oauth2.TokenSource. The store reads auth.json and
// refreshes under its lock, so this stays correct across processes.
func (h *oauthHandler) Token() (*oauth2.Token, error) {
	tokens := h.s.m.Tokens
	if tokens == nil {
		return nil, errNoTokens
	}
	ctx, cancel := context.WithTimeout(h.s.m.ctx, 30*time.Second)
	defer cancel()
	tok, err := tokens.MCPToken(ctx, h.s.name)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	h.last = tok
	h.mu.Unlock()
	return &oauth2.Token{AccessToken: tok, TokenType: "Bearer"}, nil
}

// Authorize runs when the server answers 401 or 403. The first refusal of a
// token marks it used up so the retry refreshes it; a second refusal soon
// after means the login itself is no good.
func (h *oauthHandler) Authorize(ctx context.Context, _ *http.Request, resp *http.Response) error {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	login := fmt.Sprintf("run `habakkuk mcp login %s`", h.s.name)
	if resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("the server refused access (HTTP 403); the login may lack a scope it needs: %s, or set scopes on the server", login)
	}
	tokens := h.s.m.Tokens
	if tokens == nil {
		return errNoTokens
	}
	h.mu.Lock()
	last, recent := h.last, time.Since(h.refreshed) < time.Minute
	if !recent {
		h.refreshed = time.Now()
	}
	h.mu.Unlock()
	if last == "" || recent {
		return fmt.Errorf("the server refused the OAuth token: %s", login)
	}
	if err := tokens.MCPInvalidate(ctx, h.s.name, last); err != nil {
		return err
	}
	return nil
}
