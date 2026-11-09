package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"gopkg.in/yaml.v3"
)

// AddMCPServer adds a server under mcp_servers in home/config.yaml. The
// rest of the file (comments, order, spacing) is left as it is. The edited
// file must load; the previous one is kept as config.yaml.bak.
func AddMCPServer(home, name string, s *MCPServer) error {
	path := filepath.Join(home, "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	out, err := InsertMCPServer(data, name, s)
	if err != nil {
		return err
	}
	if _, err := ParseHome(out, home); err != nil {
		return fmt.Errorf("config.yaml would not load with %s added: %w", name, err)
	}
	if err := store.WriteFileAtomic(path+".bak", data, 0o600); err != nil {
		return err
	}
	return store.WriteFileAtomic(path, out, 0o600)
}

var (
	reMCPBlock  = regexp.MustCompile(`^mcp_servers:\s*(#.*)?$`)
	reMCPInline = regexp.MustCompile(`^mcp_servers:\s*\S`)
	reIndent    = regexp.MustCompile(`^( +)\S`)
)

// InsertMCPServer returns data with the server added. A block-style
// mcp_servers section (or none) is edited as text, so nothing else in the
// file moves; a flow-style one (mcp_servers: {}) is rewritten through the
// YAML node tree, which keeps comments but may reformat that file.
func InsertMCPServer(data []byte, name string, s *MCPServer) ([]byte, error) {
	if !mcpServerName.MatchString(name) {
		return nil, fmt.Errorf("mcp server name %q must be 1-32 letters, digits, _ or -", name)
	}
	var probe struct {
		MCP map[string]any `yaml:"mcp_servers"`
	}
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("config.yaml: %w", err)
	}
	if _, ok := probe.MCP[name]; ok {
		return nil, fmt.Errorf("mcp_servers already has %s", name)
	}
	lines := strings.Split(string(data), "\n")
	header := -1
	for i, l := range lines {
		if reMCPBlock.MatchString(l) {
			header = i
			break
		}
		if reMCPInline.MatchString(l) {
			return insertNode(data, name, s)
		}
	}
	if header < 0 {
		entry, err := renderServer(name, s, 2)
		if err != nil {
			return nil, err
		}
		out := strings.TrimRight(string(data), "\n") + "\n\nmcp_servers:\n" + entry
		return []byte(out), nil
	}
	indent, last := 0, header
block:
	for j := header + 1; j < len(lines); j++ {
		l := lines[j]
		switch {
		case strings.TrimSpace(l) == "":
			continue
		case !strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "\t"):
			if strings.HasPrefix(l, "#") {
				continue // a comment at the margin: could belong to the next section
			}
			break block // the next top-level key
		}
		last = j
		if m := reIndent.FindStringSubmatch(l); indent == 0 && m != nil && !strings.HasPrefix(strings.TrimSpace(l), "#") {
			indent = len(m[1])
		}
	}
	if indent == 0 {
		indent = 2
	}
	entry, err := renderServer(name, s, indent)
	if err != nil {
		return nil, err
	}
	out := slices.Insert(lines, last+1, strings.Split(strings.TrimRight(entry, "\n"), "\n")...)
	return []byte(strings.Join(out, "\n")), nil
}

// serverYAML is MCPServer with empty fields left out.
type serverYAML struct {
	Command string            `yaml:"command,omitempty"`
	Args    []string          `yaml:"args,omitempty,flow"`
	Env     map[string]string `yaml:"env,omitempty"`
	URL     string            `yaml:"url,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty"`
	Risk    string            `yaml:"risk,omitempty"`
	Approve []string          `yaml:"approve,omitempty,flow"`
	Allow   []string          `yaml:"allow,omitempty,flow"`
	Auth    string            `yaml:"auth,omitempty"`
	Scopes  []string          `yaml:"scopes,omitempty,flow"`
}

func toYAML(s *MCPServer) serverYAML {
	return serverYAML{Command: s.Command, Args: s.Args, Env: s.Env, URL: s.URL, Headers: s.Headers,
		Risk: s.Risk, Approve: s.Approve, Allow: s.Allow, Auth: s.Auth, Scopes: s.Scopes}
}

// renderServer renders "name:" and its fields, the name indented once.
func renderServer(name string, s *MCPServer, indent int) (string, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(indent)
	if err := enc.Encode(map[string]serverYAML{name: toYAML(s)}); err != nil {
		return "", err
	}
	pad := strings.Repeat(" ", indent)
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		b.WriteString(pad + l + "\n")
	}
	return b.String(), nil
}

func insertNode(data []byte, name string, s *MCPServer) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("config.yaml is not a mapping")
	}
	root := doc.Content[0]
	var entry yaml.Node
	if err := entry.Encode(toYAML(s)); err != nil {
		return nil, err
	}
	key := &yaml.Node{Kind: yaml.ScalarNode, Value: name}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "mcp_servers" {
			continue
		}
		v := root.Content[i+1]
		if v.Kind != yaml.MappingNode {
			*v = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		}
		v.Style = 0 // block style, so the new entry reads like the rest
		v.Content = append(v.Content, key, &entry)
		var buf bytes.Buffer
		enc := yaml.NewEncoder(&buf)
		enc.SetIndent(2)
		if err := enc.Encode(&doc); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	return nil, errors.New("mcp_servers not found")
}
