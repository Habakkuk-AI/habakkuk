package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/tools/media"
	"github.com/habakkuk-ai/habakkuk/internal/tools/mediatools"
	"github.com/habakkuk-ai/habakkuk/internal/tools/voice"
)

// talk is the local voice loop: the default microphone through ffmpeg,
// utterances found by an energy-based detector, transcribed by the
// configured transcription provider, a turn per utterance, and the reply
// spoken with the configured tts and played with afplay, paplay, aplay or
// ffplay. Talking over a reply stops it.
//
// With a wake phrase, every utterance is still transcribed (that is how the
// phrase is heard), so each one costs a transcription call; only those that
// start with the phrase, or come within the follow-up window after a reply,
// start a turn. A local, offline wake-word engine is future work.
func talk(ctx context.Context, home string, args []string) error {
	fs := flag.NewFlagSet("talk", flag.ExitOnError)
	agentName := fs.String("agent", "", "agent to talk to")
	sessionID := fs.String("session", "", "continue a session")
	wake := fs.String("wake", "", `wake phrases, comma-separated (e.g. "hey habakkuk"); default talk.wake, else none`)
	noWake := fs.Bool("no-wake", false, "answer every utterance, even with talk.wake set")
	fs.Parse(args)

	a, closeApp, err := open(ctx, home)
	if err != nil {
		return err
	}
	defer closeApp()
	cfg := a.Cfg
	name, err := defaultAgent(cfg, *agentName)
	if err != nil {
		return err
	}
	tr := cfg.Transcription
	if tr == nil {
		return errors.New("talk needs a transcription section in config.yaml (an OpenAI-compatible speech-to-text provider)")
	}
	p := cfg.Providers[tr.Provider]
	base := p.BaseURL
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	transcriber := &media.Transcriber{BaseURL: base, Model: tr.Model, Prompt: tr.Prompt}
	for _, env := range p.KeysEnv {
		if transcriber.Key = a.Secret(env); transcriber.Key != "" {
			break
		}
	}
	tc := config.TalkDefaults(cfg.Talk)
	phrases := tc.Wake
	if *wake != "" {
		phrases = voice.ParsePhrases(*wake)
	}
	if *noWake {
		phrases = nil
	}

	sid := *sessionID
	if sid == "" {
		sess, err := a.Runner.NewSession(ctx, name)
		if err != nil {
			return err
		}
		sid = sess.ID
	} else if _, err := a.Sessions.Get(ctx, sid); err != nil {
		return err
	}
	term := &terminal{in: bufio.NewReader(os.Stdin)}
	detach := a.Runner.Attach(sid, term) // approvals are answered at the keyboard
	defer detach()
	_, stopWorkers := startWorkers(ctx, a, false)
	defer stopWorkers()

	dev := &voice.Device{GOOS: runtime.GOOS, Capture: voice.Capture{Input: tc.Input, Device: tc.Device},
		Player: tc.Player, Dir: os.TempDir(), Have: voice.HaveProgram}
	t := &voice.Talker{
		Listen:     dev.Listen,
		Play:       dev.Play,
		Transcribe: transcriber.Transcribe,
		Ask: func(ctx context.Context, text string) (string, error) {
			return a.Runner.Ask(ctx, agent.Turn{Session: sid, Text: text}, nil)
		},
		VAD:      voice.VADConfig{Threshold: tc.Threshold, Silence: tc.Silence},
		Wake:     phrases,
		FollowUp: tc.FollowUp,
		BargeIn:  *tc.BargeIn,
		Out:      os.Stdout,
	}
	sp := mediatools.New(cfg, a.Secret).Speech
	if sp != nil {
		t.Speak = func(ctx context.Context, reply string) ([]byte, string, error) {
			audio, err := sp.Synthesize(ctx, spokenReply(reply, sp.MaxChars), "")
			return audio.Data, audio.Ext, err
		}
	}

	fmt.Printf("Habakkuk %s · agent %s · session %s\n", version, name, sid)
	switch {
	case len(phrases) > 0:
		fmt.Printf("Listening for %q. Every utterance is transcribed to hear the wake phrase. Ctrl+C to stop.\n",
			strings.Join(phrases, `", "`))
	default:
		fmt.Println("Listening. Speak, then pause. Ctrl+C to stop.")
	}
	if sp == nil {
		fmt.Println("No tts section in config.yaml: replies are printed, not spoken.")
	} else if t.BargeIn {
		fmt.Println("Talk over a reply to stop it (headphones help: the mic hears the speaker).")
	}
	return t.Run(ctx)
}

// spokenReply is the reply as read aloud: Markdown stripped, and cut at a
// sentence when it is longer than the tts limit.
func spokenReply(reply string, maxChars int) string {
	text := mediatools.SpeechText(reply)
	if maxChars <= 0 || utf8.RuneCountInString(text) <= maxChars {
		return text
	}
	const more = " The rest is on screen."
	cut := string([]rune(text)[:max(0, maxChars-len(more))])
	if i := strings.LastIndexAny(cut, ".!?"); i > len(cut)/2 {
		cut = cut[:i+1]
	}
	return strings.TrimSpace(cut) + more
}
