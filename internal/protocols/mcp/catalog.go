package mcp

import (
	"bytes"
	_ "embed"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/habakkuk-ai/habakkuk/internal/config"
)

//go:embed catalog.yaml
var catalogYAML []byte

// CatalogEntry is one well-known MCP server `hk mcp add` can configure.
type CatalogEntry struct {
	Name        string            `yaml:"name"`
	Title       string            `yaml:"title"`
	Description string            `yaml:"description"`
	Homepage    string            `yaml:"homepage"`
	Tags        []string          `yaml:"tags"`
	Needs       []string          `yaml:"needs"`  // programs that must be on PATH
	Params      map[string]string `yaml:"params"` // {name} placeholders in args or url -> what to give
	Server      config.MCPServer  `yaml:"server"`
	Notes       string            `yaml:"notes"`
}

// Catalog returns the built-in catalog, sorted by name.
func Catalog() ([]CatalogEntry, error) {
	dec := yaml.NewDecoder(bytes.NewReader(catalogYAML))
	dec.KnownFields(true)
	var list []CatalogEntry
	if err := dec.Decode(&list); err != nil {
		return nil, fmt.Errorf("mcp catalog: %w", err)
	}
	slices.SortFunc(list, func(a, b CatalogEntry) int { return strings.Compare(a.Name, b.Name) })
	return list, nil
}

// SearchCatalog returns the entries whose name, title, description or tags
// contain every word of query (all of them for an empty query).
func SearchCatalog(query string) ([]CatalogEntry, error) {
	list, err := Catalog()
	if err != nil {
		return nil, err
	}
	words := strings.Fields(strings.ToLower(query))
	var out []CatalogEntry
	for _, e := range list {
		text := strings.ToLower(strings.Join(append([]string{e.Name, e.Title, e.Description}, e.Tags...), " "))
		if !slices.ContainsFunc(words, func(w string) bool { return !strings.Contains(text, w) }) {
			out = append(out, e)
		}
	}
	return out, nil
}

// FindCatalog looks an entry up by name.
func FindCatalog(name string) (CatalogEntry, error) {
	list, err := Catalog()
	if err != nil {
		return CatalogEntry{}, err
	}
	for _, e := range list {
		if e.Name == name {
			return e, nil
		}
	}
	return CatalogEntry{}, fmt.Errorf("no %q in the MCP catalog (hk mcp catalog lists it)", name)
}

var reParam = regexp.MustCompile(`\{([a-z][a-z0-9_]*)\}`)

// Configure returns the server's config with its {param} placeholders
// filled from params; it names the ones missing or unknown.
func (e CatalogEntry) Configure(params map[string]string) (*config.MCPServer, error) {
	for k := range params {
		if _, ok := e.Params[k]; !ok {
			return nil, fmt.Errorf("%s takes no parameter %q", e.Name, k)
		}
	}
	var missing []string
	for _, k := range slices.Sorted(maps.Keys(e.Params)) {
		if params[k] == "" {
			missing = append(missing, fmt.Sprintf("%s=<%s>", k, e.Params[k]))
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%s needs %s", e.Name, strings.Join(missing, " "))
	}
	fill := func(s string) string {
		return reParam.ReplaceAllStringFunc(s, func(m string) string {
			if v, ok := params[m[1:len(m)-1]]; ok {
				return v
			}
			return m
		})
	}
	s := e.Server
	s.Args = slices.Clone(s.Args)
	for i := range s.Args {
		s.Args[i] = fill(s.Args[i])
	}
	s.URL = fill(s.URL)
	s.Env, s.Headers = maps.Clone(s.Env), maps.Clone(s.Headers)
	s.Approve, s.Allow, s.Scopes = slices.Clone(s.Approve), slices.Clone(s.Allow), slices.Clone(s.Scopes)
	return &s, nil
}

// Vars lists the variables the server needs set, sorted.
func (e CatalogEntry) Vars() []string {
	seen := map[string]bool{}
	for _, v := range e.Server.Env {
		seen[v] = true
	}
	for _, h := range e.Server.Headers {
		for _, v := range HeaderVars(h) {
			seen[v] = true
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// Launch is how the server runs, in one line: the command or the URL.
func (e CatalogEntry) Launch() string {
	if e.Server.URL != "" {
		return e.Server.URL
	}
	return strings.Join(append([]string{e.Server.Command}, e.Server.Args...), " ")
}
