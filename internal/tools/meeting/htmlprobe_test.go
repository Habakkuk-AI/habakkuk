package meeting

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// A test-side copy of the page script's probe and caption extraction over
// parsed HTML, so fixture pages can be checked without a browser. It
// understands the compound selectors the specs use: tag, #id, .class and
// [attr], [attr="v"], [attr*="v"], [attr^="v"].

type simpleSelector struct {
	tag, id string
	classes []string
	attrs   []attrCond
}

type attrCond struct{ name, op, val string }

func parseSelector(t *testing.T, s string) simpleSelector {
	t.Helper()
	var sel simpleSelector
	rest := s
	if m := regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9-]*`).FindString(rest); m != "" {
		sel.tag, rest = m, rest[len(m):]
	}
	part := regexp.MustCompile(`^(?:#([\w-]+)|\.([\w-]+)|\[([\w-]+)(?:([*^]?=)"([^"]*)")?\])`)
	for rest != "" {
		m := part.FindStringSubmatch(rest)
		if m == nil {
			t.Fatalf("selector %q: cannot read %q (compound selectors only)", s, rest)
		}
		switch {
		case m[1] != "":
			sel.id = m[1]
		case m[2] != "":
			sel.classes = append(sel.classes, m[2])
		default:
			sel.attrs = append(sel.attrs, attrCond{m[3], m[4], m[5]})
		}
		rest = rest[len(m[0]):]
	}
	return sel
}

func attr(n *html.Node, name string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val, true
		}
	}
	return "", false
}

func (s simpleSelector) match(n *html.Node) bool {
	if n.Type != html.ElementNode || (s.tag != "" && n.Data != s.tag) {
		return false
	}
	if s.id != "" {
		if v, _ := attr(n, "id"); v != s.id {
			return false
		}
	}
	cls, _ := attr(n, "class")
	for _, c := range s.classes {
		if !strings.Contains(" "+cls+" ", " "+c+" ") {
			return false
		}
	}
	for _, a := range s.attrs {
		v, ok := attr(n, a.name)
		switch {
		case !ok:
			return false
		case a.op == "=" && v != a.val, a.op == "*=" && !strings.Contains(v, a.val), a.op == "^=" && !strings.HasPrefix(v, a.val):
			return false
		}
	}
	return true
}

func walk(n *html.Node, fn func(*html.Node) bool) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if fn(c) {
			walk(c, fn)
		}
	}
}

func queryAll(t *testing.T, root *html.Node, sel string) []*html.Node {
	s := parseSelector(t, sel)
	var out []*html.Node
	walk(root, func(n *html.Node) bool {
		if s.match(n) {
			out = append(out, n)
		}
		return true
	})
	return out
}

func matchesAny(t *testing.T, n *html.Node, sels []string) bool {
	for _, s := range sels {
		if parseSelector(t, s).match(n) {
			return true
		}
	}
	return false
}

// textOf is the text of a node, block elements on lines of their own.
func textOf(n *html.Node) string {
	var b strings.Builder
	var rec func(*html.Node)
	rec = func(n *html.Node) {
		switch n.Type {
		case html.TextNode:
			b.WriteString(n.Data)
			return
		case html.ElementNode:
			switch n.Data {
			case "script", "style":
				return
			case "div", "p", "h1", "h2", "li", "aside", "footer", "ul", "section":
				b.WriteString("\n")
				defer b.WriteString("\n")
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			rec(c)
		}
	}
	rec(n)
	return b.String()
}

func clickable(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	role, _ := attr(n, "role")
	typ, _ := attr(n, "type")
	_, href := attr(n, "href")
	switch {
	case n.Data == "button", n.Data == "a" && href:
		return true
	case n.Data == "input" && (typ == "button" || typ == "submit" || typ == "checkbox"):
		return true
	}
	switch role {
	case "button", "menuitem", "menuitemcheckbox", "menuitemradio", "switch", "tab", "option":
		return true
	}
	return false
}

func field(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	typ, hasType := attr(n, "type")
	ce, _ := attr(n, "contenteditable")
	role, _ := attr(n, "role")
	return (n.Data == "input" && (!hasType || typ == "text" || typ == "email" || typ == "search")) ||
		n.Data == "textarea" || ce == "true" || role == "textbox"
}

func probeHTML(t *testing.T, root *html.Node, p Platform) Probe {
	s := specFor(p)
	var pr Probe
	var text strings.Builder
	var rec func(*html.Node)
	rec = func(n *html.Node) {
		if n.Type == html.ElementNode && (matchesAny(t, n, s.skipSelectors()) || n.Data == "script" || n.Data == "style" || n.Data == "template") {
			return
		}
		if n.Type == html.TextNode {
			text.WriteString(n.Data + " ")
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			rec(c)
		}
	}
	rec(root)
	pr.Text = clean(text.String())
	walk(root, func(n *html.Node) bool {
		if clickable(n) {
			l, ok := attr(n, "aria-label")
			if !ok || l == "" {
				l = clean(textOf(n))
			}
			if l == "" {
				l, _ = attr(n, "value")
			}
			l = clean(l)
			st, ok := attr(n, "aria-pressed")
			if !ok {
				st, ok = attr(n, "aria-checked")
			}
			if ok && st == "true" {
				l += " [on]"
			} else if ok && st == "false" {
				l += " [off]"
			}
			if l != "" {
				pr.Buttons = append(pr.Buttons, l)
			}
		}
		if field(n) {
			var parts []string
			for _, k := range []string{"aria-label", "placeholder", "aria-placeholder", "id", "name", "data-tid"} {
				if v, ok := attr(n, k); ok && v != "" {
					parts = append(parts, clean(v))
				}
			}
			pr.Fields = append(pr.Fields, strings.Join(parts, " | "))
		}
		return true
	})
	for _, sel := range s.captions.Containers {
		if len(queryAll(t, root, sel)) > 0 {
			pr.Captions = true
			break
		}
	}
	return pr
}

// captionsHTML is the caption extraction of the page script.
func captionsHTML(t *testing.T, root *html.Node, p Platform) [][2]string {
	cs := specFor(p).captions
	var containers []*html.Node
	for _, sel := range cs.Containers {
		if containers = queryAll(t, root, sel); len(containers) > 0 {
			break
		}
	}
	pick := func(n *html.Node, sels []string) string {
		for _, s := range sels {
			for _, f := range queryAll(t, n, s) {
				if v := clean(textOf(f)); v != "" {
					return v
				}
			}
		}
		return ""
	}
	var out [][2]string
	for _, c := range containers {
		var items []*html.Node
		for _, s := range cs.Items {
			if items = queryAll(t, c, s); len(items) > 0 {
				break
			}
		}
		if len(items) == 0 {
			for k := c.FirstChild; k != nil; k = k.NextSibling {
				if k.Type == html.ElementNode {
					items = append(items, k)
				}
			}
		}
		for _, it := range items {
			speaker, text := pick(it, cs.Speaker), pick(it, cs.Text)
			if text == "" {
				var lines []string
				for _, l := range strings.Split(textOf(it), "\n") {
					if l = clean(l); l != "" {
						lines = append(lines, l)
					}
				}
				if speaker == "" && len(lines) >= 2 && len(lines[0]) <= 50 {
					speaker, text = lines[0], strings.Join(lines[1:], " ")
				} else {
					text = strings.Join(lines, " ")
				}
			}
			out = append(out, [2]string{speaker, text})
		}
	}
	return out
}

// fixtureSite serves testdata: the platform pages and the simulator.
func fixtureSite(t *testing.T, mux *http.ServeMux) *httptest.Server {
	t.Helper()
	if mux == nil {
		mux = http.NewServeMux()
	}
	for _, p := range []string{"meet", "zoom", "teams"} {
		mux.HandleFunc("/"+p, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			http.ServeFile(w, r, "testdata/"+p+".html")
		})
	}
	mux.HandleFunc("/sim.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		http.ServeFile(w, r, "testdata/sim.js")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// fetchTemplates downloads a fixture page and returns its stage templates,
// parsed.
func fetchTemplates(t *testing.T, url string) map[string]*html.Node {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*html.Node{}
	walk(doc, func(n *html.Node) bool {
		if n.Type == html.ElementNode && n.Data == "template" {
			id, _ := attr(n, "id")
			out[id] = n
			return false
		}
		return true
	})
	if len(out) == 0 {
		t.Fatalf("no templates in %s", url)
	}
	return out
}

// combine makes one page of several templates' contents.
func combine(ts ...*html.Node) *html.Node {
	root := &html.Node{Type: html.ElementNode, Data: "body"}
	for _, t := range ts {
		for c := t.FirstChild; c != nil; c = c.NextSibling {
			cp := *c
			cp.Parent, cp.PrevSibling, cp.NextSibling = nil, nil, nil
			root.AppendChild(clone(&cp))
		}
	}
	return root
}

func clone(n *html.Node) *html.Node {
	c := &html.Node{Type: n.Type, DataAtom: n.DataAtom, Data: n.Data, Namespace: n.Namespace, Attr: append([]html.Attribute(nil), n.Attr...)}
	for k := n.FirstChild; k != nil; k = k.NextSibling {
		c.AppendChild(clone(k))
	}
	return c
}
