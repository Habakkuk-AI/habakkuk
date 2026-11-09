package config

import "errors"

// MCPServe configures Habakkuk as an MCP server: `habakkuk mcp serve` on stdio,
// and with HTTP on, the /mcp endpoint of the HTTP surface (same token).
type MCPServe struct {
	// HTTP serves Streamable HTTP at /mcp; needs the http section.
	HTTP bool `yaml:"http"`
	// AllowSend exposes habakkuk_send, which posts a message to any configured
	// channel conversation. Off by default: it lets a client message people.
	AllowSend bool `yaml:"allow_send"`
}

func (c *Config) validateMCPServe() []error {
	if c.MCPServer != nil && c.MCPServer.HTTP && c.HTTP == nil {
		return []error{errors.New("mcp_server: http needs the http section")}
	}
	return nil
}
