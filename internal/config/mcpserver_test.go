package config

import (
	"strings"
	"testing"
)

const mcpServeBase = `version: 1
providers:
  anthropic: {type: anthropic, keys_env: [K]}
agents:
  habakkuk: {model: anthropic/m}
`

func TestMCPServe(t *testing.T) {
	cfg, err := Parse([]byte(mcpServeBase + "http: {}\nmcp_server: {http: true, allow_send: true}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.MCPServer.HTTP || !cfg.MCPServer.AllowSend {
		t.Fatalf("mcp_server %+v", cfg.MCPServer)
	}
	_, err = Parse([]byte(mcpServeBase + "mcp_server: {http: true}\n"))
	if err == nil || !strings.Contains(err.Error(), "needs the http section") {
		t.Fatalf("http without the http section: %v", err)
	}
	_, err = Parse([]byte(mcpServeBase + "mcp_server: {alow_send: true}\n"))
	if err == nil {
		t.Fatal("misspelled key accepted")
	}
}
