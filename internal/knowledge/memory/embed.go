package memory

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Embedder turns texts into vectors for semantic search.
type Embedder interface {
	// Embed returns one vector per text, in order.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// ID names the model and its settings; vectors are cached under it, so
	// switching models re-embeds rather than mixing incompatible vectors.
	ID() string
}

const (
	embedBatch = 64  // texts per request
	embedMax   = 512 // texts embedded per pass, so one pass stays short
	// minSimilarity is the cosine below which a note is not offered as a
	// semantic match at all; fusion only reorders what passes it.
	minSimilarity = 0.25
	// embedBackoff is how long embedding is skipped after a failure, so a
	// dead endpoint does not slow every search down.
	embedBackoff = time.Minute
	embedTimeout = 30 * time.Second
	queryTimeout = 10 * time.Second
)

func (s *Store) embedAvailable() bool {
	if s.emb == nil {
		return false
	}
	s.embMu.Lock()
	defer s.embMu.Unlock()
	return !s.now().Before(s.embDown)
}

func (s *Store) embedFailed(err error) {
	s.embMu.Lock()
	s.embDown = s.now().Add(embedBackoff)
	s.embMu.Unlock()
	s.log.Warn("embedding failed; semantic memory search is paused for a minute", "model", s.emb.ID(), "err", err)
}

// EmbedPending embeds an agent's notes that have no vector for the current
// model yet, in batches. Vectors are cached by content hash, so only new or
// changed text is sent. Failures are logged and leave search on full text.
func (s *Store) EmbedPending(ctx context.Context, agent string) {
	if !s.embedAvailable() {
		return
	}
	if err := s.embedPending(ctx, agent); err != nil && ctx.Err() == nil {
		s.embedFailed(err)
	}
}

func (s *Store) embedPending(ctx context.Context, agent string) error {
	model := s.emb.ID()
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT m.chash, f.title, f.tags, f.body
		FROM memories m JOIN memory_fts f ON f.rowid = m.id
		WHERE m.agent = ? AND NOT EXISTS (SELECT 1 FROM embeddings e WHERE e.model = ? AND e.hash = m.chash)
		LIMIT ?`, agent, model, embedMax)
	if err != nil {
		return err
	}
	var hashes, texts []string
	for rows.Next() {
		var hash, title, tags, body string
		if err := rows.Scan(&hash, &title, &tags, &body); err != nil {
			rows.Close()
			return err
		}
		if slices.Contains(hashes, hash) {
			continue
		}
		hashes = append(hashes, hash)
		texts = append(texts, embedText(Memory{Title: title, Tags: strings.Fields(tags), Body: body}))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for start := 0; start < len(texts); start += embedBatch {
		end := min(start+embedBatch, len(texts))
		bctx, cancel := context.WithTimeout(ctx, embedTimeout)
		vecs, err := s.emb.Embed(bctx, texts[start:end])
		cancel()
		if err != nil {
			return err
		}
		if len(vecs) != end-start {
			return fmt.Errorf("embedding returned %d vectors for %d texts", len(vecs), end-start)
		}
		if err := s.storeVectors(ctx, model, hashes[start:end], vecs); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) storeVectors(ctx context.Context, model string, hashes []string, vecs [][]float32) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now().Unix()
	for i, v := range vecs {
		if len(v) == 0 {
			return errors.New("embedding returned an empty vector")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO embeddings (model, hash, vec, used) VALUES (?, ?, ?, ?)
			ON CONFLICT (model, hash) DO UPDATE SET used = excluded.used`, model, hashes[i], encodeVec(normalize(v)), now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// semantic ranks an agent's embedded notes by cosine similarity to query.
func (s *Store) semantic(ctx context.Context, agent, query, filter string, filterArgs []any, limit int) ([]Hit, error) {
	if !s.embedAvailable() {
		return nil, errors.New("embedding paused after a recent failure")
	}
	if err := s.embedPending(ctx, agent); err != nil {
		if ctx.Err() == nil {
			s.embedFailed(err)
		}
		return nil, err
	}
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	vecs, err := s.emb.Embed(qctx, []string{cutUTF8(query, maxEmbedChars)})
	if err == nil && (len(vecs) != 1 || len(vecs[0]) == 0) {
		err = errors.New("embedding returned no vector for the query")
	}
	if err != nil {
		if ctx.Err() == nil {
			s.embedFailed(err)
		}
		return nil, err
	}
	q := normalize(vecs[0])
	rows, err := s.db.QueryContext(ctx, `SELECT m.slug, m.title, m.tags, m.updated, m.trust, substr(f.body, 1, 240), e.vec
		FROM memories m JOIN embeddings e ON e.model = ? AND e.hash = m.chash JOIN memory_fts f ON f.rowid = m.id
		WHERE m.agent = ?`+filter, append([]any{s.emb.ID(), agent}, filterArgs...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type scored struct {
		h   Hit
		sim float32
	}
	var all []scored
	for rows.Next() {
		var h Hit
		var tags, trust string
		var updated int64
		var blob []byte
		if err := rows.Scan(&h.Slug, &h.Title, &tags, &updated, &trust, &h.Snippet, &blob); err != nil {
			return nil, err
		}
		v := decodeVec(blob)
		if len(v) != len(q) {
			continue // another dimension setting under the same id; never matches
		}
		sim := dot(q, v)
		if sim < minSimilarity {
			continue
		}
		h.Tags, h.Updated, h.Untrusted = splitTags(tags), time.Unix(updated, 0).UTC(), trust == TrustUntrusted
		h.Snippet = strings.Join(strings.Fields(h.Snippet), " ")
		all = append(all, scored{h, sim})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.SortStableFunc(all, func(a, b scored) int {
		switch {
		case a.sim > b.sim:
			return -1
		case a.sim < b.sim:
			return 1
		}
		return strings.Compare(a.h.Slug, b.h.Slug)
	})
	out := make([]Hit, 0, min(limit, len(all)))
	for _, sc := range all[:min(limit, len(all))] {
		out = append(out, sc.h)
	}
	return out, nil
}

// Vectors returns the cached vector of each of an agent's embedded notes,
// by slug, for the nightly pass's duplicate check. Nil without an embedder.
func (s *Store) Vectors(ctx context.Context, agent string) (map[string][]float32, error) {
	if s.emb == nil {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT m.slug, e.vec FROM memories m
		JOIN embeddings e ON e.model = ? AND e.hash = m.chash WHERE m.agent = ?`, s.emb.ID(), agent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]float32{}
	for rows.Next() {
		var slug string
		var blob []byte
		if err := rows.Scan(&slug, &blob); err != nil {
			return nil, err
		}
		out[slug] = decodeVec(blob)
	}
	return out, rows.Err()
}

// Cosine is the cosine similarity of two vectors of equal length.
func Cosine(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	return dot(normalize(a), normalize(b))
}

func dot(a, b []float32) float32 {
	var s float32
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

func normalize(v []float32) []float32 {
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	if n == 0 {
		return v
	}
	inv := float32(1 / math.Sqrt(n))
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x * inv
	}
	return out
}

func encodeVec(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(x))
	}
	return b
}

func decodeVec(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v
}

// HTTPEmbedder calls an embeddings API over plain HTTP: an OpenAI-compatible
// /embeddings endpoint (OpenAI, local servers such as Ollama, gateways) or
// Gemini's batchEmbedContents.
type HTTPEmbedder struct {
	Kind       string // "openai" or "gemini"
	Provider   string // the config provider name, for ID
	BaseURL    string // default https://api.openai.com/v1 or the Gemini v1beta URL
	Key        string // may be empty for local servers
	Model      string
	Dimensions int // 0 is the model's default
	Client     *http.Client
}

func (e *HTTPEmbedder) ID() string {
	id := e.Provider + "/" + e.Model
	if e.Dimensions > 0 {
		id += fmt.Sprintf(":%d", e.Dimensions)
	}
	return id
}

func (e *HTTPEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if e.Kind == "gemini" {
		return e.gemini(ctx, texts)
	}
	return e.openai(ctx, texts)
}

func (e *HTTPEmbedder) openai(ctx context.Context, texts []string) ([][]float32, error) {
	base := e.BaseURL
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	body := map[string]any{"model": e.Model, "input": texts}
	if e.Dimensions > 0 {
		body["dimensions"] = e.Dimensions
	}
	header := http.Header{}
	if e.Key != "" {
		header.Set("Authorization", "Bearer "+e.Key)
	}
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := e.post(ctx, strings.TrimRight(base, "/")+"/embeddings", header, body, &out); err != nil {
		return nil, err
	}
	vecs := make([][]float32, len(texts))
	for i, d := range out.Data {
		idx := d.Index
		if idx < 0 || idx >= len(vecs) || vecs[idx] != nil {
			idx = i // servers that leave index out answer in order
		}
		if idx >= len(vecs) {
			return nil, fmt.Errorf("embedding response has %d vectors for %d texts", len(out.Data), len(texts))
		}
		vecs[idx] = d.Embedding
	}
	for _, v := range vecs {
		if v == nil {
			return nil, fmt.Errorf("embedding response has %d vectors for %d texts", len(out.Data), len(texts))
		}
	}
	return vecs, nil
}

func (e *HTTPEmbedder) gemini(ctx context.Context, texts []string) ([][]float32, error) {
	base := e.BaseURL
	if base == "" {
		base = "https://generativelanguage.googleapis.com/v1beta"
	}
	model := strings.TrimPrefix(e.Model, "models/")
	type part struct {
		Text string `json:"text"`
	}
	type request struct {
		Model   string `json:"model"`
		Content struct {
			Parts []part `json:"parts"`
		} `json:"content"`
		OutputDimensionality int `json:"outputDimensionality,omitempty"`
	}
	reqs := make([]request, len(texts))
	for i, t := range texts {
		reqs[i].Model = "models/" + model
		reqs[i].Content.Parts = []part{{Text: t}}
		reqs[i].OutputDimensionality = e.Dimensions
	}
	header := http.Header{}
	if e.Key != "" {
		header.Set("x-goog-api-key", e.Key)
	}
	var out struct {
		Embeddings []struct {
			Values []float32 `json:"values"`
		} `json:"embeddings"`
	}
	url := strings.TrimRight(base, "/") + "/models/" + model + ":batchEmbedContents"
	if err := e.post(ctx, url, header, map[string]any{"requests": reqs}, &out); err != nil {
		return nil, err
	}
	if len(out.Embeddings) != len(texts) {
		return nil, fmt.Errorf("embedding response has %d vectors for %d texts", len(out.Embeddings), len(texts))
	}
	vecs := make([][]float32, len(texts))
	for i, em := range out.Embeddings {
		vecs[i] = em.Values
	}
	return vecs, nil
}

func (e *HTTPEmbedder) post(ctx context.Context, url string, header http.Header, body, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header = header
	req.Header.Set("Content-Type", "application/json")
	client := e.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		msg := strings.Join(strings.Fields(string(raw)), " ")
		if len(msg) > 300 {
			msg = msg[:300] + "..."
		}
		return fmt.Errorf("embeddings: %s: %s", resp.Status, msg)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("embeddings: decode response: %w", err)
	}
	return nil
}
