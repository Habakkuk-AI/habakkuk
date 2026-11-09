package channel

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
	"github.com/habakkuk-ai/habakkuk/internal/tools/mediatools"
)

// oggNote is a tiny Ogg Opus stream lasting secs seconds.
func oggNote(secs int) []byte {
	page := func(granule uint64, payload []byte) []byte {
		h := make([]byte, 27, 28+len(payload))
		copy(h, "OggS")
		binary.LittleEndian.PutUint64(h[6:], granule)
		h[26] = 1
		return append(append(h, byte(len(payload))), payload...)
	}
	head := append([]byte("OpusHead"), 1, 1, 0, 0, 0x80, 0xbb, 0, 0, 0, 0, 0)
	return append(page(0, head), page(uint64(secs*48000), []byte("audio"))...)
}

// fakeSpeech records what it was asked to say.
type fakeSpeech struct {
	mu   sync.Mutex
	said []string
	fail error
}

func (f *fakeSpeech) Synthesize(_ context.Context, text, voice string) (mediatools.Audio, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return mediatools.Audio{}, f.fail
	}
	f.said = append(f.said, text+"|"+voice)
	return mediatools.Audio{Data: oggNote(4), MediaType: "audio/ogg", Ext: ".ogg"}, nil
}

func (f *fakeSpeech) spoken() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.said...)
}

// voiceRec is a recorder that also plays voice notes.
type voiceRec struct {
	*recorder
	mu     sync.Mutex
	voices []sentVoice
}

type sentVoice struct {
	To      Address
	Size    int
	Seconds int
	Caption string
}

func (v *voiceRec) SendVoice(_ context.Context, to Address, data []byte, seconds int, caption string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.voices = append(v.voices, sentVoice{to, len(data), seconds, caption})
	return nil
}

func (v *voiceRec) sentVoices() []sentVoice {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]sentVoice(nil), v.voices...)
}

// voiceRig is a rig whose adapter plays voice notes and whose gateway
// speaks.
func voiceRig(t *testing.T, ch *config.Channel) (*rig, *voiceRec, *fakeSpeech) {
	t.Helper()
	ch.AllowFrom = []string{"7"}
	r := newRig(t, ch)
	vr := &voiceRec{recorder: r.a}
	sp := &fakeSpeech{}
	r.g.Adapters["tg"] = vr
	r.g.Speech = sp
	return r, vr, sp
}

func voiceDM(id string) Message {
	m := dm(id, "")
	m.Attachments = []Attachment{{Name: "voice.ogg", MediaType: "audio/ogg",
		Fetch: func(context.Context) ([]byte, error) { return oggNote(2), nil }}}
	return m
}

func on(b bool) *bool { return &b }

func TestVoiceRepliesMirror(t *testing.T) {
	r, vr, sp := voiceRig(t, &config.Channel{VoiceReplies: "mirror", VoiceText: on(true)})
	ctx := context.Background()
	r.g.Handle(ctx, dm("m1", "typed question"))
	waitFor(t, "text reply", hasText(r.a, "echo: typed question"))
	r.g.Handle(ctx, voiceDM("m2"))
	waitFor(t, "voice reply", func() bool { return len(vr.sentVoices()) == 1 })
	waitFor(t, "text with the voice note", func() bool { return len(r.a.texts()) == 2 })
	if said := sp.spoken(); len(said) != 1 || !strings.HasPrefix(said[0], "echo:") {
		t.Fatalf("spoke %q; a typed message must not get a voice reply", said)
	}
	v := vr.sentVoices()[0]
	if v.Seconds != 4 || v.To.Conversation != "100" || v.To.ReplyTo != "" || v.Caption != "" {
		t.Fatalf("voice note: %+v", v)
	}
	if len(r.a.sentFiles()) != 0 {
		t.Fatal("a voice note also went out as a file")
	}
}

func TestVoiceRepliesAlwaysWithoutText(t *testing.T) {
	r, vr, sp := voiceRig(t, &config.Channel{VoiceReplies: "always", VoiceText: on(false)})
	r.model.reply = "**Done.** See [the report](https://example.com/r) for `details`."
	r.g.Handle(context.Background(), dm("m1", "status?"))
	waitFor(t, "voice reply", func() bool { return len(vr.sentVoices()) == 1 })
	time.Sleep(150 * time.Millisecond)
	if texts := r.a.texts(); len(texts) != 0 {
		t.Fatalf("voice_text off but text sent: %q", texts)
	}
	if said := sp.spoken(); len(said) != 1 || said[0] != "Done. See the report for details.|" {
		t.Fatalf("spoken text: %q", said)
	}
}

func TestVoiceReplyFailureFallsBackToText(t *testing.T) {
	r, vr, sp := voiceRig(t, &config.Channel{VoiceReplies: "always", VoiceText: on(false)})
	sp.fail = errors.New("speech service down")
	r.g.Handle(context.Background(), dm("m1", "hello"))
	waitFor(t, "text fallback", hasText(r.a, "echo: hello"))
	if len(vr.sentVoices()) != 0 {
		t.Fatal("voice sent despite failure")
	}
}

func TestVoiceRepliesOffByDefault(t *testing.T) {
	r, vr, sp := voiceRig(t, &config.Channel{})
	r.g.Handle(context.Background(), voiceDM("m1"))
	waitFor(t, "text reply", func() bool { return len(r.a.texts()) == 1 })
	time.Sleep(100 * time.Millisecond)
	if len(vr.sentVoices()) != 0 || len(sp.spoken()) != 0 {
		t.Fatal("voice reply without voice_replies")
	}
}

func TestVoiceNoteAsFileWhereThereAreNoVoiceNotes(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7"}, VoiceReplies: "always", VoiceText: on(true)})
	r.g.Speech = &fakeSpeech{}
	r.g.Handle(context.Background(), dm("m1", "hi"))
	waitFor(t, "audio file", func() bool { return len(r.a.sentFiles()) == 1 })
	if f := r.a.sentFiles()[0]; f.Name != "voice.ogg" || f.MediaType != "audio/ogg" {
		t.Fatalf("file: %+v", f)
	}
	waitFor(t, "text", hasText(r.a, "echo: hi"))
}

func TestSpeakTool(t *testing.T) {
	r, vr, sp := voiceRig(t, &config.Channel{})
	ws := r.g.Runner.Agents["habakkuk"].Workspace
	tool := speakTool{g: r.g, sp: sp}
	ctx := context.WithValue(context.Background(), originKey{}, Address{Account: "tg", Conversation: "100", ReplyTo: "m1"})
	env := &tools.Env{Session: "s1", Workspace: ws, Mode: tools.ModeWorkspace}
	input, _ := json.Marshal(map[string]string{"text": "Good morning", "voice": "nova"})
	out, err := tool.Run(tools.WithCallID(ctx, "call-1"), env, input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Queued a voice note") || !strings.Contains(out, "4 s") {
		t.Fatalf("result: %s", out)
	}
	if _, err := tool.Run(tools.WithCallID(ctx, "call-1"), env, input); err != nil { // a retried call
		t.Fatal(err)
	}
	waitFor(t, "voice note", func() bool { return len(vr.sentVoices()) >= 1 })
	time.Sleep(100 * time.Millisecond)
	if n := len(vr.sentVoices()); n != 1 {
		t.Fatalf("a retried call sent %d voice notes", n)
	}
	if said := sp.spoken(); said[0] != "Good morning|nova" {
		t.Fatalf("spoken: %q", said)
	}
	saved, _ := filepath.Glob(filepath.Join(ws, "voice", "*.ogg"))
	if len(saved) == 0 {
		t.Fatal("audio not saved in the workspace")
	}

	// No chat: the audio is only saved.
	noChat, err := speakTool{sp: sp}.Run(context.Background(), env, input)
	if err != nil || !strings.Contains(noChat, "not a chat") {
		t.Fatalf("no chat: %v %s", err, noChat)
	}
	if _, err := tool.Run(ctx, env, []byte(`{"text":"  "}`)); err == nil {
		t.Fatal("empty text accepted")
	}
}
