package a2a

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
)

// Client calls other agents over A2A JSON-RPC.
type Client struct {
	HTTP   *http.Client
	Header http.Header // sent with every request (a peer's auth headers)
	id     atomic.Int64
}

// CardURL turns a base URL into its card's URL; a URL that already names a
// .json file is used as is.
func CardURL(u string) string {
	u = strings.TrimRight(u, "/")
	if strings.HasSuffix(u, ".json") {
		return u
	}
	return u + CardPath
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	for k, v := range c.Header {
		req.Header[k] = v
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	return hc.Do(req)
}

// Card fetches and reads an agent's card.
func (c *Client) Card(ctx context.Context, url string) (*CardInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, CardURL(url), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("A2A-Version", "1.0")
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("agent card at %s: HTTP %d: %s", CardURL(url), resp.StatusCode, snippet(data))
	}
	return DecodeCard(data)
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// NewMessageID returns a random message id.
func NewMessageID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var methodNames = map[Version]map[string]string{
	V03: {"send": "message/send", "get": "tasks/get", "cancel": "tasks/cancel", "stream": "message/stream"},
	V10: {"send": "SendMessage", "get": "GetTask", "cancel": "CancelTask", "stream": "SendStreamingMessage"},
}

func (c *Client) request(ctx context.Context, card *CardInfo, op string, params any, accept string) (*http.Response, error) {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.id.Add(1), "method": methodNames[card.Protocol][op], "params": params})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, card.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)
	req.Header.Set("A2A-Version", card.Protocol.String())
	return c.do(req)
}

// call makes one JSON-RPC call and returns its result.
func (c *Client) call(ctx context.Context, card *CardInfo, op string, params any) (json.RawMessage, error) {
	resp, err := c.request(ctx, card, op, params, "application/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	return rpcResponse(resp.StatusCode, data)
}

func rpcResponse(status int, data []byte) (json.RawMessage, error) {
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  *RPCError       `json:"error"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("HTTP %d, not a JSON-RPC response: %s", status, snippet(data))
	}
	if out.Error != nil {
		return nil, out.Error
	}
	if len(out.Result) == 0 {
		return nil, fmt.Errorf("HTTP %d: JSON-RPC response without result", status)
	}
	return out.Result, nil
}

// Send sends a message and returns the agent's answer: a task or a
// message. With wait false it asks the agent to return at once.
func (c *Client) Send(ctx context.Context, card *CardInfo, msg *Message, wait bool) (Event, error) {
	if msg.ID == "" {
		msg.ID = NewMessageID()
	}
	params := map[string]any{"message": EncodeMessage(card.Protocol, msg)}
	if card.Protocol == V10 {
		params["configuration"] = map[string]any{"returnImmediately": !wait, "acceptedOutputModes": []string{"text/plain", "application/json"}}
	} else {
		params["configuration"] = map[string]any{"blocking": wait, "acceptedOutputModes": []string{"text/plain", "application/json"}}
	}
	res, err := c.call(ctx, card, "send", params)
	if err != nil {
		return Event{}, err
	}
	ev, err := DecodeEvent(res)
	if err == nil && ev.Task == nil && ev.Message == nil {
		err = errors.New("the agent answered with neither a task nor a message")
	}
	return ev, err
}

// GetTask reads a task's current state.
func (c *Client) GetTask(ctx context.Context, card *CardInfo, id string) (*Task, error) {
	res, err := c.call(ctx, card, "get", map[string]any{"id": id})
	if err != nil {
		return nil, err
	}
	return DecodeTask(res)
}

// CancelTask asks the agent to stop a task.
func (c *Client) CancelTask(ctx context.Context, card *CardInfo, id string) (*Task, error) {
	res, err := c.call(ctx, card, "cancel", map[string]any{"id": id})
	if err != nil {
		return nil, err
	}
	return DecodeTask(res)
}

// Stream sends a message with message/stream and calls on for each event
// until the stream ends.
func (c *Client) Stream(ctx context.Context, card *CardInfo, msg *Message, on func(Event)) error {
	if msg.ID == "" {
		msg.ID = NewMessageID()
	}
	resp, err := c.request(ctx, card, "stream", map[string]any{"message": EncodeMessage(card.Protocol, msg)}, "text/event-stream")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_, err := rpcResponse(resp.StatusCode, data)
		if err == nil {
			err = fmt.Errorf("HTTP %d: expected an event stream", resp.StatusCode)
		}
		return err
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	var data strings.Builder
	for sc.Scan() {
		line := sc.Text()
		if d, ok := strings.CutPrefix(line, "data:"); ok {
			data.WriteString(strings.TrimPrefix(d, " "))
			continue
		}
		if line != "" || data.Len() == 0 {
			continue
		}
		res, err := rpcResponse(resp.StatusCode, []byte(data.String()))
		data.Reset()
		if err != nil {
			return err
		}
		ev, err := DecodeEvent(res)
		if err != nil {
			return err
		}
		on(ev)
	}
	return sc.Err()
}
