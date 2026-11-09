package nodes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// ErrRejected means the server refused the node's secret: it was revoked
// or paired again elsewhere. Reconnecting cannot help; pair again.
var ErrRejected = errors.New("the server does not accept this node; pair it again")

// Pair redeems a pairing code with the server at base (http or https) and
// returns the node's secret.
func Pair(ctx context.Context, hc *http.Client, base, code, name, platform string) (string, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	body, _ := json.Marshal(pairRequest{Code: code, Name: name, Platform: platform})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/nodes/pair", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var out struct {
		Secret string `json:"secret"`
		Error  string `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("pairing: %s", res.Status)
	}
	if res.StatusCode != http.StatusOK || out.Secret == "" {
		if out.Error == "" {
			out.Error = res.Status
		}
		return "", fmt.Errorf("pairing refused: %s", out.Error)
	}
	return out.Secret, nil
}

// Client is the device side: it holds a websocket to the server's /nodes
// endpoint, answers invocations with its capabilities, and reconnects with
// backoff whenever the connection drops.
type Client struct {
	Server   string // the server's base URL, http or https
	Name     string
	Secret   string
	Platform string
	Version  string
	Caps     Caps
	Log      *slog.Logger
	HTTP     *http.Client
	// Heartbeat is how often the client pings (default 20s); a ping that
	// gets no answer within it drops the connection.
	Heartbeat time.Duration
	// MinBackoff and MaxBackoff bound the wait between reconnects
	// (default 1s and 1m); the wait doubles after each failure and resets
	// after a connection that lasted a minute.
	MinBackoff, MaxBackoff time.Duration
	// OnConnect and OnDisconnect report the connection's state, for the
	// CLI and tests.
	OnConnect    func()
	OnDisconnect func(err error, retryIn time.Duration)
}

func (c *Client) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

// WebsocketURL is the node endpoint for a server base URL.
func WebsocketURL(base string) (string, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("server %q: want an http or https URL", base)
	}
	u.Path += "/nodes"
	return u.String(), nil
}

// Run connects and serves until ctx ends, reconnecting after every drop.
// It returns ErrRejected when the server refuses the node.
func (c *Client) Run(ctx context.Context) error {
	minB, maxB := c.MinBackoff, c.MaxBackoff
	if minB <= 0 {
		minB = time.Second
	}
	if maxB < minB {
		maxB = max(time.Minute, minB)
	}
	wait := minB
	for {
		start := time.Now()
		err := c.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, ErrRejected) {
			return err
		}
		if time.Since(start) > time.Minute {
			wait = minB
		}
		// Up to a quarter less, so many nodes do not all reconnect at once.
		sleep := wait - time.Duration(rand.Int64N(int64(wait)/4+1))
		if c.OnDisconnect != nil {
			c.OnDisconnect(err, sleep)
		}
		c.log().Warn("node connection lost", "err", err, "retry_in", sleep)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(sleep):
		}
		wait = min(wait*2, maxB)
	}
}

func (c *Client) heartbeat() time.Duration {
	if c.Heartbeat > 0 {
		return c.Heartbeat
	}
	return 20 * time.Second
}

// session is one connection: hello, welcome, then invocations until the
// connection fails.
func (c *Client) session(ctx context.Context) error {
	u, err := WebsocketURL(c.Server)
	if err != nil {
		return err
	}
	dctx, dcancel := context.WithTimeout(ctx, 30*time.Second)
	conn, _, err := websocket.Dial(dctx, u, &websocket.DialOptions{HTTPClient: c.HTTP})
	dcancel()
	if err != nil {
		return err
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(MaxFrame)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	hello := Frame{Type: "hello", Name: c.Name, Secret: c.Secret, Caps: c.Caps.Names(), Platform: c.Platform, Version: c.Version}
	if err := wsjson.Write(ctx, conn, hello); err != nil {
		return err
	}
	wctx, wcancel := context.WithTimeout(ctx, 15*time.Second)
	var welcome Frame
	err = wsjson.Read(wctx, conn, &welcome)
	wcancel()
	if err != nil {
		if websocket.CloseStatus(err) == websocket.StatusPolicyViolation {
			return ErrRejected
		}
		return err
	}
	if welcome.Type == "error" {
		return fmt.Errorf("%w: %s", ErrRejected, welcome.Error)
	}
	if welcome.Type != "welcome" {
		return fmt.Errorf("unexpected %q from the server", welcome.Type)
	}
	if c.OnConnect != nil {
		c.OnConnect()
	}
	c.log().Info("node connected", "server", c.Server, "name", c.Name)

	out := make(chan Frame, 16)
	go func() { // the only writer
		for {
			select {
			case f := <-out:
				if err := wsjson.Write(ctx, conn, f); err != nil {
					cancel()
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { // heartbeat
		t := time.NewTicker(c.heartbeat())
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			pctx, pcancel := context.WithTimeout(ctx, c.heartbeat())
			err := conn.Ping(pctx)
			pcancel()
			if err != nil {
				cancel()
				return
			}
		}
	}()
	for {
		var f Frame
		if err := wsjson.Read(ctx, conn, &f); err != nil {
			return err
		}
		switch f.Type {
		case "invoke":
			go c.invoke(ctx, f, out)
		case "ping":
			select {
			case out <- Frame{Type: "pong"}:
			default:
			}
		}
	}
}

// invoke runs one command and queues its result.
func (c *Client) invoke(ctx context.Context, f Frame, out chan<- Frame) {
	res := &Result{}
	if capability, ok := c.Caps[f.Command]; !ok {
		res.Error = "this node does not offer " + f.Command
	} else {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		r, err := capability.Run(rctx, f.Args)
		cancel()
		switch {
		case err != nil:
			res.Error = err.Error()
		case r != nil:
			res = r
		}
	}
	for _, file := range res.Files {
		if len(file.Data) > MaxFile {
			res = &Result{Error: fmt.Sprintf("%s is over the %d MB limit", file.Name, MaxFile>>20)}
			break
		}
	}
	c.log().Info("node invocation", "command", f.Command, "ok", res.Error == "")
	select {
	case out <- Frame{Type: "result", ID: f.ID, Result: res}:
	case <-ctx.Done():
	}
}
