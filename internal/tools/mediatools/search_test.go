package mediatools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/config"
)

func TestSearchProviders(t *testing.T) {
	var lastReq *http.Request
	var lastBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastReq = r.Clone(context.Background())
		lastBody = nil
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &lastBody)
		switch r.URL.Path {
		case "/brave":
			io.WriteString(w, `{"web":{"results":[{"title":"Go","url":"https://go.dev","description":"The <strong>Go</strong> language"}]}}`)
		case "/tavily":
			io.WriteString(w, `{"results":[{"title":"Go","url":"https://go.dev","content":"Build simple software"}]}`)
		case "/exa":
			io.WriteString(w, `{"results":[{"title":"Go","url":"https://go.dev","text":"Go text","publishedDate":"2026-01-02T00:00:00Z"}]}`)
		case "/searx/search":
			io.WriteString(w, `{"results":[{"title":"Go","url":"https://go.dev","content":"from searx"},{"title":"B","url":"https://b"},{"title":"C","url":"https://c"}]}`)
		case "/ddg":
			r.ParseForm()
			io.WriteString(w, `<html><body>
				<div class="result results_links"><a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc%2F&amp;rut=abc">The <b>Go</b> docs</a>
				<a class="result__snippet" href="x">Documentation &amp; tutorials</a></div>
				<div class="result"><a class="result__a" href="https://duckduckgo.com/y.js?ad=1">Ad</a></div>
				<div class="result"><a class="result__a" href="https://example.com/b">Second</a><td class="result__snippet">two</td></div>
			</body></html>`)
		default:
			http.Error(w, "no", 404)
		}
	}))
	defer srv.Close()
	ctx := context.Background()

	check := func(name string, s *Searcher, n int, wantURL, wantSnippet string) {
		t.Helper()
		res, err := s.Search(ctx, "golang", n)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(res) == 0 || res[0].URL != wantURL || res[0].Snippet != wantSnippet {
			t.Fatalf("%s: %+v", name, res)
		}
	}

	check("brave", &Searcher{Provider: "brave", Key: "bk", URL: srv.URL + "/brave"}, 3, "https://go.dev", "The Go language")
	if lastReq.Header.Get("X-Subscription-Token") != "bk" || lastReq.URL.Query().Get("q") != "golang" || lastReq.URL.Query().Get("count") != "3" {
		t.Fatalf("brave request: %v %v", lastReq.Header, lastReq.URL)
	}
	check("tavily", &Searcher{Provider: "tavily", Key: "tk", URL: srv.URL + "/tavily"}, 4, "https://go.dev", "Build simple software")
	if lastReq.Header.Get("Authorization") != "Bearer tk" || lastBody["query"] != "golang" || lastBody["max_results"] != float64(4) {
		t.Fatalf("tavily request: %v %v", lastReq.Header, lastBody)
	}
	check("exa", &Searcher{Provider: "exa", Key: "ek", URL: srv.URL + "/exa"}, 2, "https://go.dev", "2026-01-02: Go text")
	if lastReq.Header.Get("x-api-key") != "ek" || lastBody["numResults"] != float64(2) {
		t.Fatalf("exa request: %v %v", lastReq.Header, lastBody)
	}
	check("searxng", &Searcher{Provider: "searxng", URL: srv.URL + "/searx"}, 2, "https://go.dev", "from searx")
	if lastReq.URL.Query().Get("format") != "json" {
		t.Fatalf("searxng request: %v", lastReq.URL)
	}
	res, _ := (&Searcher{Provider: "searxng", URL: srv.URL + "/searx/"}).Search(ctx, "golang", 2)
	if len(res) != 2 {
		t.Fatalf("searxng limit: %d", len(res))
	}
	check("duckduckgo", &Searcher{Provider: "duckduckgo", URL: srv.URL + "/ddg"}, 5, "https://go.dev/doc/", "Documentation & tutorials")
	if lastReq.Method != http.MethodPost {
		t.Fatalf("ddg method %s", lastReq.Method)
	}
	res, _ = (&Searcher{URL: srv.URL + "/ddg"}).Search(ctx, "golang", 5)
	if len(res) != 2 || res[0].Title != "The Go docs" || res[1].URL != "https://example.com/b" || res[1].Snippet != "two" {
		t.Fatalf("ddg results (ads skipped): %+v", res)
	}
}

func TestSearchErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := (&Searcher{Provider: "brave", KeyEnv: "BRAVE_API_KEY"}).Search(ctx, "x", 1); err == nil || !strings.Contains(err.Error(), "BRAVE_API_KEY") {
		t.Fatalf("missing key: %v", err)
	}
	if _, err := (&Searcher{Provider: "brave", Key: "k"}).Search(ctx, "  ", 1); err == nil {
		t.Fatal("empty query accepted")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ddg" {
			io.WriteString(w, "<html>Unfortunately, bots use DuckDuckGo too. anomaly</html>")
			return
		}
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()
	if _, err := (&Searcher{Provider: "searxng", URL: srv.URL}).Search(ctx, "x", 1); err == nil || !strings.Contains(err.Error(), "json format") {
		t.Fatalf("searxng 403: %v", err)
	}
	if _, err := (&Searcher{Provider: "duckduckgo", URL: srv.URL + "/ddg"}).Search(ctx, "x", 1); err == nil || !strings.Contains(err.Error(), "configure a search provider") {
		t.Fatalf("ddg block: %v", err)
	}
}

func TestWebSearchTool(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, _ := url.QueryUnescape(r.URL.Query().Get("q"))
		if q == "nothing" {
			io.WriteString(w, `{"web":{"results":[]}}`)
			return
		}
		io.WriteString(w, `{"web":{"results":[{"title":"One","url":"https://one","description":"first"},{"title":"","url":"https://two","description":""}]}}`)
	}))
	defer srv.Close()
	tool := webSearchTool{s: &Searcher{Provider: "brave", Key: "k", URL: srv.URL}}
	env := testEnv(t)
	res, _ := run(t, env, tool, `{"query":"habakkuk agent"}`)
	if res.IsError || !strings.Contains(res.Text, "1. One\n   https://one\n   first") || !strings.Contains(res.Text, "2. https://two\n   https://two") {
		t.Fatalf("result:\n%s", res.Text)
	}
	if res, _ = run(t, env, tool, `{"query":"nothing"}`); !strings.Contains(res.Text, "No results") {
		t.Fatalf("empty: %s", res.Text)
	}
}

func TestKitWiring(t *testing.T) {
	cfg, err := config.Parse([]byte(`version: 1
providers:
  claude: {type: anthropic, keys_env: [A]}
  router: {type: openai, keys_env: [B]}
agents:
  habakkuk: {model: claude/opus}
  other: {model: router/gpt, fallbacks: [claude/opus]}
search: {provider: tavily}
images: {provider: gemini}
tts: {provider: edge}
`))
	if err != nil {
		t.Fatal(err)
	}
	if !HostedSearch(cfg, cfg.Agents["habakkuk"]) || HostedSearch(cfg, cfg.Agents["other"]) {
		t.Fatal("HostedSearch picks by the main model's provider")
	}
	k := New(cfg, func(name string) string {
		return map[string]string{"TAVILY_API_KEY": "tv", "GEMINI_API_KEY": "gm"}[name]
	})
	if k.Search.Provider != "tavily" || k.Search.Key != "tv" || k.Images.Key != "gm" || k.Speech.Provider != "edge" || k.Speech.Format != "opus" {
		t.Fatalf("kit: %+v %+v %+v", k.Search, k.Images, k.Speech)
	}
	cfg.Search = nil
	// No search provider is contacted unless one is configured.
	if k := New(cfg, func(string) string { return "" }); k.Search != nil {
		t.Fatalf("default search: %+v, want none", k.Search)
	}
}
