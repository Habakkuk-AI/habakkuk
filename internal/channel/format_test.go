package channel

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFormatSlack(t *testing.T) {
	in := "# Plan\n**Bold** and *it* and ~~old~~, see [docs](https://x.io/a?b=1&c=2).\n- one\n- two <tag>\n`a*b*c` stays\n```go\nx := **y**\n```\n"
	got := FormatSlack(in)
	for _, want := range []string{"*Plan*", "*Bold* and _it_ and ~old~", "<https://x.io/a?b=1&amp;c=2|docs>", "• one", "two &lt;tag&gt;", "`a*b*c` stays", "```\nx := **y**\n```"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatWhatsApp(t *testing.T) {
	got := FormatWhatsApp("## Hi\n**bold** *it* [site](https://a.b) [https://c.d](https://c.d)")
	for _, want := range []string{"*Hi*", "*bold* _it_", "site (https://a.b)", " https://c.d"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

func TestFormatTelegram(t *testing.T) {
	got := FormatTelegram("**a & b** <x> _it_ snake_case_name [l](https://e.f?a=1&b=2)\n```py\nif a < b: pass\n```\n`<c>`")
	for _, want := range []string{"<b>a &amp; b</b>", "&lt;x&gt;", "<i>it</i>", "snake_case_name",
		`<a href="https://e.f?a=1&amp;b=2">l</a>`, `<pre><code class="language-py">if a &lt; b: pass</code></pre>`, "<code>&lt;c&gt;</code>"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestSplitRespectsLimitAndFences(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 40; i++ {
		b.WriteString("Paragraph text that goes on for a while to fill space.\n\n")
	}
	b.WriteString("```\n")
	for i := 0; i < 60; i++ {
		b.WriteString("code line number something something\n")
	}
	b.WriteString("```\nend\n")
	b.WriteString(strings.Repeat("x", 900)) // one line longer than the limit
	chunks := Split(b.String(), 500)
	if len(chunks) < 6 {
		t.Fatalf("got %d chunks", len(chunks))
	}
	for i, c := range chunks {
		if n := utf8.RuneCountInString(c); n > 500 {
			t.Fatalf("chunk %d has %d runes", i, n)
		}
		if strings.Count(c, "```")%2 != 0 {
			t.Fatalf("chunk %d has an unbalanced fence:\n%s", i, c)
		}
	}
	joined := strings.Join(chunks, "\n")
	if strings.Count(joined, "code line number") != 60 || !strings.Contains(joined, "end") {
		t.Fatal("content lost in splitting")
	}
	if got := Split("short", 100); len(got) != 1 || got[0] != "short" {
		t.Fatalf("short text: %q", got)
	}
}
