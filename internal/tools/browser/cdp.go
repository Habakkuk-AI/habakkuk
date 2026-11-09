package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"
)

// A small Chrome DevTools Protocol client. One connection carries every
// target through flattened sessions (Target.attachToTarget with flatten), so
// a message names its target by sessionId.

// transport moves whole CDP messages: a pipe to a browser Habakkuk started
// (--remote-debugging-pipe, no open port) or a WebSocket to one it connects to.
type transport interface {
	send(msg []byte) error
	recv() ([]byte, error)
	close() error
}

type wsTransport struct{ c *websocket.Conn }

// maxMessage bounds one CDP message; full-page screenshots and PDFs arrive
// base64-encoded in a single reply.
const maxMessage = 256 << 20

func dialWS(ctx context.Context, url string) (*wsTransport, error) {
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(maxMessage)
	return &wsTransport{c: c}, nil
}

func (w *wsTransport) send(msg []byte) error {
	return w.c.Write(context.Background(), websocket.MessageText, msg)
}

func (w *wsTransport) recv() ([]byte, error) {
	_, b, err := w.c.Read(context.Background())
	return b, err
}

func (w *wsTransport) close() error { return w.c.CloseNow() }

type message struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *cdpError       `json:"error,omitempty"`
}

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data,omitempty"`
}

func (e *cdpError) Error() string {
	if e.Data != "" {
		return e.Message + ": " + e.Data
	}
	return e.Message
}

// errClosed means the connection to the browser is gone (it exited, crashed
// or was closed); the next tool call starts a new one.
var errClosed = errors.New("the browser connection closed")

type conn struct {
	t      transport
	nextID atomic.Int64

	mu      sync.Mutex
	pending map[int64]chan message
	subs    map[int64]func(message)
	nextSub int64

	done chan struct{}
	err  error // why done closed; set before close(done)
	once sync.Once
}

func newConn(t transport) *conn {
	c := &conn{t: t, pending: map[int64]chan message{}, subs: map[int64]func(message){}, done: make(chan struct{})}
	go c.readLoop()
	return c
}

func (c *conn) readLoop() {
	for {
		data, err := c.t.recv()
		if err != nil {
			c.fail(err)
			return
		}
		var m message
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		if m.ID != 0 {
			c.mu.Lock()
			ch := c.pending[m.ID]
			delete(c.pending, m.ID)
			c.mu.Unlock()
			if ch != nil {
				ch <- m
			}
			continue
		}
		c.mu.Lock()
		subs := make([]func(message), 0, len(c.subs))
		for _, fn := range c.subs {
			subs = append(subs, fn)
		}
		c.mu.Unlock()
		for _, fn := range subs {
			fn(m)
		}
	}
}

func (c *conn) fail(err error) {
	c.once.Do(func() {
		c.err = err
		close(c.done)
		_ = c.t.close()
	})
}

// close ends the connection; calls in flight return errClosed.
func (c *conn) close() { c.fail(errClosed) }

func (c *conn) closed() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// on registers fn for every event. It runs on the read loop, so it must not
// block or make calls itself; spawn a goroutine for that.
func (c *conn) on(fn func(message)) (cancel func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextSub++
	id := c.nextSub
	c.subs[id] = fn
	return func() {
		c.mu.Lock()
		delete(c.subs, id)
		c.mu.Unlock()
	}
}

// call sends one command to the target of session ("" is the browser) and
// decodes its result into out, if out is not nil.
func (c *conn) call(ctx context.Context, session, method string, params, out any) error {
	id := c.nextID.Add(1)
	m := message{ID: id, Method: method, SessionID: session}
	if params != nil {
		p, err := json.Marshal(params)
		if err != nil {
			return err
		}
		m.Params = p
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	ch := make(chan message, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	forget := func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}
	if c.closed() {
		forget()
		return errClosed
	}
	if err := c.t.send(data); err != nil {
		forget()
		c.fail(err)
		return errClosed
	}
	select {
	case <-ctx.Done():
		forget()
		return fmt.Errorf("%s: %w", method, ctx.Err())
	case <-c.done:
		forget()
		return errClosed
	case resp := <-ch:
		if resp.Error != nil {
			return fmt.Errorf("%s: %w", method, resp.Error)
		}
		if out != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, out); err != nil {
				return fmt.Errorf("%s: decoding result: %w", method, err)
			}
		}
		return nil
	}
}

// waiter collects events that match pred from the moment it is created, so
// an action can start listening before it triggers what it waits for.
type waiter struct {
	ch     chan message
	cancel func()
}

func (c *conn) wait(pred func(message) bool) *waiter {
	w := &waiter{ch: make(chan message, 64)}
	w.cancel = c.on(func(m message) {
		if pred(m) {
			select {
			case w.ch <- m:
			default:
			}
		}
	})
	return w
}

// next returns the next matching event, or an error when ctx ends or the
// connection closes.
func (w *waiter) next(ctx context.Context, c *conn) (message, error) {
	select {
	case m := <-w.ch:
		return m, nil
	case <-ctx.Done():
		return message{}, ctx.Err()
	case <-c.done:
		return message{}, errClosed
	}
}
