package mcp

import (
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/config"
)

func TestCatalogEntriesAreValid(t *testing.T) {
	list, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) < 25 {
		t.Fatalf("only %d entries", len(list))
	}
	for _, want := range []string{"github", "gitlab", "linear", "notion", "slack", "google-drive", "stripe", "sentry",
		"supabase", "postgres", "sqlite", "filesystem", "fetch", "brave-search", "puppeteer", "playwright", "figma",
		"atlassian", "cloudflare", "vercel", "railway"} {
		if _, err := FindCatalog(want); err != nil {
			t.Errorf("missing %s", want)
		}
	}
	seen := map[string]bool{}
	for _, e := range list {
		if seen[e.Name] {
			t.Errorf("%s listed twice", e.Name)
		}
		seen[e.Name] = true
		if e.Description == "" || e.Title == "" {
			t.Errorf("%s: no title or description", e.Name)
		}
		params := map[string]string{}
		for k := range e.Params {
			params[k] = "/tmp/x"
		}
		s, err := e.Configure(params)
		if err != nil {
			t.Errorf("%s: %v", e.Name, err)
			continue
		}
		if strings.Contains(strings.Join(s.Args, " ")+s.URL, "{") {
			t.Errorf("%s: placeholder left: %v %s", e.Name, s.Args, s.URL)
		}
		// Every entry must make a config that loads.
		out, err := config.InsertMCPServer([]byte(config.Template), e.Name, s)
		if err != nil {
			t.Errorf("%s: %v", e.Name, err)
			continue
		}
		if _, err := config.Parse(out); err != nil {
			t.Errorf("%s: config does not load: %v", e.Name, err)
		}
	}
}

func TestCatalogConfigureAndSearch(t *testing.T) {
	e, err := FindCatalog("filesystem")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Configure(nil); err == nil || !strings.Contains(err.Error(), "dir=<") {
		t.Fatalf("missing param: %v", err)
	}
	if _, err := e.Configure(map[string]string{"dir": "/a", "x": "y"}); err == nil {
		t.Fatal("unknown param accepted")
	}
	s, err := e.Configure(map[string]string{"dir": "/home/me/notes"})
	if err != nil || s.Args[len(s.Args)-1] != "/home/me/notes" {
		t.Fatalf("%+v %v", s, err)
	}
	if e.Server.Args[len(e.Server.Args)-1] != "{dir}" {
		t.Fatal("Configure changed the catalog entry")
	}
	lin, _ := FindCatalog("linear")
	if v := lin.Vars(); len(v) != 1 || v[0] != "LINEAR_API_KEY" {
		t.Fatalf("vars %v", v)
	}
	hits, _ := SearchCatalog("search web")
	names := map[string]bool{}
	for _, h := range hits {
		names[h.Name] = true
	}
	if !names["brave-search"] || !names["exa"] || names["github"] {
		t.Fatalf("search %v", names)
	}
	if all, _ := SearchCatalog(""); len(all) < 25 {
		t.Fatal("empty query lists everything")
	}
}

func TestCatalogOAuthEntries(t *testing.T) {
	for _, name := range []string{"atlassian", "cloudflare", "vercel", "linear-oauth", "notion-hosted", "sentry-hosted"} {
		e, err := FindCatalog(name)
		if err != nil {
			t.Fatal(err)
		}
		s, err := e.Configure(nil)
		if err != nil || s.Auth != "oauth" || s.URL == "" || len(e.Vars()) != 0 {
			t.Fatalf("%s: %+v %v", name, s, err)
		}
		out, err := config.InsertMCPServer([]byte(config.Template), name, s)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Parse(out)
		if err != nil || cfg.MCP[name].Auth != "oauth" {
			t.Fatalf("%s: auth lost in config.yaml: %v", name, err)
		}
	}
}
