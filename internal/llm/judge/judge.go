// Package judge asks a decision model (TypeSafe's Jev) typed questions about
// a piece of state: yes/no with a probability, or one choice of several. It
// answers in well under a second for a fraction of a cent, so Habakkuk uses
// it for its own small decisions (which model a turn needs, whether a tool
// call is plainly routine, whether a chat holds anything worth remembering)
// instead of a full model call.
//
// Every caller treats an error as "no answer" and falls back to what it did
// before, so a slow or missing judge never blocks a turn.
package judge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// Defaults.
const (
	DefaultBaseURL = "https://api.typesafe.ai/v1"
	DefaultModel   = "jev-latest"
	DefaultKeyEnv  = "TYPESAFE_API_KEY"
	DefaultTimeout = 800 * time.Millisecond
	// Endpoint is the provider name usage is recorded under.
	Endpoint = "typesafe"
	// maxState keeps a request well inside the 32k-token per-question limit.
	maxState = 60_000
)

// Question is one typed question. Type is "noul" (yes/no, answered with the
// probability of yes) or "choice" (Criteria maps each option to a
// description).
type Question struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions,omitempty"`
	Criteria     map[string]string `json:"criteria,omitempty"`
}

// Yes is a yes/no question.
func Yes(instructions string) Question { return Question{Type: "noul", Instructions: instructions} }

// Answer is one typed answer. P is the probability of yes for a yes/no
// question; Choice and Confidence answer a choice question.
type Answer struct {
	Type       string  `json:"type"`
	P          float64 `json:"noul"`
	Choice     string  `json:"choice"`
	Confidence float64 `json:"confidence"`
}

// Client calls the decision API.
type Client struct {
	BaseURL string
	Model   string
	Key     func() string // read at call time, so a changed .env applies
	Timeout time.Duration
	HTTP    *http.Client
	// OnUsage records each call's tokens (the router's usage hook).
	OnUsage func(ctx context.Context, t llm.Target, u llm.Usage)
}

// ErrNoKey means no API key is set; callers fall back quietly.
var ErrNoKey = errors.New("judge: no API key")

// Ask sends state and questions and returns the answers by question name.
func (c *Client) Ask(ctx context.Context, state any, questions map[string]Question) (map[string]Answer, error) {
	if c == nil {
		return nil, errors.New("judge: not configured")
	}
	key := ""
	if c.Key != nil {
		key = strings.TrimSpace(c.Key())
	}
	if key == "" {
		return nil, ErrNoKey
	}
	if s, ok := state.(string); ok && len(s) > maxState {
		state = s[:maxState] + "\n[truncated]"
	}
	body, err := json.Marshal(map[string]any{"model": c.model(), "state": state, "questions": questions})
	if err != nil {
		return nil, err
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/systemone", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("judge: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("judge: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("judge: HTTP %d: %s", resp.StatusCode, clip(string(data), 300))
	}
	var out struct {
		Model   string            `json:"model"`
		Answers map[string]Answer `json:"answers"`
		Usage   struct {
			Input  int64 `json:"input_tokens"`
			Output int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("judge: bad response: %w", err)
	}
	for name := range questions {
		if _, ok := out.Answers[name]; !ok {
			return nil, fmt.Errorf("judge: no answer to %q", name)
		}
	}
	if c.OnUsage != nil {
		model := out.Model
		if model == "" {
			model = c.model()
		}
		c.OnUsage(ctx, llm.Target{Endpoint: Endpoint, Model: model}, llm.Usage{InputTokens: out.Usage.Input, OutputTokens: out.Usage.Output})
	}
	return out.Answers, nil
}

func (c *Client) model() string {
	if c.Model == "" {
		return DefaultModel
	}
	return c.Model
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
