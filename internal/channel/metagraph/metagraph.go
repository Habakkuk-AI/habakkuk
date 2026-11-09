// Package metagraph holds what the WhatsApp Cloud API, Messenger and
// Instagram adapters share: Meta's Graph API with its error codes, the
// webhook subscription handshake (hub.challenge) and the X-Hub-Signature-256
// body signature.
package metagraph

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/channel/upload"
)

// DefaultBase is the Graph API version the adapters are written against.
const DefaultBase = "https://graph.facebook.com/v24.0"

// ValidSignature checks X-Hub-Signature-256: "sha256=" and the hex HMAC-SHA256
// of the raw body, keyed with the app secret.
func ValidSignature(appSecret string, body []byte, header string) bool {
	hexSum, ok := strings.CutPrefix(header, "sha256=")
	if !ok || appSecret == "" {
		return false
	}
	got, err := hex.DecodeString(hexSum)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// Sign returns the X-Hub-Signature-256 value for body (for tests and fakes).
func Sign(appSecret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Challenge answers the GET Meta sends when the webhook is subscribed:
// it echoes hub.challenge when hub.verify_token matches.
func Challenge(w http.ResponseWriter, r *http.Request, verifyToken string) {
	q := r.URL.Query()
	tok := q.Get("hub.verify_token")
	if q.Get("hub.mode") != "subscribe" || verifyToken == "" ||
		!hmac.Equal([]byte(tok), []byte(verifyToken)) {
		http.Error(w, "verification failed", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = io.WriteString(w, q.Get("hub.challenge"))
}

// Error is a Graph API error response.
type Error struct {
	Name    string // the adapter, for messages
	Path    string
	Status  int
	Code    int
	Subcode int
	Message string
	Details string
}

func (e *Error) Error() string {
	s := fmt.Sprintf("%s %s: HTTP %d", e.Name, e.Path, e.Status)
	if e.Code != 0 {
		s += fmt.Sprintf(" (code %d", e.Code)
		if e.Subcode != 0 {
			s += fmt.Sprintf("/%d", e.Subcode)
		}
		s += ")"
	}
	if e.Message != "" {
		s += " " + e.Message
	}
	if e.Details != "" && e.Details != e.Message {
		s += ": " + e.Details
	}
	return s
}

// Codes that mean "slow down": app, account and pair rate limits.
var rateCodes = map[int]bool{4: true, 17: true, 32: true, 613: true, 80007: true, 130429: true, 131048: true, 131056: true}

// classify wraps a Graph error in the channel error the gateway acts on.
func classify(e *Error, header http.Header) error {
	switch {
	case e.Code == 190 || e.Status == http.StatusUnauthorized:
		return channel.Fatal{Err: e}
	case rateCodes[e.Code] || e.Status == http.StatusTooManyRequests:
		after, _ := strconv.Atoi(header.Get("Retry-After"))
		if after <= 0 {
			after = 60
		}
		return channel.RetryAfter{Err: e, After: time.Duration(after) * time.Second}
	case e.Status >= 500 || e.Code == 1 || e.Code == 2:
		return e // transient: retry
	case e.Status >= 400:
		return channel.Undeliverable{Err: e}
	}
	return e
}

// Client calls the Graph API with a bearer token.
type Client struct {
	Name  string // "whatsapp", "messenger", ...
	Base  string // e.g. DefaultBase
	Token string
	HTTP  *http.Client
}

// JSON sends body (when not nil) as JSON and decodes the reply into out.
func (c *Client) JSON(ctx context.Context, method, path string, body, out any) error {
	var r io.Reader
	ctype := ""
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r, ctype = bytes.NewReader(raw), "application/json"
	}
	return c.do(ctx, method, path, r, ctype, out)
}

// Part is one file in a multipart upload.
type Part = upload.File

// Multipart posts form fields and one file.
func (c *Client) Multipart(ctx context.Context, path string, fields map[string]string, file Part, out any) error {
	body, ctype, err := upload.Form(fields, file)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, path, bytes.NewReader(body), ctype, out)
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, ctype string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", c.Name, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 300 {
		if out != nil && len(bytes.TrimSpace(raw)) > 0 { // 204 No Content has no body
			return json.Unmarshal(raw, out)
		}
		return nil
	}
	var env struct {
		Error struct {
			Message   string `json:"message"`
			Code      int    `json:"code"`
			Subcode   int    `json:"error_subcode"`
			ErrorData struct {
				Details string `json:"details"`
			} `json:"error_data"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &env)
	e := &Error{Name: c.Name, Path: path, Status: resp.StatusCode, Code: env.Error.Code, Subcode: env.Error.Subcode,
		Message: env.Error.Message, Details: env.Error.ErrorData.Details}
	return classify(e, resp.Header)
}

// Download fetches a URL, with the bearer token when auth is set (WhatsApp
// media URLs need it; Messenger CDN URLs are signed and must not get it).
func (c *Client) Download(ctx context.Context, url string, auth bool, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s media: HTTP %d", c.Name, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s media: larger than %d bytes", c.Name, limit)
	}
	return data, nil
}

// Code reports the Graph error code inside err, or 0.
func Code(err error) (code, subcode int) {
	var e *Error
	if errors.As(err, &e) {
		return e.Code, e.Subcode
	}
	return 0, 0
}
