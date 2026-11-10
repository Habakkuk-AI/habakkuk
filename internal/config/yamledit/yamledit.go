// Package yamledit edits config.yaml as a YAML node tree, so a change
// touches only what was asked and keeps the rest of the file, comments
// included. habakkuk setup and habakkuk migrate both use it.
package yamledit

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Parse reads a config document; it must be a mapping. Empty input gives a
// new document holding an empty mapping.
func Parse(data []byte) (*yaml.Node, error) {
	var doc yaml.Node
	if len(bytes.TrimSpace(data)) == 0 {
		return &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}, nil
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("config.yaml: %w", err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("config.yaml is not a mapping")
	}
	return &doc, nil
}

// Encode renders a document with two-space indents and a blank line before
// each top-level section.
func Encode(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return SpaceSections(buf.Bytes()), nil
}

// SpaceSections puts a blank line before each top-level section, which
// the YAML encoder drops.
func SpaceSections(data []byte) []byte {
	lines := strings.Split(string(data), "\n")
	var out []string
	for i, l := range lines {
		// A top-level key or comment that follows anything but a blank line
		// or a top-level comment (which belongs to it) starts a section.
		if top := l != "" && l[0] != ' ' && l[0] != '-'; top && i > 0 {
			if prev := lines[i-1]; prev != "" && !strings.HasPrefix(prev, "#") {
				out = append(out, "")
			}
		}
		out = append(out, l)
	}
	return []byte(strings.Join(out, "\n"))
}

// Str is a string scalar, quoted when a plain one would read as something
// else.
func Str(v string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
	if NeedsQuotes(v) {
		n.Style = yaml.DoubleQuotedStyle
	}
	return n
}

// NeedsQuotes reports whether a plain scalar would read as something else.
func NeedsQuotes(v string) bool {
	var probe any
	if err := yaml.Unmarshal([]byte(v), &probe); err != nil {
		return true
	}
	s, ok := probe.(string)
	return !ok || s != v
}

// Int is an integer scalar.
func Int(v int) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(v)}
}

// Bool is a boolean scalar.
func Bool(v bool) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(v)}
}

// FlowList is a one-line list of strings: [a, b].
func FlowList(vals ...string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle}
	for _, v := range vals {
		n.Content = append(n.Content, Str(v))
	}
	return n
}

// FlowMap is a one-line mapping of strings: {k: v}, keys in the order given.
func FlowMap(keys []string, vals map[string]string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Style: yaml.FlowStyle}
	for _, k := range keys {
		n.Content = append(n.Content, Str(k), Str(vals[k]))
	}
	return n
}

// Get returns the value under key in a mapping, or nil.
func Get(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// Set sets key to val in a mapping, replacing the old value (and keeping
// the key's comments) or appending it. comment, when set, becomes the
// value's line comment.
func Set(m *yaml.Node, key string, val *yaml.Node, comment string) {
	if comment != "" {
		val.LineComment = comment
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			old := m.Content[i+1]
			if comment == "" && old.Kind == yaml.ScalarNode && val.Kind == yaml.ScalarNode {
				val.LineComment = old.LineComment
			}
			m.Content[i+1] = val
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, val)
}

// EnsureMap returns the mapping under key, adding it (with head, a comment
// above it) when missing. A null value (a bare "channels:") becomes a map.
func EnsureMap(root *yaml.Node, key, head string) *yaml.Node {
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			v := root.Content[i+1]
			if v.Kind != yaml.MappingNode {
				*v = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", HeadComment: v.HeadComment, LineComment: v.LineComment}
			}
			v.Style = 0
			return v
		}
	}
	v := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key, HeadComment: head}, v)
	return v
}
