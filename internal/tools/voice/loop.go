package voice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Talker is the voice loop: listen, detect an utterance, transcribe it,
// check the wake phrase, run a turn, speak the reply, and stop speaking
// when the user talks over it.
type Talker struct {
	Listen     func(ctx context.Context) (io.ReadCloser, error)
	Play       func(ctx context.Context, data []byte, ext string) error
	Transcribe func(ctx context.Context, name, mediaType string, data []byte) (string, error)
	Ask        func(ctx context.Context, text string) (string, error)
	// Speak turns a reply into audio and its file extension; nil prints
	// replies only.
	Speak func(ctx context.Context, text string) ([]byte, string, error)

	VAD VADConfig
	// Wake phrases; empty means every utterance is a turn.
	Wake []string
	// FollowUp is how long after a reply no wake phrase is needed.
	FollowUp time.Duration
	// BargeIn stops a reply when the user starts talking. While a reply
	// plays, speech must be twice as loud, since the mic hears the speaker.
	BargeIn bool
	Out     io.Writer
	Now     func() time.Time

	mu       sync.Mutex
	stop     context.CancelFunc // stops the reply playing now
	gen      int                // counts replies started, so an old one cannot clear a new one
	playing  atomic.Bool
	awakeTil time.Time
}

func (t *Talker) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

func (t *Talker) say(format string, args ...any) {
	if t.Out != nil {
		fmt.Fprintf(t.Out, format+"\n", args...)
	}
}

// Run listens until ctx ends or the microphone stream fails.
func (t *Talker) Run(ctx context.Context) error {
	mic, err := t.Listen(ctx)
	if err != nil {
		return err
	}
	defer mic.Close()
	utts := make(chan []byte, 4)
	errc := make(chan error, 1)
	go func() { errc <- t.hear(ctx, mic, utts) }()
	defer t.stopPlaying()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errc:
			if ctx.Err() != nil {
				return nil
			}
			return err
		case pcm := <-utts:
			if err := t.handle(ctx, pcm); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				t.say("! %v", err)
			}
		}
	}
}

// hear reads frames into the detector and hands over utterances.
func (t *Talker) hear(ctx context.Context, mic io.Reader, utts chan<- []byte) error {
	vad := NewVAD(t.VAD)
	frame := make([]byte, FrameBytes)
	for {
		if _, err := io.ReadFull(mic, frame); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return errors.New("the microphone stream ended")
			}
			return err
		}
		playing := t.playing.Load()
		if playing && !t.BargeIn {
			vad.Reset() // it would only hear the reply
			continue
		}
		vad.Boost = 1
		if playing {
			vad.Boost = 2
		}
		switch ev := vad.Feed(frame); ev.Kind {
		case SpeechStart:
			if playing {
				t.stopPlaying() // barge-in
			}
		case Utterance:
			select {
			case utts <- ev.PCM:
			case <-ctx.Done():
				return nil
			default:
				t.say("! busy; missed what you just said")
			}
		}
	}
}

// handle turns one utterance into a turn and starts speaking its reply.
func (t *Talker) handle(ctx context.Context, pcm []byte) error {
	tctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	text, err := t.Transcribe(tctx, "utterance.wav", "audio/wav", WAV(pcm))
	cancel()
	if err != nil {
		return fmt.Errorf("transcription failed: %w", err)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if len(t.Wake) > 0 {
		rest, woke := MatchWake(text, t.Wake)
		switch {
		case woke:
			text = rest
		case !t.awake():
			t.say("  (not for me: %q)", text)
			return nil
		}
		if text == "" { // just the wake phrase: listen for the request
			t.wake(t.FollowUp)
			t.say("  listening…")
			t.startPlaying(ctx, WAV(Tone(880, 150)), ".wav")
			return nil
		}
	}
	t.stopPlaying()
	t.say("you: %s", text)
	reply, err := t.Ask(ctx, text)
	if err != nil {
		return err
	}
	reply = strings.TrimSpace(reply)
	t.say("habakkuk: %s", reply)
	t.wake(t.FollowUp)
	if reply == "" || t.Speak == nil {
		return nil
	}
	audio, ext, err := t.Speak(ctx, reply)
	if err != nil {
		return fmt.Errorf("speech failed: %w", err)
	}
	t.startPlaying(ctx, audio, ext)
	return nil
}

func (t *Talker) awake() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.now().Before(t.awakeTil)
}

func (t *Talker) wake(d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if til := t.now().Add(d); til.After(t.awakeTil) {
		t.awakeTil = til
	}
}

// startPlaying plays audio in the background, replacing anything playing.
// The follow-up window runs from when it ends.
func (t *Talker) startPlaying(ctx context.Context, data []byte, ext string) {
	t.stopPlaying()
	pctx, cancel := context.WithCancel(ctx)
	t.mu.Lock()
	t.gen++
	gen := t.gen
	t.stop = cancel
	t.playing.Store(true)
	t.mu.Unlock()
	go func() {
		defer cancel()
		if err := t.Play(pctx, data, ext); err != nil && pctx.Err() == nil {
			t.say("! playback failed: %v", err)
		}
		t.mu.Lock()
		if t.gen == gen { // nothing newer started
			t.stop = nil
			t.playing.Store(false)
		}
		t.mu.Unlock()
		if pctx.Err() == nil { // it played to the end: the window runs from now
			t.wake(t.FollowUp)
		}
	}()
}

// stopPlaying stops the reply playing now, if any.
func (t *Talker) stopPlaying() {
	t.mu.Lock()
	stop := t.stop
	t.stop = nil
	t.playing.Store(false)
	t.mu.Unlock()
	if stop != nil {
		stop()
	}
}
