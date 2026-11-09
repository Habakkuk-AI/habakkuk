package voice

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"math"
	"math/rand"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// pcm makes d of audio: a sine at amp (0..1 of full scale) or, at 0, faint
// noise like a quiet room.
func pcm(d time.Duration, amp float64, rng *rand.Rand) []byte {
	n := int(d.Seconds() * SampleRate)
	out := make([]byte, 2*n)
	for i := range n {
		v := amp * math.Sin(2*math.Pi*220*float64(i)/SampleRate)
		if amp == 0 {
			v = (rng.Float64()*2 - 1) * 0.003
		}
		binary.LittleEndian.PutUint16(out[2*i:], uint16(int16(v*32767)))
	}
	return out
}

func feedAll(v *VAD, audio []byte) []Event {
	var out []Event
	for i := 0; i+FrameBytes <= len(audio); i += FrameBytes {
		if ev := v.Feed(audio[i : i+FrameBytes]); ev.Kind != None {
			out = append(out, ev)
		}
	}
	return out
}

func TestRMS(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	if got := RMS(pcm(100*time.Millisecond, 0.5, rng)); math.Abs(got-0.5/math.Sqrt2) > 0.01 {
		t.Fatalf("sine rms = %v", got)
	}
	if got := RMS(make([]byte, FrameBytes)); got != 0 {
		t.Fatalf("silence rms = %v", got)
	}
}

func TestVADFindsUtterances(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	v := NewVAD(VADConfig{Threshold: 0.02, Silence: 400 * time.Millisecond})
	var audio []byte
	audio = append(audio, pcm(time.Second, 0, rng)...)
	audio = append(audio, pcm(800*time.Millisecond, 0.3, rng)...) // first utterance
	audio = append(audio, pcm(600*time.Millisecond, 0, rng)...)
	audio = append(audio, pcm(100*time.Millisecond, 0.3, rng)...) // a click: too short
	audio = append(audio, pcm(600*time.Millisecond, 0, rng)...)
	audio = append(audio, pcm(500*time.Millisecond, 0.3, rng)...) // second utterance
	audio = append(audio, pcm(200*time.Millisecond, 0, rng)...)   // a pause shorter than the silence timeout
	audio = append(audio, pcm(500*time.Millisecond, 0.3, rng)...)
	audio = append(audio, pcm(time.Second, 0, rng)...)
	var kinds []EventKind
	var utts [][]byte
	for _, ev := range feedAll(v, audio) {
		kinds = append(kinds, ev.Kind)
		if ev.Kind == Utterance {
			utts = append(utts, ev.PCM)
		}
	}
	want := []EventKind{SpeechStart, Utterance, SpeechStart, Dropped, SpeechStart, Utterance}
	if !slices.Equal(kinds, want) {
		t.Fatalf("events = %v, want %v", kinds, want)
	}
	// Pre-roll, speech, and the trailing silence up to the timeout.
	ms := func(b []byte) int { return len(b) / 2 * 1000 / SampleRate }
	if got := ms(utts[0]); got < 1100 || got > 1700 {
		t.Fatalf("first utterance is %d ms", got)
	}
	if got := ms(utts[1]); got < 1500 || got > 2100 {
		t.Fatalf("second utterance (one pause inside) is %d ms", got)
	}
	if v.Speaking() {
		t.Fatal("still speaking after silence")
	}
}

func TestVADLearnsTheNoiseFloorAndBoost(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	v := NewVAD(VADConfig{Threshold: 0.01})
	// A steady hum just over the threshold is the room, not speech.
	if evs := feedAll(v, pcm(3*time.Second, 0.012, rng)); len(evs) != 0 && evs[0].Kind == SpeechStart {
		// the first frames may trigger before the floor is learned; after
		// that the hum must not keep the detector talking
		v.Reset()
	}
	if evs := feedAll(v, pcm(2*time.Second, 0.012, rng)); len(evs) != 0 {
		t.Fatalf("hum counted as speech: %v", evs)
	}
	// With the boost (a reply playing) moderate speech does not start;
	// loud speech still does.
	v = NewVAD(VADConfig{Threshold: 0.05})
	v.Boost = 2
	if evs := feedAll(v, pcm(500*time.Millisecond, 0.08, rng)); len(evs) != 0 {
		t.Fatalf("boosted detector started on moderate speech: %v", evs)
	}
	if evs := feedAll(v, pcm(500*time.Millisecond, 0.4, rng)); len(evs) == 0 || evs[0].Kind != SpeechStart {
		t.Fatalf("boosted detector missed loud speech: %v", evs)
	}
}

func TestVADCutsLongUtterances(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	v := NewVAD(VADConfig{MaxUtterance: time.Second})
	evs := feedAll(v, pcm(2500*time.Millisecond, 0.3, rng))
	n := 0
	for _, e := range evs {
		if e.Kind == Utterance {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("utterances = %d (%v)", n, evs)
	}
}

func TestMatchWake(t *testing.T) {
	phrases := []string{"hey habakkuk", "ok habakkuk"}
	for _, c := range []struct {
		in, rest string
		ok       bool
	}{
		{"Hey Habakkuk, what's on my calendar today?", "what's on my calendar today?", true},
		{"hey habakkuk", "", true},
		{"Hey, Habakkuk.", "", true},
		{"Hay Habakkuk turn off the lights", "turn off the lights", true},
		{"Hey Habakkuks, what time is it", "what time is it", true},
		{"heyhabakkuk play music", "play music", true},
		{"Um, hey Habakkuk. Remind me at five.", "Remind me at five.", true},
		{"Okay Habakkuk: stop", "stop", true}, // "okay" is within reach of "ok"
		{"OK Habakkuk stop", "stop", true},
		{"What time is it?", "", false},
		{"Habakkuk is my favourite day", "", false},
		{"hey there habakkuk", "", false},
		{"", "", false},
	} {
		rest, ok := MatchWake(c.in, phrases)
		if ok != c.ok || rest != c.rest {
			t.Errorf("MatchWake(%q) = %q, %v; want %q, %v", c.in, rest, ok, c.rest, c.ok)
		}
	}
	if got := ParsePhrases(" hey habakkuk, ,computer "); !slices.Equal(got, []string{"hey habakkuk", "computer"}) {
		t.Fatalf("ParsePhrases = %q", got)
	}
	if _, ok := MatchWake("Computer, lights", []string{"computer"}); !ok {
		t.Fatal("one-word phrase")
	}
}

func TestCaptureCommandPerOS(t *testing.T) {
	none := func(string) bool { return false }
	pulse := func(p string) bool { return p == "pactl" }
	tail := []string{"-ac", "1", "-ar", "16000", "-f", "s16le", "-acodec", "pcm_s16le", "-"}
	for _, c := range []struct {
		goos string
		cap  Capture
		have func(string) bool
		in   []string
	}{
		{"darwin", Capture{}, none, []string{"-f", "avfoundation", "-i", ":0"}},
		{"darwin", Capture{Device: "2"}, none, []string{"-f", "avfoundation", "-i", ":2"}},
		{"darwin", Capture{Device: ":MacBook Pro Microphone"}, none, []string{"-f", "avfoundation", "-i", ":MacBook Pro Microphone"}},
		{"linux", Capture{}, pulse, []string{"-f", "pulse", "-i", "default"}},
		{"linux", Capture{}, none, []string{"-f", "alsa", "-i", "default"}},
		{"linux", Capture{Input: "alsa", Device: "hw:1,0"}, pulse, []string{"-f", "alsa", "-i", "hw:1,0"}},
	} {
		name, args, err := CaptureCommand(c.goos, c.cap, c.have)
		if err != nil {
			t.Fatalf("%s %+v: %v", c.goos, c.cap, err)
		}
		want := slices.Concat([]string{"-hide_banner", "-loglevel", "error", "-nostdin"}, c.in, tail)
		if name != "ffmpeg" || !slices.Equal(args, want) {
			t.Errorf("%s %+v: %s %q\nwant %q", c.goos, c.cap, name, args, want)
		}
	}
	if _, _, err := CaptureCommand("windows", Capture{}, none); err == nil {
		t.Error("windows without input: no error")
	}
	if _, _, err := CaptureCommand("linux", Capture{Input: "dshow"}, none); err == nil {
		t.Error("unknown input: no error")
	}
}

func TestPlayerCommand(t *testing.T) {
	only := func(names ...string) func(string) bool {
		return func(p string) bool { return slices.Contains(names, p) }
	}
	for _, c := range []struct {
		goos, player string
		have         func(string) bool
		name         string
		args         []string
		wav          bool
	}{
		{"darwin", "", only("afplay", "ffplay"), "afplay", []string{"x.ogg"}, true},
		{"linux", "", only("ffplay", "paplay"), "ffplay", []string{"-nodisp", "-autoexit", "-loglevel", "quiet", "x.ogg"}, false},
		{"linux", "", only("paplay", "aplay"), "paplay", []string{"x.ogg"}, true},
		{"linux", "", only("aplay"), "aplay", []string{"-q", "x.ogg"}, true},
		{"linux", "aplay", only("ffplay"), "aplay", []string{"-q", "x.ogg"}, true},
	} {
		name, args, wav, err := PlayerCommand(c.goos, c.player, c.have, "x.ogg")
		if err != nil || name != c.name || !slices.Equal(args, c.args) || wav != c.wav {
			t.Errorf("%s %q: %s %q %v %v", c.goos, c.player, name, args, wav, err)
		}
	}
	if _, _, _, err := PlayerCommand("linux", "", only(), "x.ogg"); err == nil {
		t.Error("no player: no error")
	}
	if n, a := ConvertCommand("in.ogg", "out.wav"); n != "ffmpeg" || a[len(a)-1] != "out.wav" || !slices.Contains(a, "in.ogg") {
		t.Errorf("convert: %s %q", n, a)
	}
}

func TestWAVHeader(t *testing.T) {
	w := WAV(make([]byte, 320))
	if len(w) != 44+320 || string(w[:4]) != "RIFF" || string(w[8:16]) != "WAVEfmt " || string(w[36:40]) != "data" ||
		binary.LittleEndian.Uint32(w[24:]) != SampleRate || binary.LittleEndian.Uint32(w[40:]) != 320 {
		t.Fatalf("header: % x", w[:44])
	}
}

// fakeMic is a microphone the test speaks into, frame-paced like a device.
type fakeMic struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func newFakeMic() *fakeMic {
	r, w := io.Pipe()
	return &fakeMic{r: r, w: w}
}

func (m *fakeMic) speak(t *testing.T, audio []byte) {
	t.Helper()
	if _, err := m.w.Write(audio); err != nil {
		t.Fatal(err)
	}
}

type loopRig struct {
	t       *Talker
	mic     *fakeMic
	mu      sync.Mutex
	heard   []string // transcripts to return, in order
	asked   []string
	played  []string
	playing chan context.Context
	out     bytes.Buffer
	lw      *lockedWriter
}

func newLoopRig(wake []string) *loopRig {
	r := &loopRig{mic: newFakeMic(), playing: make(chan context.Context, 8)}
	r.lw = &lockedWriter{w: &r.out}
	r.t = &Talker{
		Out:    r.lw,
		Listen: func(context.Context) (io.ReadCloser, error) { return r.mic.r, nil },
		Play: func(ctx context.Context, data []byte, ext string) error {
			r.mu.Lock()
			r.played = append(r.played, ext)
			r.mu.Unlock()
			r.playing <- ctx
			<-ctx.Done() // plays until stopped
			return nil
		},
		Transcribe: func(_ context.Context, name, mt string, data []byte) (string, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			if name != "utterance.wav" || mt != "audio/wav" || string(data[:4]) != "RIFF" {
				return "", io.ErrUnexpectedEOF
			}
			text := r.heard[0]
			r.heard = r.heard[1:]
			return text, nil
		},
		Ask: func(_ context.Context, text string) (string, error) {
			r.mu.Lock()
			r.asked = append(r.asked, text)
			r.mu.Unlock()
			return "**Sure**: " + text, nil
		},
		Speak: func(_ context.Context, text string) ([]byte, string, error) { return []byte("OggS"), ".ogg", nil },
		VAD:   VADConfig{Threshold: 0.02, Silence: 300 * time.Millisecond},
		Wake:  wake, FollowUp: 30 * time.Second, BargeIn: true,
	}
	return r
}

func (r *loopRig) snapshot() (asked, played []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.asked), slices.Clone(r.played)
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestTalkerWakeFollowUpAndBargeIn(t *testing.T) {
	rng := rand.New(rand.NewSource(4))
	r := newLoopRig([]string{"hey habakkuk"})
	r.heard = []string{"what's the weather", "Hey Habakkuk, what's the time?", "and tomorrow?"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.t.Run(ctx) }()
	utter := func() {
		r.mic.speak(t, pcm(500*time.Millisecond, 0.3, rng))
		r.mic.speak(t, pcm(400*time.Millisecond, 0, rng))
	}
	r.mic.speak(t, pcm(500*time.Millisecond, 0, rng))

	// Without the wake phrase nothing happens.
	utter()
	// With it, the rest of the utterance is the turn, and the reply plays.
	utter()
	var first context.Context
	select {
	case first = <-r.playing:
	case <-time.After(5 * time.Second):
		t.Fatal("no reply played")
	}
	if asked, _ := r.snapshot(); !slices.Equal(asked, []string{"what's the time?"}) {
		t.Fatalf("asked = %q", asked)
	}
	// Talking over the reply stops it (barge-in), and within the follow-up
	// window no wake phrase is needed.
	r.mic.speak(t, pcm(500*time.Millisecond, 0.5, rng))
	select {
	case <-first.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("reply not interrupted")
	}
	r.mic.speak(t, pcm(400*time.Millisecond, 0, rng))
	waitUntil(t, "follow-up turn", func() bool { a, _ := r.snapshot(); return len(a) == 2 })
	if asked, played := r.snapshot(); asked[1] != "and tomorrow?" || len(played) < 1 || played[0] != ".ogg" {
		t.Fatalf("asked = %q, played = %q", asked, played)
	}
	r.lw.mu.Lock()
	out := r.out.String()
	r.lw.mu.Unlock()
	if !strings.Contains(out, "(not for me: \"what's the weather\")") || !strings.Contains(out, "you: and tomorrow?") {
		t.Fatalf("printed:\n%s", out)
	}
	cancel()
	r.mic.w.Close()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestTalkerWakePhraseAloneOpensTheWindow(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	r := newLoopRig([]string{"hey habakkuk"})
	now := time.Unix(1000, 0)
	var clock sync.Mutex
	r.t.Now = func() time.Time { clock.Lock(); defer clock.Unlock(); return now }
	r.heard = []string{"hey habakkuk", "set a timer", "later on"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.t.Run(ctx) }()
	utter := func() {
		r.mic.speak(t, pcm(500*time.Millisecond, 0.3, rng))
		r.mic.speak(t, pcm(400*time.Millisecond, 0, rng))
	}
	utter()
	select { // the cue tone
	case <-r.playing:
	case <-time.After(5 * time.Second):
		t.Fatal("no cue")
	}
	utter() // interrupts the cue, then is the request
	waitUntil(t, "turn", func() bool { a, _ := r.snapshot(); return len(a) == 1 })
	<-r.playing // the reply
	r.t.stopPlaying()
	clock.Lock()
	now = now.Add(time.Minute) // the window closed
	clock.Unlock()
	utter()
	time.Sleep(200 * time.Millisecond)
	if asked, played := r.snapshot(); !slices.Equal(asked, []string{"set a timer"}) || played[0] != ".wav" {
		t.Fatalf("asked = %q, played = %q", asked, played)
	}
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
