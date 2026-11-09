// Package nodes lets other machines lend Habakkuk capabilities. A device (a
// laptop, a Raspberry Pi, a phone running the dashboard) pairs once with a
// one-time code, which gives it a per-node secret, then holds a connection
// to the server: a websocket at /nodes for `habakkuk node run`, or a
// Server-Sent Events stream plus result posts for the browser. The server
// sends invocations ("take a screenshot") down the connection and the node
// answers with text and files. Agents reach nodes through nodes_list and
// node_invoke, and every invocation is approval-gated per node and command
// unless config nodes.allow lets it run.
package nodes

import (
	"encoding/json"
	"regexp"
)

// Commands a node can offer.
const (
	CmdRun         = "system.run"      // run a shell command (off unless the node allows it)
	CmdNotify      = "system.notify"   // show a desktop notification
	CmdScreen      = "screen.snapshot" // capture the screen
	CmdCamera      = "camera.snap"     // take a photo (off unless the node allows it)
	CmdLocation    = "location.get"    // where the device is, best effort
	CmdClipRead    = "clipboard.read"  // the clipboard's text
	CmdClipWrite   = "clipboard.write" // replace the clipboard's text
	CmdFilesGet    = "files.get"       // read a file inside the node's allowed directory
	CmdFilesPut    = "files.put"       // write a file inside the node's allowed directory
	commandPattern = `^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`
)

// Limits on what travels over a node connection.
const (
	MaxFile  = 25 << 20 // one file in a result or a files.put
	MaxFrame = 48 << 20 // one frame: base64 files plus their JSON
	MaxText  = 256 << 10
)

// Frame is one message on a node connection, in either direction.
//
//	node -> server  hello   {name, secret, caps, platform, version}
//	server -> node  welcome {} or error {error}, then the server closes
//	server -> node  invoke  {id, command, args}
//	node -> server  result  {id, result}
//	either way      ping / pong (the websocket also pings)
type Frame struct {
	Type string `json:"type"`

	Name     string   `json:"name,omitempty"`
	Secret   string   `json:"secret,omitempty"`
	Caps     []string `json:"caps,omitempty"`
	Platform string   `json:"platform,omitempty"`
	Version  string   `json:"version,omitempty"`

	ID      string          `json:"id,omitempty"`
	Command string          `json:"command,omitempty"`
	Args    json.RawMessage `json:"args,omitempty"`
	Result  *Result         `json:"result,omitempty"`
	Error   string          `json:"error,omitempty"`
}

// Result is what a node answers an invocation with.
type Result struct {
	Text  string `json:"text,omitempty"`
	Files []File `json:"files,omitempty"`
	// Error is set when the command failed on the node.
	Error string `json:"error,omitempty"`
}

// File is a file in a result, or the payload of files.put.
type File struct {
	Name      string `json:"name"`
	MediaType string `json:"media_type,omitempty"`
	Data      []byte `json:"data"` // base64 in JSON
}

var commandRE = regexp.MustCompile(commandPattern)
