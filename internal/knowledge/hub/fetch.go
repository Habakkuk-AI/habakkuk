package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// DefaultGitHubAPI is GitHub's REST API.
const DefaultGitHubAPI = "https://api.github.com"

// Fetcher downloads sources. The zero value works; tests point GitHubAPI
// and the registries at local servers.
type Fetcher struct {
	HTTP      *http.Client
	GitHubAPI string        // default DefaultGitHubAPI
	Token     func() string // optional GitHub token, to lift the anonymous rate limit
	// Registries are the indexes a bare name or registry: source is looked
	// up in, in order.
	Registries []Registry
}

// Fetched is a source downloaded into a work folder.
type Fetched struct {
	Root     string // the folder holding the source's content
	Version  string // a commit, a registry version, or the sha256 of a download
	Origin   Source // the concrete source (a registry name resolved)
	Registry string // the registry that named it
	Expect   string // the TreeHash the registry promised, or ""
}

func (f *Fetcher) client() *http.Client {
	if f.HTTP != nil {
		return f.HTTP
	}
	return &http.Client{Timeout: 2 * time.Minute}
}

func (f *Fetcher) api() string {
	if f.GitHubAPI != "" {
		return strings.TrimRight(f.GitHubAPI, "/")
	}
	return DefaultGitHubAPI
}

// Fetch downloads src into work (an empty folder). A plain file (not an
// archive) is saved under the name single, the marker file of the package
// kind. kind ("skills" or "plugins") picks the registry list.
func (f *Fetcher) Fetch(ctx context.Context, src Source, work, single, kind string) (*Fetched, error) {
	out := &Fetched{Origin: src}
	if src.Kind == KindRegistry {
		e, reg, err := f.Lookup(ctx, kind, src.Registry, src.Name)
		if err != nil {
			return nil, err
		}
		inner, err := ParseSource(e.Source)
		if err != nil {
			return nil, fmt.Errorf("registry %s, %s: %w", reg, src.Name, err)
		}
		if inner.Kind == KindRegistry {
			return nil, fmt.Errorf("registry %s, %s: source %q points at another registry entry", reg, src.Name, e.Source)
		}
		got, err := f.Fetch(ctx, inner, work, single, kind)
		if err != nil {
			return nil, err
		}
		got.Registry, got.Expect = reg, strings.ToLower(e.SHA256)
		if e.Version != "" && inner.Kind != KindGitHub {
			got.Version = e.Version
		}
		return got, nil
	}
	root := filepath.Join(work, "src")
	switch src.Kind {
	case KindGitHub:
		sha, err := f.resolveGitHub(ctx, src)
		if err != nil {
			return nil, err
		}
		body, err := f.get(ctx, fmt.Sprintf("%s/repos/%s/%s/tarball/%s", f.api(), src.Owner, src.Repo, sha), true)
		if err != nil {
			return nil, err
		}
		if err := extractTarGz(bytes.NewReader(body), root, 1); err != nil {
			return nil, fmt.Errorf("%s: %w", src, err)
		}
		out.Version = sha
		if src.Path != "" {
			root = filepath.Join(root, filepath.FromSlash(src.Path))
			if !isDir(root) {
				return nil, fmt.Errorf("%s: no folder %s in the repository", src, src.Path)
			}
		}
	case KindURL:
		body, err := f.get(ctx, src.URL, false)
		if err != nil {
			return nil, err
		}
		u, _ := url.Parse(src.URL)
		if err := unpack(u.Path, body, root, single); err != nil {
			return nil, fmt.Errorf("%s: %w", src, err)
		}
		sum := sha256.Sum256(body)
		out.Version = hex.EncodeToString(sum[:])
		root = unwrap(root, single)
	case KindLocal:
		info, err := os.Stat(src.Dir)
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			if err := copyTree(src.Dir, root); err != nil {
				return nil, err
			}
		} else {
			if info.Size() > MaxDownload {
				return nil, errTooBig
			}
			data, err := os.ReadFile(src.Dir)
			if err != nil {
				return nil, err
			}
			if err := unpack(src.Dir, data, root, single); err != nil {
				return nil, err
			}
			root = unwrap(root, single)
		}
	default:
		return nil, fmt.Errorf("cannot fetch source %q", src)
	}
	out.Root = root
	return out, nil
}

// resolveGitHub turns the source's ref (default the repository's default
// branch) into a commit, so the lock records exactly what was installed.
func (f *Fetcher) resolveGitHub(ctx context.Context, src Source) (string, error) {
	if reSHA.MatchString(src.Ref) {
		return src.Ref, nil
	}
	ref := src.Ref
	if ref == "" {
		ref = "HEAD"
	}
	req, err := f.request(ctx, fmt.Sprintf("%s/repos/%s/%s/commits/%s", f.api(), src.Owner, src.Repo, url.PathEscape(ref)), true)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github.sha")
	body, err := f.do(req)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", src, err)
	}
	sha := strings.TrimSpace(string(body))
	if !reSHA.MatchString(sha) {
		return "", fmt.Errorf("resolve %s: unexpected answer %q", src, shorten(sha, 80))
	}
	return sha, nil
}

func (f *Fetcher) request(ctx context.Context, u string, github bool) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "habakkuk-hub")
	if github && f.Token != nil {
		if tok := f.Token(); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	return req, nil
}

func (f *Fetcher) get(ctx context.Context, u string, github bool) ([]byte, error) {
	req, err := f.request(ctx, u, github)
	if err != nil {
		return nil, err
	}
	return f.do(req)
}

func (f *Fetcher) do(req *http.Request) ([]byte, error) {
	resp, err := f.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if final := resp.Request.URL; final.Scheme != "https" && !loopbackHost(final.Hostname()) {
		return nil, fmt.Errorf("%s: redirected to a plain http URL", req.URL)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxDownload+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s: %s", req.URL, resp.Status, shorten(strings.TrimSpace(string(body)), 200))
	}
	if len(body) > MaxDownload {
		return nil, fmt.Errorf("GET %s: %w (over %d MB)", req.URL, errTooBig, MaxDownload>>20)
	}
	return body, nil
}

// Registry is one index of packages.
type Registry struct {
	Name string
	URL  string
}

// Index is a registry's JSON document:
//
//	{"version": 1,
//	 "skills":  [{"name", "description", "source", "sha256", "version", "tags"}],
//	 "plugins": [...same fields...]}
//
// source is any other source form (github:..., an https URL); sha256, when
// set, is the TreeHash the installed files must have.
type Index struct {
	Version int          `json:"version"`
	Skills  []IndexEntry `json:"skills"`
	Plugins []IndexEntry `json:"plugins"`
}

// IndexEntry is one package in an index.
type IndexEntry struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Source      string   `json:"source"`
	SHA256      string   `json:"sha256,omitempty"`
	Version     string   `json:"version,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Registry    string   `json:"-"` // which registry listed it
}

// Index downloads one registry's index.
func (f *Fetcher) Index(ctx context.Context, r Registry) (*Index, error) {
	u, err := url.Parse(r.URL)
	if err != nil || (u.Scheme != "https" && (u.Scheme != "http" || !loopbackHost(u.Hostname()))) {
		return nil, fmt.Errorf("registry %s: url %q must be https", r.Name, r.URL)
	}
	body, err := f.get(ctx, r.URL, false)
	if err != nil {
		return nil, fmt.Errorf("registry %s: %w", r.Name, err)
	}
	var idx Index
	if err := json.Unmarshal(body, &idx); err != nil {
		return nil, fmt.Errorf("registry %s: bad index: %w", r.Name, err)
	}
	for _, list := range []*[]IndexEntry{&idx.Skills, &idx.Plugins} {
		for i := range *list {
			(*list)[i].Registry = r.Name
		}
	}
	return &idx, nil
}

func (idx *Index) list(kind string) []IndexEntry {
	if kind == "plugins" {
		return idx.Plugins
	}
	return idx.Skills
}

// Lookup finds name in the registries (only the one named registry, when
// set), in order.
func (f *Fetcher) Lookup(ctx context.Context, kind, registry, name string) (IndexEntry, string, error) {
	if len(f.Registries) == 0 {
		return IndexEntry{}, "", fmt.Errorf("%q looks like a registry name, but no registries are configured (skills.registries in config.yaml); give a github:, https or local source instead", name)
	}
	var errs []error
	found := false
	for _, r := range f.Registries {
		if registry != "" && r.Name != registry {
			continue
		}
		found = true
		idx, err := f.Index(ctx, r)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, e := range idx.list(kind) {
			if e.Name == name {
				return e, r.Name, nil
			}
		}
	}
	if !found {
		return IndexEntry{}, "", fmt.Errorf("no registry named %q is configured", registry)
	}
	errs = append(errs, fmt.Errorf("no %s named %q in the registries", strings.TrimSuffix(kind, "s"), name))
	return IndexEntry{}, "", errors.Join(errs...)
}

// Search returns the entries of kind whose name, description or tags
// contain every word of query, best matches (name hits) first. Registries
// that fail are reported in errs; the others still answer.
func (f *Fetcher) Search(ctx context.Context, kind, query string) (out []IndexEntry, errs []error) {
	words := strings.Fields(strings.ToLower(query))
	type hit struct {
		e     IndexEntry
		score int
	}
	var hits []hit
	for _, r := range f.Registries {
		idx, err := f.Index(ctx, r)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, e := range idx.list(kind) {
			name, text := strings.ToLower(e.Name), strings.ToLower(e.Description+" "+strings.Join(e.Tags, " "))
			score, ok := 0, true
			for _, w := range words {
				switch {
				case strings.Contains(name, w):
					score += 3
				case strings.Contains(text, w):
					score++
				default:
					ok = false
				}
			}
			if ok {
				hits = append(hits, hit{e, score})
			}
		}
	}
	slices.SortStableFunc(hits, func(a, b hit) int {
		if a.score != b.score {
			return b.score - a.score
		}
		return strings.Compare(a.e.Name, b.e.Name)
	})
	for _, h := range hits {
		out = append(out, h.e)
	}
	return out, errs
}
