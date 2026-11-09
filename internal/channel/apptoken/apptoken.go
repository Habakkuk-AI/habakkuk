// Package apptoken caches the short-lived access tokens that app platforms
// (Feishu, DingTalk, WeCom) hand out for an app id and secret: one fetch
// serves every caller until shortly before the token expires.
package apptoken

import (
	"context"
	"sync"
	"time"
)

// margin is how long before its stated expiry a token is replaced, so a
// request never leaves with a token that dies in flight.
const margin = 5 * time.Minute

// Cache holds one token. Fetch gets a new one and says how long it lives.
type Cache struct {
	Fetch func(ctx context.Context) (token string, ttl time.Duration, err error)
	Now   func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

func (c *Cache) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Get returns a live token, fetching one when none is cached or the cached
// one is about to expire. Concurrent callers wait for one fetch.
func (c *Cache) Get(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.now().Before(c.expires) {
		return c.token, nil
	}
	tok, ttl, err := c.Fetch(ctx)
	if err != nil {
		return "", err
	}
	if ttl > 2*margin {
		ttl -= margin
	} else {
		ttl /= 2
	}
	c.token, c.expires = tok, c.now().Add(ttl)
	return tok, nil
}

// Reset drops the cached token if it is still tok, after the platform
// rejected it, so the next Get fetches a fresh one.
func (c *Cache) Reset(tok string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == tok {
		c.token = ""
	}
}
