package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
	"github.com/habakkuk-ai/habakkuk/internal/tools/media"
	"github.com/habakkuk-ai/habakkuk/internal/tools/mediatools"
)

// RegisterTools adds nodes_list and node_invoke to the core toolset. cfg
// is the nodes section (its allow list and timeout).
func RegisterTools(r *tools.Registry, h *Hub, cfg *config.Nodes) {
	r.Register("core", listTool{h: h, cfg: cfg})
	r.Register("core", invokeTool{h: h, cfg: cfg})
}

type listTool struct {
	h   *Hub
	cfg *config.Nodes
}

func (listTool) Risk() tools.Risk { return tools.RiskRead }
func (listTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "nodes_list",
		Description: `List the devices (nodes) paired with Habakkuk: laptops, servers, phones running the dashboard. For each: whether it is connected, its platform, the commands it offers now, and which of them run without asking the user. Use node_invoke to run one.`,
		Schema:      json.RawMessage(`{"type":"object","properties":{}}`),
	}
}

func (t listTool) Run(ctx context.Context, _ *tools.Env, _ json.RawMessage) (string, error) {
	nodes, err := t.h.List(ctx)
	if err != nil {
		return "", err
	}
	if len(nodes) == 0 {
		return "No nodes are paired. The user can pair one with `hk node pair` on the server, or from the dashboard's Nodes page.", nil
	}
	var b strings.Builder
	for _, n := range nodes {
		state := "offline"
		if n.Online {
			state = "online"
		} else if !n.LastSeen.IsZero() {
			state = "offline, last seen " + n.LastSeen.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(&b, "- %s (%s, %s)", n.Name, orDash(n.Platform), state)
		if len(n.Caps) > 0 {
			b.WriteString(": " + strings.Join(n.Caps, ", "))
			var free []string
			for _, c := range n.Caps {
				if t.cfg.Allowed(n.Name, c) {
					free = append(free, c)
				}
			}
			if len(free) > 0 {
				b.WriteString(" [run without asking: " + strings.Join(free, ", ") + "]")
			}
		}
		b.WriteString("\n")
	}
	b.WriteString("Commands not listed as running without asking need the user's approval each time.")
	return b.String(), nil
}

func orDash(s string) string {
	if s == "" {
		return "unknown platform"
	}
	return s
}

type invokeTool struct {
	h   *Hub
	cfg *config.Nodes
}

// Risk is read for the registry: node_invoke does its own gating, per node
// and command, in every permission mode (see Run), so the registry must
// not ask a second time.
func (invokeTool) Risk() tools.Risk { return tools.RiskRead }

// ExternalContent: what a device returns (clipboard text, files) comes from
// outside.
func (invokeTool) ExternalContent() {}

// Capability puts device commands under the turn's policy: running a
// command is the shell, and the screen, camera and clipboard are the
// computer, so guests never get them and channel turns ask first.
func (invokeTool) Capability(_ context.Context, _ *tools.Env, input json.RawMessage) tools.Capability {
	var in invokeInput
	if json.Unmarshal(input, &in) != nil {
		return ""
	}
	switch {
	case in.Command == "system.run":
		return tools.CapExec
	case in.Command == "files.put":
		return tools.CapWrite
	case strings.HasPrefix(in.Command, "screen."), strings.HasPrefix(in.Command, "camera."), strings.HasPrefix(in.Command, "clipboard."):
		return tools.CapComputer
	}
	return ""
}

func (invokeTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "node_invoke",
		Description: `Run a command on a paired device (see nodes_list). Unless the owner allowed that command on that node in config, the user is asked to approve every call; with nobody to ask it is denied. Commands and their args:
- system.notify {title?, message}: show a notification on the device
- screen.snapshot {}: capture the screen; the image is shown to you and saved in your workspace
- camera.snap {device?}: take a photo, shown to you and saved
- location.get {}: where the device is (best effort)
- clipboard.read {} / clipboard.write {text}
- files.get {path}: copy a file from the node's shared directory into your workspace (a directory path lists it)
- files.put {path, from | text}: write a file into the node's shared directory, from a workspace file ("from") or text
- system.run {command, cwd?, timeout_seconds?}: run a shell command on the device (only nodes started with -allow-run offer it)
Files the device returns are saved under nodes/<node>/ in your workspace.`,
		Schema: json.RawMessage(`{"type":"object","properties":{
			"node":{"type":"string","description":"node name from nodes_list"},
			"command":{"type":"string","description":"e.g. screen.snapshot"},
			"args":{"type":"object","description":"the command's arguments"},
			"timeout_seconds":{"type":"integer","description":"how long to wait for the device (default from config, at most 600)"}
		},"required":["node","command"]}`),
	}
}

type invokeInput struct {
	Node    string          `json:"node"`
	Command string          `json:"command"`
	Args    json.RawMessage `json:"args"`
	Timeout int             `json:"timeout_seconds"`
}

func (t invokeTool) Run(ctx context.Context, env *tools.Env, input json.RawMessage) (string, error) {
	var in invokeInput
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	if env.Mode == tools.ModeReadOnly {
		return "", fmt.Errorf("%w: node_invoke is not allowed in read-only mode", tools.ErrDenied)
	}
	live, ok := t.h.Online()[in.Node]
	if !ok {
		names := make([]string, 0)
		for n := range t.h.Online() {
			names = append(names, n)
		}
		slices.Sort(names)
		if len(names) == 0 {
			return "", fmt.Errorf("node %q is not connected, and no node is; nodes_list shows the paired ones", in.Node)
		}
		return "", fmt.Errorf("node %q is not connected; connected: %s", in.Node, strings.Join(names, ", "))
	}
	if !slices.Contains(live.Caps, in.Command) {
		return "", fmt.Errorf("%s does not offer %q; it offers: %s", in.Node, in.Command, strings.Join(live.Caps, ", "))
	}
	args := in.Args
	if in.Command == CmdFilesPut {
		var err error
		if args, err = putArgs(env, args); err != nil {
			return "", err
		}
	}
	note := ""
	if !t.cfg.Allowed(in.Node, in.Command) {
		summary := in.Command + " on node " + in.Node
		if a := strings.TrimSpace(string(in.Args)); a != "" && a != "{}" && a != "null" {
			if len(a) > 400 {
				a = a[:400] + "..."
			}
			summary += " " + a
		}
		var err error
		note, err = tools.Ask(ctx, env, tools.ApprovalRequest{Tool: "node_invoke", Summary: summary, Input: input})
		if err != nil {
			return "", err
		}
	}
	timeout := t.cfg.InvokeTimeout()
	if in.Timeout > 0 {
		timeout = min(time.Duration(in.Timeout)*time.Second, 10*time.Minute)
	}
	res, err := t.h.Invoke(ctx, in.Node, in.Command, args, timeout)
	if err != nil {
		return "", err
	}
	if res.Error != "" {
		return "", fmt.Errorf("%s on %s failed: %s", in.Command, in.Node, res.Error)
	}
	out := t.files(ctx, env, in.Node, res)
	if note != "" {
		out += "\n\n" + note
	}
	return out, nil
}

// putArgs turns files.put's "from" (a workspace file) into the file's
// bytes, so the device receives the content rather than a path it cannot
// read.
func putArgs(env *tools.Env, raw json.RawMessage) (json.RawMessage, error) {
	var in struct {
		Path string `json:"path"`
		From string `json:"from"`
		Text string `json:"text"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, fmt.Errorf("invalid args: %w", err)
		}
	}
	if in.Path == "" {
		return nil, errors.New("files.put needs args.path, where the file goes on the device")
	}
	if in.From == "" {
		return raw, nil
	}
	p := env.Resolve(in.From)
	if !env.InWorkspace(p) && env.Mode != tools.ModeFull {
		return nil, fmt.Errorf("%s is outside your workspace; copy it there first", in.From)
	}
	st, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if st.Size() > MaxFile {
		return nil, fmt.Errorf("%s is over the %d MB limit for node files", in.From, MaxFile>>20)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"path": in.Path, "data": data})
}

// files saves the files in a result under nodes/<node>/ in the workspace
// and shows images to the model.
func (t invokeTool) files(ctx context.Context, env *tools.Env, node string, res *Result) string {
	text := strings.TrimSpace(res.Text)
	if text == "" {
		text = "done"
	}
	if len(res.Files) == 0 {
		return text
	}
	// Saved only where nodes/<node>/ really is inside the workspace, never
	// through a link (tools.Env.OutputDir).
	dir, err := env.OutputDir(filepath.Join("nodes", media.SafeName(node)))
	if err == nil {
		err = os.MkdirAll(dir, 0o755)
	}
	if err == nil {
		dir, err = env.OutputDir(dir)
	}
	if err != nil {
		return text + "\n(could not save the files: " + err.Error() + ")"
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	var b strings.Builder
	b.WriteString(text)
	for i, f := range res.Files {
		name := media.SafeName(f.Name)
		if name == "" {
			name = fmt.Sprintf("file-%d", i+1)
		}
		path := filepath.Join(dir, stamp+"-"+name)
		if err := store.WriteFileAtomic(path, f.Data, 0o644); err != nil {
			fmt.Fprintf(&b, "\n(could not save %s: %v)", name, err)
			continue
		}
		fmt.Fprintf(&b, "\nsaved %s (%d bytes)", path, len(f.Data))
		if !strings.HasPrefix(media.Detect(name, f.MediaType, f.Data), "image/") || env.Blobs == nil {
			continue
		}
		img, mt, note, err := mediatools.ModelImage(name, f.Data)
		if err != nil {
			continue
		}
		ref, err := env.Blobs.Put(img)
		if err != nil {
			continue
		}
		if tools.AddMedia(ctx, llm.Block{Type: llm.Image, Media: ref, MediaType: mt, Name: name}) {
			fmt.Fprintf(&b, ", shown to you below (%s)", note)
		}
	}
	return b.String()
}
