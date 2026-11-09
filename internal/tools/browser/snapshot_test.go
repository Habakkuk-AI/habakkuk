package browser

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// ax builds an accessibility node; props are name=value pairs.
func ax(id, role, name string, backend int64, children []string, props ...string) axNode {
	n := axNode{NodeID: id, BackendID: backend, ChildIDs: children}
	str := func(s string) *axValue { b, _ := json.Marshal(s); return &axValue{Type: "string", Value: b} }
	n.Role = str(role)
	if name != "" {
		n.Name = str(name)
	}
	for _, p := range props {
		k, v, _ := strings.Cut(p, "=")
		if k == "value" {
			n.Value = str(v)
			continue
		}
		raw := json.RawMessage(v)
		if !json.Valid(raw) {
			raw, _ = json.Marshal(v)
		}
		n.Properties = append(n.Properties, axProp{Name: k, Value: axValue{Value: raw}})
	}
	return n
}

func withParents(nodes []axNode) []axNode {
	parent := map[string]string{}
	for _, n := range nodes {
		for _, c := range n.ChildIDs {
			parent[c] = n.NodeID
		}
	}
	for i := range nodes {
		nodes[i].ParentID = parent[nodes[i].NodeID]
	}
	return nodes
}

func page() []axNode {
	return withParents([]axNode{
		ax("1", "RootWebArea", "Shop", 1, []string{"2", "3", "20", "30", "40"}),
		ax("2", "heading", "Checkout", 2, []string{"2a"}, "level=1"),
		ax("2a", "StaticText", "Checkout", 0, nil),
		ax("3", "generic", "", 3, []string{"4", "5", "6", "7", "8", "9"}),
		ax("4", "StaticText", "Pay   with\ncard", 0, nil),
		ax("5", "StaticText", "|", 0, nil),
		ax("6", "textbox", "Card number", 6, nil, "value=4242", "required=true", "focused=true"),
		ax("7", "combobox", "Country", 7, []string{"7a"}, "value=Canada", "expanded=false"),
		ax("7a", "MenuListPopup", "", 0, []string{"7b", "7c"}),
		ax("7b", "option", "Canada", 71, nil, "selected=true"),
		ax("7c", "option", "Mexico", 72, nil),
		ax("8", "checkbox", "Save card", 8, nil, "checked=true"),
		ax("9", "button", "Pay now", 9, []string{"9a"}, "disabled=true"),
		ax("9a", "StaticText", "Pay now", 0, nil),
		ax("20", "list", "", 20, []string{"21", "22"}),
		ax("21", "listitem", "", 21, []string{"21a"}),
		ax("21a", "link", "Terms", 211, nil, "url=https://shop.example/terms"),
		ax("22", "listitem", "", 22, []string{"22a"}),
		ax("22a", "link", "Help", 221, nil, "url=https://help.example/"),
		ax("30", "table", "", 30, []string{"31", "32"}),
		ax("31", "row", "", 31, nil), // an empty spacer row
		ax("32", "row", "", 32, []string{"32a"}),
		ax("32a", "cell", "", 0, []string{"32b"}),
		ax("32b", "StaticText", "Total $12", 0, nil),
		{NodeID: "40", Ignored: true, ChildIDs: []string{"41"}},
		ax("41", "Iframe", "", 41, nil),
	})
}

func frame() []axNode {
	return withParents([]axNode{
		ax("f1", "RootWebArea", "Card", 500, []string{"f2"}),
		ax("f2", "textbox", "CVC", 501, nil),
	})
}

func TestRender(t *testing.T) {
	refs := newRefTable()
	got := render(page(), map[int64][]axNode{41: frame()}, refs, renderOptions{pageURL: "https://shop.example/checkout"})
	want := `- heading "Checkout" [level=1]
- text "Pay with card"
- textbox "Card number" [ref=1] value="4242" required focused
- combobox "Country" [ref=2] value="Canada" options: "Canada", "Mexico"
- checkbox "Save card" [ref=3] checked
- button "Pay now" [ref=4] disabled
- list
  - listitem
    - link "Terms" [ref=5] url=/terms
  - listitem
    - link "Help" [ref=6] url=https://help.example/
- table
  - row
    - text "Total $12"
- iframe
  - textbox "CVC" [ref=7]
`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if info, ok := refs.lookup(1); !ok || info.node != 6 || info.role != "textbox" || info.name != "Card number" {
		t.Fatalf("ref 1 = %+v", info)
	}
	if info, _ := refs.lookup(7); info.node != 501 {
		t.Fatalf("frame ref = %+v", info)
	}

	// The same nodes keep their refs; a navigation forgets them, and new
	// elements never reuse an old number.
	if again := render(page(), map[int64][]axNode{41: frame()}, refs, renderOptions{}); !strings.Contains(again, `textbox "Card number" [ref=1]`) {
		t.Fatalf("refs moved:\n%s", again)
	}
	refs.reset()
	if _, ok := refs.lookup(1); ok {
		t.Fatal("ref survived a reset")
	}
	if after := render(page(), nil, refs, renderOptions{}); !strings.Contains(after, `textbox "Card number" [ref=8]`) {
		t.Fatalf("numbering restarted after reset:\n%s", after)
	}
}

func TestRenderInteractiveOnly(t *testing.T) {
	got := render(page(), nil, newRefTable(), renderOptions{interactiveOnly: true})
	for _, missing := range []string{"text ", "list", "table", "Total"} {
		if strings.Contains(got, missing) {
			t.Fatalf("interactive_only kept %q:\n%s", missing, got)
		}
	}
	for _, kept := range []string{`heading "Checkout"`, `[ref=1]`, `link "Help" [ref=6]`} {
		if !strings.Contains(got, kept) {
			t.Fatalf("interactive_only dropped %q:\n%s", kept, got)
		}
	}
}

func TestRenderCutAndOffset(t *testing.T) {
	var nodes []axNode
	var kids []string
	for i := range 100 {
		id := fmt.Sprint("b", i)
		kids = append(kids, id)
		nodes = append(nodes, ax(id, "button", fmt.Sprintf("Button %02d", i), int64(100+i), nil))
	}
	nodes = withParents(append([]axNode{ax("root", "RootWebArea", "", 1, kids)}, nodes...))
	refs := newRefTable()
	first := render(nodes, nil, refs, renderOptions{maxChars: 1000})
	if len(first) > 1200 || !strings.Contains(first, "browser_snapshot with offset") {
		t.Fatalf("not cut (%d chars):\n%s", len(first), first)
	}
	var end int
	fmt.Sscanf(first[strings.Index(first, "[snapshot cut after line "):], "[snapshot cut after line %d", &end)
	rest := render(nodes, nil, refs, renderOptions{maxChars: 100000, offset: end})
	if !strings.HasPrefix(rest, fmt.Sprintf("[lines %d to 100 of 100]", end+1)) ||
		!strings.Contains(rest, fmt.Sprintf(`"Button %02d" [ref=%d]`, end, end+1)) || !strings.Contains(rest, `"Button 99" [ref=100]`) {
		t.Fatalf("offset %d:\n%s", end, rest)
	}
	if strings.Contains(rest, `"Button 00"`) {
		t.Fatalf("offset repeated earlier lines:\n%s", rest)
	}
	if got := render(nodes, nil, refs, renderOptions{offset: 500}); !strings.Contains(got, "only 100 lines") {
		t.Fatalf("offset past the end: %s", got)
	}
}

func TestRenderEmpty(t *testing.T) {
	nodes := withParents([]axNode{ax("1", "RootWebArea", "", 1, nil)})
	if got := render(nodes, nil, newRefTable(), renderOptions{}); !strings.Contains(got, "no visible content") {
		t.Fatalf("got %q", got)
	}
}

func TestParseKey(t *testing.T) {
	cases := []struct {
		in        string
		key, code string
		vk, mods  int
		text      string
		err       bool
	}{
		{in: "Enter", key: "Enter", code: "Enter", vk: 13, text: "\r"},
		{in: "tab", key: "Tab", code: "Tab", vk: 9},
		{in: "a", key: "a", code: "KeyA", vk: 65, text: "a"},
		{in: "Shift+a", key: "A", code: "KeyA", vk: 65, mods: 8, text: "A"},
		{in: "Control+a", key: "a", code: "KeyA", vk: 65, mods: 2},
		{in: "Meta+Shift+z", key: "Z", code: "KeyZ", vk: 90, mods: 12},
		{in: "7", key: "7", code: "Digit7", vk: 55, text: "7"},
		{in: "F5", key: "F5", code: "F5", vk: 116},
		{in: "Control++", key: "+", mods: 2},
		{in: "é", key: "é", text: "é"},
		{in: "Hyper+a", err: true},
		{in: "Enterr", err: true},
	}
	for _, c := range cases {
		k, mods, err := parseKey(c.in)
		if (err != nil) != c.err {
			t.Fatalf("%q: err %v", c.in, err)
		}
		if c.err {
			continue
		}
		if k.key != c.key || k.code != c.code || k.vk != c.vk || mods != c.mods || k.text != c.text {
			t.Errorf("%q = %+v mods %d, want key %q code %q vk %d mods %d text %q", c.in, k, mods, c.key, c.code, c.vk, c.mods, c.text)
		}
	}
}

func TestSafeName(t *testing.T) {
	for in, want := range map[string]string{
		"report.pdf":        "report.pdf",
		"../../etc/passwd":  "passwd",
		".bashrc":           "download.bashrc",
		"":                  "download",
		"a/b\\c:d*e?.txt":   "b_c_d_e_.txt",
		"weird\x00name.csv": "weird_name.csv",
	} {
		if got := safeName(in); got != want {
			t.Errorf("safeName(%q) = %q, want %q", in, got, want)
		}
	}
}
