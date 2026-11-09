// Package mediatools gives agents senses and media beyond text: speech
// (text-to-speech for voice notes), image generation, image viewing for any
// model, document text extraction, and web search for models without hosted
// search. Providers are reached over plain HTTP; keys come from the
// environment variables the config names.
package mediatools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// SearchToolset holds web_search. Agents whose main model has no hosted
// search get it in place of the provider's tool.
const SearchToolset = "search"

// Kit is the configured media providers. A nil field is not configured.
type Kit struct {
	Speech *Speech
	Images *ImageGen
	Video  *VideoGen
	Search *Searcher
}

// New builds the kit from config. secret resolves a variable name to its
// value (environment first, then $HABAKKUK_HOME/.env).
func New(cfg *config.Config, secret func(string) string) *Kit {
	k := &Kit{}
	key := func(env string) string {
		if env == "" {
			return ""
		}
		return secret(env)
	}
	if t := cfg.TTS; t != nil {
		k.Speech = &Speech{Provider: t.Provider, Key: key(t.KeyEnv), KeyEnv: t.KeyEnv, BaseURL: t.BaseURL,
			Model: t.Model, Voice: t.Voice, Format: t.Format, Instructions: t.Instructions, MaxChars: t.MaxChars}
	}
	if im := cfg.Images; im != nil {
		k.Images = &ImageGen{Provider: im.Provider, Key: key(im.KeyEnv), KeyEnv: im.KeyEnv, BaseURL: im.BaseURL,
			Model: im.Model, EditModel: im.EditModel, Size: im.Size}
	}
	if v := cfg.Video; v != nil {
		k.Video = &VideoGen{Provider: v.Provider, Key: key(v.KeyEnv), KeyEnv: v.KeyEnv, BaseURL: v.BaseURL,
			Model: v.Model, ImageModel: v.ImageModel, Size: v.Size, Seconds: v.Seconds, Timeout: v.Timeout}
	}
	// No search provider is contacted unless config.yaml names one
	// (duckduckgo included): nothing phones out by default.
	if s := cfg.Search; s != nil {
		k.Search = &Searcher{Provider: s.Provider, Key: key(s.KeyEnv), KeyEnv: s.KeyEnv, URL: s.URL}
	}
	return k
}

// Register adds view_image and read_document to the core toolset,
// image_generate and video_generate when configured, and web_search to
// SearchToolset.
func (k *Kit) Register(r *tools.Registry) {
	r.Register("core", viewImageTool{client: newClient(time.Minute)})
	r.Register("core", readDocumentTool{})
	if k.Images != nil {
		r.Register("core", imageGenerateTool{gen: k.Images})
	}
	if k.Video != nil {
		r.Register("core", videoGenerateTool{gen: k.Video})
	}
	if k.Search != nil {
		r.Register(SearchToolset, webSearchTool{s: k.Search})
	}
}

// HostedSearch reports whether an agent's main model runs on a provider with
// hosted web search (Anthropic). Such agents keep the provider's tools;
// others get web_search.
func HostedSearch(cfg *config.Config, a *config.Agent) bool {
	ref, err := config.ParseModelRef(a.Model)
	if err != nil {
		return false
	}
	p := cfg.Providers[ref.Provider]
	return p != nil && p.Type == "anthropic"
}

const userAgent = "Habakkuk/0.1 (+https://github.com/habakkuk-ai/habakkuk)"

func newClient(timeout time.Duration) *http.Client { return &http.Client{Timeout: timeout} }

// doJSON sends a request and decodes a JSON answer into out (when non-nil).
// A non-2xx status is an error carrying the start of the body.
func doJSON(ctx context.Context, client *http.Client, req *http.Request, out any) error {
	raw, err := doRaw(ctx, client, req, 8<<20)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("unreadable response: %w", err)
	}
	return nil
}

// doRaw sends a request and returns at most limit bytes of a 2xx body.
func doRaw(ctx context.Context, client *http.Client, req *http.Request, limit int64) ([]byte, error) {
	req = req.WithContext(ctx)
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", userAgent)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet(raw))
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("response is larger than %d MB", limit>>20)
	}
	return raw, nil
}

func snippet(raw []byte) string {
	s := strings.Join(strings.Fields(string(raw)), " ")
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

func decode[T any](input json.RawMessage) (T, error) {
	var v T
	if err := json.Unmarshal(input, &v); err != nil {
		return v, fmt.Errorf("invalid input: %w", err)
	}
	return v, nil
}

func missingKey(what, env string) error {
	if env == "" {
		return fmt.Errorf("%s: no API key configured", what)
	}
	return fmt.Errorf("%s: the API key is not set (put %s in the environment or .env)", what, env)
}

var errNoBlobs = errors.New("no blob store to hold images")
