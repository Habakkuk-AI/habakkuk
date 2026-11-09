package frontmatter

import (
	"testing"
)

type meta struct {
	Title string   `yaml:"title"`
	Tags  []string `yaml:"tags"`
}

func TestParse(t *testing.T) {
	for _, c := range []struct {
		name, in, title, body string
		wantErr               bool
	}{
		{"plain", "---\ntitle: Hi\ntags: [a]\n---\n\nBody text\n", "Hi", "Body text", false},
		{"crlf and bom", "\xef\xbb\xbf---\r\ntitle: Hi\r\n---\r\nBody\r\n", "Hi", "Body", false},
		{"dots close", "---\ntitle: Hi\n...\nBody", "Hi", "Body", false},
		{"no frontmatter", "Just a note.\n", "", "Just a note.", false},
		{"unclosed", "---\ntitle: Hi\nBody", "", "---\ntitle: Hi\nBody", false},
		{"rule in body", "Intro\n---\nmore", "", "Intro\n---\nmore", false},
		{"bad yaml", "---\ntitle: [unclosed\n---\nBody", "", "---\ntitle: [unclosed\n---\nBody", true},
		{"unknown keys", "---\ntitle: Hi\nauthor: me\n---\nBody", "Hi", "Body", false},
		{"empty body", "---\ntitle: Hi\n---", "Hi", "", false},
	} {
		var m meta
		body, err := Parse([]byte(c.in), &m)
		if (err != nil) != c.wantErr || m.Title != c.title || body != c.body {
			t.Errorf("%s: title %q body %q err %v", c.name, m.Title, body, err)
		}
	}
}

func TestFormatRoundTrip(t *testing.T) {
	data, err := Format(meta{Title: "Hello: world", Tags: []string{"x"}}, "\n\nBody\n\n")
	if err != nil {
		t.Fatal(err)
	}
	var m meta
	body, err := Parse(data, &m)
	if err != nil || m.Title != "Hello: world" || len(m.Tags) != 1 || body != "Body" {
		t.Fatalf("round trip: %+v %q %v\n%s", m, body, err, data)
	}
}
