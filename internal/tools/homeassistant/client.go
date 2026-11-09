// Package homeassistant connects Habakkuk to Home Assistant: the
// homeassistant toolset over the REST API (states, service calls, history)
// with a long-lived access token, and event triggers over the WebSocket API
// that start a turn when something happens at home.
package homeassistant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client calls Home Assistant's REST API.
type Client struct {
	URL   string // base URL, e.g. http://homeassistant.local:8123
	Token string
	HTTP  *http.Client
}

// State is one entity's state.
type State struct {
	EntityID    string         `json:"entity_id"`
	State       string         `json:"state"`
	Attributes  map[string]any `json:"attributes"`
	LastChanged time.Time      `json:"last_changed"`
	LastUpdated time.Time      `json:"last_updated"`
}

// Name is the entity's friendly name, or its id.
func (s State) Name() string {
	if n, ok := s.Attributes["friendly_name"].(string); ok && n != "" {
		return n
	}
	return s.EntityID
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	if c.Token == "" {
		return fmt.Errorf("the Home Assistant token is not set")
	}
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.URL, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("home assistant: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("home assistant refused the token (HTTP 401); create a long-lived access token on your profile page")
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("home assistant: not found (HTTP 404): %s", snippet(data))
	case resp.StatusCode >= 300:
		return fmt.Errorf("home assistant: HTTP %d: %s", resp.StatusCode, snippet(data))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("home assistant: unexpected answer: %w", err)
	}
	return nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// States lists every entity's state.
func (c *Client) States(ctx context.Context) ([]State, error) {
	var out []State
	err := c.do(ctx, http.MethodGet, "/api/states", nil, &out)
	return out, err
}

// CallService calls domain.service with data (which carries the target)
// and returns the states that changed.
func (c *Client) CallService(ctx context.Context, domain, service string, data map[string]any) ([]State, error) {
	var out []State
	err := c.do(ctx, http.MethodPost, "/api/services/"+url.PathEscape(domain)+"/"+url.PathEscape(service), data, &out)
	return out, err
}

// HistoryPoint is one state an entity had.
type HistoryPoint struct {
	State       string    `json:"state"`
	LastChanged time.Time `json:"last_changed"`
}

// History returns an entity's states between start and end, oldest first.
func (c *Client) History(ctx context.Context, entity string, start, end time.Time) ([]HistoryPoint, error) {
	q := url.Values{"filter_entity_id": {entity}, "end_time": {end.UTC().Format(time.RFC3339)}}
	path := "/api/history/period/" + url.PathEscape(start.UTC().Format(time.RFC3339)) + "?" + q.Encode() +
		"&minimal_response&no_attributes"
	var out [][]HistoryPoint
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out[0], nil
}
