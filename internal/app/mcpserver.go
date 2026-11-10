package app

import "github.com/habakkuk-ai/habakkuk/internal/protocols/mcpserver"

// MCPServer is Habakkuk as an MCP server, for `hk mcp serve` and /mcp.
func (a *App) MCPServer(version string) *mcpserver.Server {
	if version == "" {
		version = "dev"
	}
	s := &mcpserver.Server{
		Runner: a.Runner, Board: a.Board, Schedules: a.Schedules, Gateway: a.Gateway, Version: version, Log: a.Log,
	}
	if a.Dispatcher != nil {
		s.Wake = a.Dispatcher.Wake
	}
	if c := a.Cfg.MCPServer; c != nil {
		s.AllowSend = c.AllowSend
	}
	return s
}

// openMCPHTTP mounts the MCP server at /mcp when the config asks for it.
func (a *App) openMCPHTTP() {
	if a.Web != nil && a.Cfg.MCPServer != nil && a.Cfg.MCPServer.HTTP {
		a.Web.MCP = a.MCPServer("").Handler()
	}
}
