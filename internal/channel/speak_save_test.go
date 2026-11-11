package channel

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
	"github.com/habakkuk-ai/habakkuk/internal/tools/mediatools"
)

type oggSpeaker struct{}

func (oggSpeaker) Synthesize(context.Context, string, string) (mediatools.Audio, error) {
	return mediatools.Audio{Data: append([]byte("OggS"), make([]byte, 60)...), MediaType: "audio/ogg", Ext: ".ogg"}, nil
}

// speak saves only where voice/ really is inside the workspace: a link
// named voice cannot lead the save into a locked folder.
func TestSpeakDoesNotSaveThroughALink(t *testing.T) {
	ws, home := t.TempDir(), t.TempDir()
	locked := filepath.Join(home, "projects")
	os.MkdirAll(locked, 0o700)
	os.Symlink(locked, filepath.Join(ws, "voice"))
	r := tools.NewRegistry()
	RegisterSpeak(r, nil, oggSpeaker{})
	env := &tools.Env{Agent: "a", Workspace: ws, Mode: tools.ModeFull, Taint: tools.NewTaint(true), Policy: &tools.Policy{Locked: []string{locked}}}
	res := r.Execute(context.Background(), env, llm.Block{Type: llm.ToolUse, ToolUseID: "1", ToolName: "speak", Input: json.RawMessage(`{"text":"hi"}`)})
	if ents, _ := os.ReadDir(locked); len(ents) > 0 || !res.IsError {
		t.Fatalf("speak saved through the link: %+v (%d files there)", res, len(ents))
	}
	// Without the link it saves in the workspace.
	os.Remove(filepath.Join(ws, "voice"))
	res = r.Execute(context.Background(), env, llm.Block{Type: llm.ToolUse, ToolUseID: "2", ToolName: "speak", Input: json.RawMessage(`{"text":"hi"}`)})
	if ents, _ := os.ReadDir(filepath.Join(ws, "voice")); res.IsError || len(ents) != 1 {
		t.Fatalf("speak did not save in the workspace: %+v", res)
	}
}
