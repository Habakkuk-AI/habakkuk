package memory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeEmbeddings is an OpenAI-compatible embeddings server whose vectors
// count concept words, so texts that share a concept but no words come out
// close. It records every text it was asked to embed.
type fakeEmbeddings struct {
	mu    sync.Mutex
	texts []string
	fail  bool
}

var concepts = [][]string{
	{"coffee", "espresso", "latte", "flat", "caffeine", "brew"},
	{"milk", "oat", "dairy", "cream"},
	{"city", "lives", "home", "toronto", "lagos", "moved", "live"},
	{"dog", "pet", "puppy", "walks"},
	{"meeting", "standup", "calendar", "schedule"},
}

func conceptVector(text string) []float32 {
	v := make([]float32, len(concepts)+1)
	v[len(concepts)] = 0.05 // never all zero
	for _, w := range strings.Fields(strings.ToLower(text)) {
		w = strings.Trim(w, ".,?!:;\"'")
		for i, c := range concepts {
			for _, x := range c {
				if w == x {
					v[i]++
				}
			}
		}
	}
	return v
}

func (f *fakeEmbeddings) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	fail := f.fail
	f.mu.Unlock()
	if fail {
		http.Error(w, `{"error":"overloaded"}`, http.StatusServiceUnavailable)
		return
	}
	if r.Header.Get("Authorization") != "Bearer test-key" {
		http.Error(w, "no key", http.StatusUnauthorized)
		return
	}
	var req struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model != "embed-small" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.texts = append(f.texts, req.Input...)
	f.mu.Unlock()
	type item struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	}
	var out struct {
		Data []item `json:"data"`
	}
	// Answer out of order, as the index field allows.
	for i := len(req.Input) - 1; i >= 0; i-- {
		out.Data = append(out.Data, item{i, conceptVector(req.Input[i])})
	}
	json.NewEncoder(w).Encode(out)
}

func (f *fakeEmbeddings) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.texts...)
}

func semanticStore(t *testing.T) (*Store, *fakeEmbeddings) {
	t.Helper()
	s := newStore(t)
	f := &fakeEmbeddings{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	s.UseEmbedder(&HTTPEmbedder{Kind: "openai", Provider: "local", BaseURL: srv.URL + "/v1", Key: "test-key", Model: "embed-small"}, nil)
	return s, f
}

func TestSemanticSearchFindsNotesWithoutSharedWords(t *testing.T) {
	ctx := context.Background()
	s, f := semanticStore(t)
	for _, n := range [][2]string{
		{"Coffee order", "Flat white with oat milk, no sugar."},
		{"Home", "Lives in Toronto since 2024."},
		{"Dog", "Has a puppy called Ada; walks at 7."},
	} {
		if _, _, err := s.Save(ctx, "habakkuk", n[0], n[1], nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(f.calls()); got != 3 {
		t.Fatalf("embedded %d texts on save, want 3", got)
	}
	// No word in common with the note, but the same concept.
	hits, err := s.Search(ctx, "habakkuk", "what espresso drink do they like", nil, 3)
	if err != nil || len(hits) == 0 || hits[0].Slug != "coffee-order" {
		t.Fatalf("semantic search: %+v %v", hits, err)
	}
	// Unrelated notes fall under the similarity floor.
	for _, h := range hits {
		if h.Slug == "dog" {
			t.Fatalf("dog note matched a coffee query: %+v", hits)
		}
	}
	// Full-text and semantic agree: the fused list keeps the FTS snippet.
	hits, err = s.Search(ctx, "habakkuk", "Toronto", nil, 3)
	if err != nil || len(hits) == 0 || hits[0].Slug != "home" || !strings.Contains(hits[0].Snippet, "[Toronto]") {
		t.Fatalf("fused search: %+v %v", hits, err)
	}
	// Notes are embedded once: searches embed only the query.
	before := len(f.calls())
	if _, err := s.Search(ctx, "habakkuk", "pet", nil, 3); err != nil {
		t.Fatal(err)
	}
	if got := f.calls()[before:]; len(got) != 1 || got[0] != "pet" {
		t.Fatalf("search embedded %q, want only the query", got)
	}
	// An edit re-embeds just that note; the old vector stays cached by hash.
	if _, _, err := s.Save(ctx, "habakkuk", "Dog", "Has a puppy called Ada; walks at 8.", nil); err != nil {
		t.Fatal(err)
	}
	if got := f.calls()[before+1:]; len(got) != 1 || !strings.Contains(got[0], "walks at 8") {
		t.Fatalf("edit embedded %q", got)
	}
}

func TestSemanticSearchFallsBackToFullText(t *testing.T) {
	ctx := context.Background()
	s, f := semanticStore(t)
	f.fail = true
	if _, _, err := s.Save(ctx, "habakkuk", "Coffee order", "Flat white with oat milk.", nil); err != nil {
		t.Fatalf("save must not fail when embedding does: %v", err)
	}
	hits, err := s.Search(ctx, "habakkuk", "oat milk", nil, 3)
	if err != nil || len(hits) != 1 || hits[0].Slug != "coffee-order" {
		t.Fatalf("fallback search: %+v %v", hits, err)
	}
	// Once the endpoint is back (and the pause has passed), the pending
	// note is embedded and semantic search works.
	f.mu.Lock()
	f.fail = false
	f.mu.Unlock()
	s.embDown = s.now().Add(-1)
	hits, err = s.Search(ctx, "habakkuk", "latte", nil, 3)
	if err != nil || len(hits) != 1 || hits[0].Slug != "coffee-order" {
		t.Fatalf("after recovery: %+v %v", hits, err)
	}
}

func TestVectorEncodingAndCosine(t *testing.T) {
	v := []float32{3, 4, 0}
	got := decodeVec(encodeVec(normalize(v)))
	if len(got) != 3 || got[0] < 0.59 || got[0] > 0.61 || got[1] < 0.79 || got[1] > 0.81 {
		t.Fatalf("round trip: %v", got)
	}
	if c := Cosine([]float32{1, 0}, []float32{2, 0}); c < 0.999 {
		t.Fatalf("cosine of parallel vectors = %v", c)
	}
	if c := Cosine([]float32{1, 0}, []float32{0, 1}); c != 0 {
		t.Fatalf("cosine of orthogonal vectors = %v", c)
	}
}

func TestFuseRanksAgreementFirst(t *testing.T) {
	h := func(slug string) Hit { return Hit{Entry: Entry{Slug: slug}} }
	got := fuse([]Hit{h("a"), h("b"), h("c")}, []Hit{h("c"), h("d"), h("b")}, 3)
	var slugs []string
	for _, x := range got {
		slugs = append(slugs, x.Slug)
	}
	// b and c appear in both lists and outrank a, which only full text found.
	if strings.Join(slugs, ",") != "c,b,a" && strings.Join(slugs, ",") != "b,c,a" {
		t.Fatalf("fused order %v", slugs)
	}
}

func TestGeminiEmbedder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/models/gemini-embedding-001:batchEmbedContents" || r.Header.Get("x-goog-api-key") != "g-key" {
			http.Error(w, "wrong request "+r.URL.Path, http.StatusBadRequest)
			return
		}
		var req struct {
			Requests []struct {
				Model   string `json:"model"`
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
				OutputDimensionality int `json:"outputDimensionality"`
			} `json:"requests"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		var out struct {
			Embeddings []struct {
				Values []float32 `json:"values"`
			} `json:"embeddings"`
		}
		for _, q := range req.Requests {
			if q.Model != "models/gemini-embedding-001" || q.OutputDimensionality != 6 {
				http.Error(w, "bad item", http.StatusBadRequest)
				return
			}
			out.Embeddings = append(out.Embeddings, struct {
				Values []float32 `json:"values"`
			}{conceptVector(q.Content.Parts[0].Text)})
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()
	e := &HTTPEmbedder{Kind: "gemini", Provider: "gemini", BaseURL: srv.URL + "/v1beta", Key: "g-key", Model: "gemini-embedding-001", Dimensions: 6}
	vecs, err := e.Embed(context.Background(), []string{"coffee", "dog walks"})
	if err != nil || len(vecs) != 2 || vecs[0][0] != 1 || vecs[1][3] != 2 {
		t.Fatalf("gemini: %v %v", vecs, err)
	}
	if e.ID() != "gemini/gemini-embedding-001:6" {
		t.Fatalf("id %q", e.ID())
	}
}
