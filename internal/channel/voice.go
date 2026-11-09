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
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
	"github.com/habakkuk-ai/habakkuk/internal/tools/mediatools"
)

// VoiceSender is implemented by adapters that can post voice notes.
type VoiceSender interface {
	// SendVoice posts Ogg Opus audio as a voice note. seconds is its length
	// (0 when unknown); caption is already formatted and may be empty.
	SendVoice(ctx context.Context, to Address, data []byte, seconds int, caption string) error
}

// Speaker turns text into audio (mediatools.Speech).
type Speaker interface {
	Synthesize(ctx context.Context, text, voice string) (mediatools.Audio, error)
}

// voiceFile synthesizes text and stores the audio in the blob store, as a
// voice note when it is (or could be converted to) Ogg Opus, otherwise as
// an ordinary audio file.
func (g *Gateway) voiceFile(ctx context.Context, sp Speaker, text, voice string) (*outFile, mediatools.Audio, error) {
	if g.Blobs == nil {
		return nil, mediatools.Audio{}, errors.New("no blob store to queue the audio in")
	}
	a, err := sp.Synthesize(ctx, text, voice)
	if err != nil {
		return nil, a, err
	}
	note, ok, err := mediatools.VoiceNote(ctx, a)
	if err != nil {
		g.Log.Warn("voice note conversion failed; sending an audio file", "err", err)
	}
	f := &outFile{Name: "voice" + note.Ext, MediaType: note.MediaType, Size: int64(len(note.Data)), Voice: ok}
	if ok {
		f.Seconds = int((mediatools.OggDuration(note.Data) + time.Second/2) / time.Second)
	}
	if f.Ref, err = g.Blobs.Put(note.Data); err != nil {
		return nil, note, err
	}
	return f, note, nil
}

// sendQueuedFile posts a queued file: a voice note through the adapter's
// VoiceSender when it has one, otherwise as a file.
func sendQueuedFile(ctx context.Context, a Adapter, fs FileSender, p deliverPayload, data []byte, caption string) error {
	if vs, ok := a.(VoiceSender); ok && p.File.Voice {
		return vs.SendVoice(ctx, p.To, data, p.File.Seconds, caption)
	}
	return fs.SendFile(ctx, p.To, p.File.Name, p.File.MediaType, data, caption)
}

// deliverReply queues a finished turn's reply: as text, and as a voice
// note too when the channel's voice_replies says so (always, or mirror when
// the user's message carried audio). With voice_text off the voice note
// replaces the text, unless speaking fails.
// draft names the turn whose progress draft the reply replaces.
func (g *Gateway) deliverReply(ctx context.Context, jobID string, media []agent.Attachment, to Address, text, draft string) error {
	ch := g.Channels[to.Account]
	voice := g.Speech != nil && ch != nil && strings.TrimSpace(text) != ""
	if voice {
		switch ch.VoiceReplies {
		case "always":
		case "mirror":
			voice = hasAudio(media)
		default:
			voice = false
		}
	}
	if _, ok := g.Adapters[to.Account].(FileSender); !ok {
		voice = false
	}
	keepText := ch == nil || ch.VoiceText == nil || *ch.VoiceText
	if !voice || keepText {
		if err := g.enqueue(ctx, "dlv_"+jobID, deliverPayload{To: to, Text: text, Draft: draft}); err != nil {
			return err
		}
	}
	if !voice {
		return nil
	}
	spoken := mediatools.SpeechText(text)
	f, _, err := g.voiceFile(ctx, g.Speech, spoken, "")
	if err != nil {
		if errors.Is(err, mediatools.ErrTooLong) {
			g.Log.Info("reply too long to speak; sent as text only", "channel", to.Account, "chars", len(spoken))
		} else {
			g.Log.Warn("voice reply failed; sent as text only", "channel", to.Account, "err", err)
		}
		if keepText {
			return nil
		}
		return g.enqueue(ctx, "dlv_"+jobID, deliverPayload{To: to, Text: text, Draft: draft})
	}
	to.ReplyTo = ""
	return g.enqueue(ctx, "voice_"+jobID, deliverPayload{To: to, File: f, Draft: draft})
}

func hasAudio(media []agent.Attachment) bool {
	for _, a := range media {
		if strings.HasPrefix(a.MediaType, "audio/") {
			return true
		}
	}
	return false
}

// RegisterSpeak adds the speak tool to the core toolset. g may be nil (no
// channels); speak then only saves the audio in the workspace.
func RegisterSpeak(r *tools.Registry, g *Gateway, sp Speaker) {
	r.Register("core", speakTool{g: g, sp: sp})
}

type speakTool struct {
	g  *Gateway
	sp Speaker
}

// Risk is network: the text goes to the speech provider and the audio to
// the chat.
func (speakTool) Risk() tools.Risk { return tools.RiskNetwork }
func (speakTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "speak",
		Description: `Say something out loud: turns text into speech, saves the audio in your workspace (voice/) and sends it to the user in this chat as a voice note (an audio file where the chat has no voice notes). Use it when the user asks to hear something or for a voice reply. Write "text" as words to be spoken: no Markdown, code or URLs. "voice" overrides the configured voice. The audio is queued ahead of your reply; do not repeat it there unless asked.`,
		Schema: json.RawMessage(`{"type":"object","properties":{
			"text":{"type":"string"},
			"voice":{"type":"string","description":"provider voice name or id; default the configured voice"}
		},"required":["text"]}`),
	}
}

func (t speakTool) Run(ctx context.Context, env *tools.Env, input json.RawMessage) (string, error) {
	var in struct {
		Text  string `json:"text"`
		Voice string `json:"voice"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return "", errors.New("text is required")
	}
	g := t.g
	var (
		to      Address
		inChat  bool
		fs      FileSender
		blobbed *outFile
		audio   mediatools.Audio
		err     error
	)
	if g != nil {
		to, inChat = g.origin(ctx, env.Session)
		if inChat {
			var ok bool
			if fs, ok = g.Adapters[to.Account].(FileSender); !ok {
				return "", fmt.Errorf("channel %s cannot send audio", to.Account)
			}
		}
	}
	if g != nil {
		blobbed, audio, err = g.voiceFile(ctx, t.sp, text, strings.TrimSpace(in.Voice))
	} else {
		audio, err = t.sp.Synthesize(ctx, text, strings.TrimSpace(in.Voice))
		if err == nil {
			audio, _, _ = mediatools.VoiceNote(ctx, audio)
		}
	}
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(audio.Data)
	path := filepath.Join(env.Workspace, "voice", time.Now().UTC().Format("20060102-150405")+"-"+hex.EncodeToString(sum[:4])+audio.Ext)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := store.WriteFileAtomic(path, audio.Data, 0o600); err != nil {
		return "", err
	}
	if !inChat {
		return fmt.Sprintf("Saved the audio at %s (%s). This session is not a chat, so it was not sent; tell the user the path.", path, size(int64(len(audio.Data)))), nil
	}
	if limit := min(fs.MaxFileSize(), maxSendFile); blobbed.Size > limit {
		return "", fmt.Errorf("the audio is %s; this chat takes files up to %s. Say less", size(blobbed.Size), size(limit))
	}
	to.ReplyTo = ""
	if err := g.enqueue(ctx, fileJobID(env.Session, tools.CallID(ctx), blobbed.Ref, "speak"),
		deliverPayload{To: to, File: blobbed}); err != nil {
		return "", err
	}
	kind := "a voice note"
	if !blobbed.Voice {
		kind = "an audio file (" + blobbed.MediaType + "; install ffmpeg to send voice notes)"
	}
	if blobbed.Seconds > 0 {
		kind += fmt.Sprintf(", %d s", blobbed.Seconds)
	}
	return fmt.Sprintf("Queued %s for this chat; also saved at %s.", kind, path), nil
}
