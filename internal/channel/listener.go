package channel

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
)

// Endpoint is an adapter that receives over HTTP: the platform posts its
// events to Habakkuk. The HTTP server mounts it at /channels/<account>, so the
// handler sees paths relative to that ("/" for the webhook itself). It
// checks every request itself (a signature or a signed token), so it is
// served outside the API's bearer token.
type Endpoint interface {
	Adapter
	http.Handler
}

// Listener is the Run half of an Endpoint: it holds the inbox while the
// supervisor runs the adapter, so requests arriving between restarts get a
// 503 and the platform retries.
type Listener struct {
	mu sync.Mutex
	in Inbox
}

// Run makes the endpoint live until ctx ends, beating every interval.
// check, when set, runs on each beat (a ping to the platform); an error
// from it ends the run so the supervisor reconnects.
func (l *Listener) Run(ctx context.Context, in Inbox, detail string, every time.Duration, check func(context.Context) error) error {
	l.mu.Lock()
	l.in = in
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		if l.in == in {
			l.in = nil
		}
		l.mu.Unlock()
	}()
	in.Connected(detail)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if check != nil {
				if err := check(ctx); err != nil {
					if ctx.Err() != nil {
						return nil
					}
					return err
				}
			}
			in.Beat()
		}
	}
}

// Inbox is the current inbox, or nil while the adapter is not running.
func (l *Listener) Inbox() Inbox {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.in
}

// ReadBody reads a webhook body of at most limit bytes, with a deadline so a
// slow client cannot hold the connection. It answers the error itself.
func ReadBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(30 * time.Second))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		if errors.As(err, new(*http.MaxBytesError)) {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "could not read the body", http.StatusBadRequest)
		}
		return nil, false
	}
	return body, true
}

// PublicFiles serves files at unguessable URLs for an hour, for platforms
// that fetch outbound media from a URL instead of taking an upload (Twilio
// MMS, LINE images). Files live in memory: the platform fetches them within
// seconds of the send, from the same process that sent it.
type PublicFiles struct {
	mu    sync.Mutex
	files map[string]publicFile
	now   func() time.Time
}

type publicFile struct {
	data      []byte
	mediaType string
	expires   time.Time
}

const publicFileTTL = time.Hour

// Put stores a file and returns its token. The URL is the adapter's public
// base plus "/media/" plus the token.
func (p *PublicFiles) Put(data []byte, mediaType string) string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	tok := base64.RawURLEncoding.EncodeToString(b)
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clock()
	if p.files == nil {
		p.files = map[string]publicFile{}
	}
	for k, f := range p.files {
		if now.After(f.expires) {
			delete(p.files, k)
		}
	}
	p.files[tok] = publicFile{data: data, mediaType: mediaType, expires: now.Add(publicFileTTL)}
	return tok
}

func (p *PublicFiles) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// ServeHTTP answers GET /media/<token>.
func (p *PublicFiles) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tok, ok := strings.CutPrefix(r.URL.Path, "/media/")
	if !ok || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		http.NotFound(w, r)
		return
	}
	p.mu.Lock()
	f, found := p.files[tok]
	p.mu.Unlock()
	if !found || p.clock().After(f.expires) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", f.mediaType)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_, _ = w.Write(f.data)
}

// LoadState reads an adapter's JSON state file into v. A missing file
// leaves v as it is.
func LoadState(path string, v any) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// SaveState writes an adapter's JSON state file atomically. Adapter state
// lives in its own file under $HABAKKUK_HOME/channels, not in control.db.
func SaveState(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return store.WriteFileAtomic(path, data, 0o600)
}
