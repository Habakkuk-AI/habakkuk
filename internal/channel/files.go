package channel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
	"github.com/habakkuk-ai/habakkuk/internal/tools/media"
)

// originKey carries a channel turn's reply address to the tools it runs.
type originKey struct{}

const (
	// maxSendFile caps what send_file reads into memory, below every
	// adapter's own limit.
	maxSendFile = 100 << 20
	maxCaption  = 900 // Telegram allows 1024 after markup
)

// outFile is a file waiting in the blob store to be delivered.
type outFile struct {
	Ref       string `json:"ref"`
	Name      string `json:"name"`
	MediaType string `json:"media_type"`
	Size      int64  `json:"size"`
	Voice     bool   `json:"voice,omitempty"`   // Ogg Opus to post as a voice note where the platform has them
	Seconds   int    `json:"seconds,omitempty"` // a voice note's length
}

// RegisterTools adds send_file, clarify, poll and message_home to the core
// toolset. Agents without a home chat are not offered message_home.
func RegisterTools(r *tools.Registry, g *Gateway) {
	r.Register("core", sendFileTool{g})
	r.Register("core", clarifyTool{g})
	r.Register("core", pollTool{g})
	r.Register("core", messageHomeTool{g})
}

var errNoChat = errors.New("send_file only works in a chat conversation (a messaging channel) and this session is not one; tell the user the file's path instead")

type sendFileTool struct{ g *Gateway }

// Risk is network: the file leaves the machine. Workspace mode sends
// workspace files without asking; read-only mode cannot send.
func (sendFileTool) Risk() tools.Risk { return tools.RiskNetwork }
func (sendFileTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "send_file",
		Description: `Send a file to the user in the current chat: a report, image, spreadsheet, export or anything you made or saved. PNG and JPEG images are shown inline; other files arrive as documents. "path" is relative to your workspace. "caption" is optional short text shown with the file. The file is queued and delivered even across a restart, ahead of your reply; do not paste its contents into the reply as well. Only works in chat conversations.`,
		Schema: json.RawMessage(`{"type":"object","properties":{
			"path":{"type":"string"},
			"caption":{"type":"string","description":"short text shown with the file"}
		},"required":["path"]}`),
	}
}

func (t sendFileTool) Run(ctx context.Context, env *tools.Env, input json.RawMessage) (string, error) {
	var in struct {
		Path    string `json:"path"`
		Caption string `json:"caption"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	in.Caption = strings.TrimSpace(in.Caption)
	if strings.TrimSpace(in.Path) == "" {
		return "", errors.New("path is required")
	}
	if n := utf8.RuneCountInString(in.Caption); n > maxCaption {
		return "", fmt.Errorf("caption is %d characters; keep it under %d and put the rest in your reply", n, maxCaption)
	}
	g := t.g
	to, ok := g.origin(ctx, env.Session)
	if !ok {
		return "", errNoChat
	}
	fs, ok := g.Adapters[to.Account].(FileSender)
	if !ok {
		return "", fmt.Errorf("channel %s cannot send files", to.Account)
	}
	if g.Blobs == nil {
		return "", errors.New("no blob store to queue the file in")
	}
	path, err := sendable(ctx, env, in.Path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	limit := min(fs.MaxFileSize(), maxSendFile)
	switch {
	case info.IsDir():
		return "", fmt.Errorf("%s is a directory; send its files one by one or archive it first", in.Path)
	case info.Size() == 0:
		return "", fmt.Errorf("%s is empty", in.Path)
	case info.Size() > limit:
		return "", fmt.Errorf("%s is %s; this chat takes files up to %s", in.Path, size(info.Size()), size(limit))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if int64(len(data)) > limit {
		return "", fmt.Errorf("%s grew to %s; this chat takes files up to %s", in.Path, size(int64(len(data))), size(limit))
	}
	name := media.SafeName(filepath.Base(path))
	f := &outFile{Name: name, MediaType: media.Detect(name, "", data), Size: int64(len(data))}
	if f.Ref, err = g.Blobs.Put(data); err != nil {
		return "", err
	}
	to.ReplyTo = ""
	if err := g.enqueue(ctx, fileJobID(env.Session, tools.CallID(ctx), f.Ref, in.Caption),
		deliverPayload{To: to, Text: in.Caption, File: f}); err != nil {
		return "", err
	}
	return fmt.Sprintf("Queued %s (%s, %s) for this chat.", name, f.MediaType, size(f.Size)), nil
}

// fileJobID makes one delivery per tool call: the call id is fixed once the
// model has made the call, so running the tool again for it (a retried
// worker) finds the delivery already queued.
func fileJobID(sessionID, callID, ref, caption string) string {
	key := sessionID + "\x00" + callID
	if callID == "" {
		key += "\x00" + ref + "\x00" + caption
	}
	sum := sha256.Sum256([]byte(key))
	return "file_" + hex.EncodeToString(sum[:12])
}

// origin is the reply address for a session: the address of the channel
// turn running (scheduled runs included), or the chat the session is bound to.
func (g *Gateway) origin(ctx context.Context, sessionID string) (Address, bool) {
	if to, ok := ctx.Value(originKey{}).(Address); ok {
		return to, true
	}
	if conv, err := g.Store.BySession(ctx, sessionID); err == nil && conv != nil {
		return conv.Address, true
	}
	return Address{}, false
}

// sendable resolves a path, following symlinks, and checks the agent may
// send it. Workspace files always; others in full mode, in ask mode (the
// user approved the call), or in workspace mode after asking the user.
func sendable(ctx context.Context, env *tools.Env, p string) (string, error) {
	path, err := filepath.EvalSymlinks(env.Resolve(p))
	if err != nil {
		return "", err
	}
	ws := *env
	if real, err := filepath.EvalSymlinks(env.Workspace); err == nil {
		ws.Workspace = real
	}
	if ws.InWorkspace(path) || env.Mode == tools.ModeFull || env.Mode == tools.ModeAsk {
		return path, nil
	}
	if env.Mode == tools.ModeWorkspace && env.Approver != nil {
		ok, err := env.Approver.Approve(ctx, tools.ApprovalRequest{
			Tool: "send_file", Summary: "send " + path + " (outside the workspace) to this chat",
		})
		if err != nil {
			return "", err
		}
		if ok {
			return path, nil
		}
		return "", fmt.Errorf("%w: the user declined sending %s", tools.ErrDenied, path)
	}
	return "", fmt.Errorf("%w: %s is outside the workspace; copy it into the workspace to send it", tools.ErrDenied, p)
}

// deliverFile sends a queued file. A crash after the platform accepted it
// but before the job completed sends it again; nothing narrower is possible
// without the platform's help.
func (g *Gateway) deliverFile(ctx context.Context, j *jobs.Job, owner string, a Adapter, p deliverPayload) (string, error) {
	fs, ok := a.(FileSender)
	if !ok {
		return "", jobs.Permanent{Err: fmt.Errorf("channel %s cannot send files", p.To.Account)}
	}
	if g.Blobs == nil {
		return "", jobs.Permanent{Err: errors.New("no blob store")}
	}
	data, err := g.Blobs.Get(p.File.Ref)
	if err != nil {
		return "", jobs.Permanent{Err: fmt.Errorf("file %s is gone from the blob store: %w", p.File.Name, err)}
	}
	if err := g.Queue.Owns(ctx, j.ID, owner); err != nil {
		return "", err
	}
	caption := ""
	if p.Text != "" {
		caption = render(a, p.Text)
	}
	if err := sendQueuedFile(WithSendKey(ctx, j.ID+".file"), a, fs, p, data, caption); err != nil {
		if errors.As(err, new(Undeliverable)) {
			return "", jobs.Permanent{Err: err}
		}
		return "", err
	}
	g.messageOut(ctx, p)
	return "sent " + p.File.Name, nil
}

func size(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", (n+1023)>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}
