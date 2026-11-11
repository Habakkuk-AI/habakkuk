package migrate

import (
	"reflect"
	"strings"
	"testing"
)

func TestDecodeTOML(t *testing.T) {
	src := `# top comment
model = "gpt-5.6-sol" # trailing comment
count = 1_000
ratio = 0.5
on = true
"quoted key" = 'C:\path\as\is'
when = 2026-11-09T12:00:00Z
list = [
  "a", # a comment inside
  'b',
]
inline = { x = 1, "y z" = "w", nested = { k = "v" } }
site.name = "Corner Bakery"
multi = """
first
second \
  joined"""
raw = '''
keep \n as is'''
esc = "tab\there \u00e9 \"q\""

[mcp_servers.docs]
command = "npx"
args = ["-y", "docs"]
env = { DOCS_TOKEN = "x" }

[mcp_servers."with.dot"]
url = "https://example.invalid"

[[hooks]]
name = "one"

[[hooks]]
name = "two"

[hooks.extra]
k = 1
`
	got, err := decodeTOML([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"model":      "gpt-5.6-sol",
		"count":      int64(1000),
		"ratio":      0.5,
		"on":         true,
		"quoted key": `C:\path\as\is`,
		"when":       "2026-11-09T12:00:00Z",
		"list":       []any{"a", "b"},
		"inline":     map[string]any{"x": int64(1), "y z": "w", "nested": map[string]any{"k": "v"}},
		"site":       map[string]any{"name": "Corner Bakery"},
		"multi":      "first\nsecond joined",
		"raw":        `keep \n as is`,
		"esc":        "tab\there é \"q\"",
		"mcp_servers": map[string]any{
			"docs":     map[string]any{"command": "npx", "args": []any{"-y", "docs"}, "env": map[string]any{"DOCS_TOKEN": "x"}},
			"with.dot": map[string]any{"url": "https://example.invalid"},
		},
		"hooks": []any{
			map[string]any{"name": "one"},
			map[string]any{"name": "two", "extra": map[string]any{"k": int64(1)}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %#v\nwant %#v", got, want)
	}
}

func TestDecodeTOMLErrors(t *testing.T) {
	for _, src := range []string{
		`a = "unterminated`,
		`a = `,
		`[table`,
		"a = 1\na.b = 2",
		`a = [1, 2`,
		`a = { b = 1`,
		`a = nonsense`,
	} {
		if _, err := decodeTOML([]byte(src)); err == nil {
			t.Errorf("%q: no error", src)
		} else if !strings.HasPrefix(err.Error(), "line ") {
			t.Errorf("%q: error %q does not say the line", src, err)
		}
	}
}
