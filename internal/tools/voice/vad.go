// Package voice is the local voice loop behind `habakkuk talk`: microphone
// capture and playback through external programs (ffmpeg, afplay, paplay,
// aplay, ffplay; no cgo), an energy-based voice activity detector, wake
// phrase matching on transcripts, and the loop that turns utterances into
// turns and speaks the replies.
package voice

import (
	"encoding/binary"
	"math"
	"time"
)

// Audio format of everything captured: 16 kHz, mono, signed 16-bit
// little-endian PCM, read in 20 ms frames.
const (
	SampleRate  = 16000
	FrameMillis = 20
	FrameBytes  = SampleRate * FrameMillis / 1000 * 2
)

// VADConfig tunes the detector.
type VADConfig struct {
	// Threshold is the least RMS level (0..1 of full scale) counted as
	// speech. The detector also requires a level well above the room's
	// noise floor, which it learns while nobody speaks.
	Threshold float64
	// Silence ends an utterance.
	Silence time.Duration
	// Start is how much continuous speech starts an utterance; default 60ms.
	Start time.Duration
	// MinSpeech drops shorter utterances (coughs, clicks); default 300ms.
	MinSpeech time.Duration
	// MaxUtterance cuts an utterance that runs on; default 30s.
	MaxUtterance time.Duration
	// PreRoll keeps audio from before speech started, so first syllables
	// are not clipped; default 300ms.
	PreRoll time.Duration
}

// EventKind is what a frame did to the detector.
type EventKind int

const (
	None        EventKind = iota
	SpeechStart           // the user started talking (barge-in point)
	Utterance             // the user stopped; Event.PCM holds what they said
	Dropped               // speech too short to be an utterance
)

// Event is the detector's output for one frame.
type Event struct {
	Kind EventKind
	PCM  []byte // Utterance: the audio, pre-roll included
}

// VAD finds utterances in a stream of frames. It is not safe for
// concurrent use.
type VAD struct {
	cfg      VADConfig
	floor    float64
	speaking bool
	voiced   int // consecutive loud frames while waiting for speech
	quiet    int // consecutive quiet frames while speaking
	speech   int // loud frames in the current utterance
	buf      []byte
	pre      [][]byte
	// Boost multiplies the threshold, so speech must be louder (while a
	// reply plays through speakers the mic hears it).
	Boost float64
}

// NewVAD returns a detector with defaults filled in.
func NewVAD(cfg VADConfig) *VAD {
	if cfg.Threshold <= 0 {
		cfg.Threshold = 0.02
	}
	if cfg.Silence <= 0 {
		cfg.Silence = 800 * time.Millisecond
	}
	if cfg.Start <= 0 {
		cfg.Start = 60 * time.Millisecond
	}
	if cfg.MinSpeech <= 0 {
		cfg.MinSpeech = 300 * time.Millisecond
	}
	if cfg.MaxUtterance <= 0 {
		cfg.MaxUtterance = 30 * time.Second
	}
	if cfg.PreRoll <= 0 {
		cfg.PreRoll = 300 * time.Millisecond
	}
	return &VAD{cfg: cfg, floor: cfg.Threshold / 4, Boost: 1}
}

func frames(d time.Duration) int { return max(1, int(d/(FrameMillis*time.Millisecond))) }

// RMS is a frame's level, 0..1 of full scale.
func RMS(frame []byte) float64 {
	n := len(frame) / 2
	if n == 0 {
		return 0
	}
	var sum float64
	for i := range n {
		v := float64(int16(binary.LittleEndian.Uint16(frame[2*i:]))) / 32768
		sum += v * v
	}
	return math.Sqrt(sum / float64(n))
}

// threshold is the level speech must reach now.
func (v *VAD) threshold() float64 {
	b := v.Boost
	if b < 1 {
		b = 1
	}
	return max(v.cfg.Threshold, v.floor*3) * b
}

// Speaking reports whether an utterance is in progress.
func (v *VAD) Speaking() bool { return v.speaking }

// Feed takes one frame (FrameBytes long; shorter is fine at the end).
func (v *VAD) Feed(frame []byte) Event {
	level := RMS(frame)
	loud := level > v.threshold()
	frame = append([]byte(nil), frame...)
	if !v.speaking {
		if !loud {
			// Learn the room's noise floor, slowly, from quiet frames only.
			v.floor = v.floor*0.95 + level*0.05
			v.voiced = 0
		} else {
			v.voiced++
		}
		v.pre = append(v.pre, frame)
		if keep := frames(v.cfg.PreRoll) + frames(v.cfg.Start); len(v.pre) > keep {
			v.pre = v.pre[len(v.pre)-keep:]
		}
		if v.voiced < frames(v.cfg.Start) {
			return Event{}
		}
		v.speaking, v.quiet, v.speech = true, 0, v.voiced
		v.buf = v.buf[:0]
		for _, f := range v.pre {
			v.buf = append(v.buf, f...)
		}
		v.pre = v.pre[:0]
		v.voiced = 0
		return Event{Kind: SpeechStart}
	}
	v.buf = append(v.buf, frame...)
	if loud {
		v.quiet = 0
		v.speech++
	} else {
		v.quiet++
	}
	total := len(v.buf) / FrameBytes
	if v.quiet < frames(v.cfg.Silence) && total < frames(v.cfg.MaxUtterance) {
		return Event{}
	}
	v.speaking = false
	pcm := v.buf
	v.buf = nil
	if v.speech < frames(v.cfg.MinSpeech) {
		return Event{Kind: Dropped}
	}
	return Event{Kind: Utterance, PCM: pcm}
}

// Reset forgets a half-heard utterance, keeping the learned noise floor.
func (v *VAD) Reset() {
	v.speaking, v.voiced, v.quiet, v.speech = false, 0, 0, 0
	v.buf, v.pre = nil, nil
}
