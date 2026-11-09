package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
)

// JSON-RPC 2.0 over newline-delimited JSON, in both directions: the client
// calls the agent (session/prompt) and the agent calls the client
// (session/request_permission, fs/read_text_file) on the same stream.

const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
)

// rpcError is a JSON-RPC error object; handlers return it to choose the code.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("%s (code %d)", e.Message, e.Code) }

func invalidParams(format string, args ...any) *rpcError {
	return &rpcError{Code: codeInvalidParams, Message: fmt.Sprintf(format, args...)}
}

// incoming is any message read from the peer.
type incoming struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"` // nil for a notification
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result"`
}

type errorResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   *rpcError       `json:"error"`
}

// handler answers a request (result or error) or a notification (ignored).
type handler func(ctx context.Context, method string, params json.RawMessage) (any, error)

// conn is one JSON-RPC peer.
type conn struct {
	in  io.Reader
	out io.Writer
	wmu sync.Mutex

	mu      sync.Mutex
	next    int64
	pending map[string]chan incoming
	closed  bool
}

func newConn(in io.Reader, out io.Writer) *conn {
	return &conn{in: in, out: out, pending: map[string]chan incoming{}}
}

func (c *conn) write(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.out.Write(append(data, '\n'))
	return err
}

// notify sends a notification.
func (c *conn) notify(method string, params any) error {
	return c.write(request{JSONRPC: "2.0", Method: method, Params: params})
}

var errClosed = errors.New("the client disconnected")

// call sends a request and waits for its response, decoding the result
// into out (when not nil).
func (c *conn) call(ctx context.Context, method string, params, out any) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errClosed
	}
	c.next++
	id := c.next
	key := strconv.FormatInt(id, 10)
	ch := make(chan incoming, 1)
	c.pending[key] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
	}()
	if err := c.write(request{JSONRPC: "2.0", ID: &id, Method: method, Params: params}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case msg, ok := <-ch:
		if !ok {
			return errClosed
		}
		if msg.Error != nil {
			return msg.Error
		}
		if out != nil {
			return json.Unmarshal(msg.Result, out)
		}
		return nil
	}
}

// serve reads messages until the stream ends, running each request in its
// own goroutine (a prompt runs for minutes while cancels and responses keep
// arriving). It returns once every handler has finished.
func (c *conn) serve(ctx context.Context, h handler) error {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		// The client is gone: fail our pending calls, stop the handlers.
		c.close()
		cancel()
		wg.Wait()
	}()
	dec := json.NewDecoder(c.in)
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			var syn *json.SyntaxError
			if errors.As(err, &syn) {
				_ = c.write(errorResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: codeParse, Message: err.Error()}})
			}
			return err
		}
		var msg incoming
		if err := json.Unmarshal(raw, &msg); err != nil {
			_ = c.write(errorResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: codeInvalidRequest, Message: err.Error()}})
			continue
		}
		hasID := len(msg.ID) > 0 && string(msg.ID) != "null"
		switch {
		case msg.Method == "" && hasID: // a response to one of our calls
			c.mu.Lock()
			ch := c.pending[string(msg.ID)]
			delete(c.pending, string(msg.ID))
			c.mu.Unlock()
			if ch != nil {
				ch <- msg
			}
		case msg.Method != "" && hasID:
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := h(ctx, msg.Method, msg.Params)
				if err != nil {
					var re *rpcError
					if !errors.As(err, &re) {
						re = &rpcError{Code: codeInternal, Message: err.Error()}
					}
					_ = c.write(errorResponse{JSONRPC: "2.0", ID: msg.ID, Error: re})
					return
				}
				_ = c.write(response{JSONRPC: "2.0", ID: msg.ID, Result: res})
			}()
		case msg.Method != "":
			// Notifications (session/cancel) run inline: they are quick, and
			// order matters.
			_, _ = h(ctx, msg.Method, msg.Params)
		}
	}
}

// close fails every call still waiting for an answer.
func (c *conn) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for k, ch := range c.pending {
		close(ch)
		delete(c.pending, k)
	}
}
