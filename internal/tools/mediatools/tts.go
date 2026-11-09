package mediatools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Audio is synthesized speech.
type Audio struct {
	Data      []byte
	MediaType string // audio/ogg (Opus) or audio/mpeg
	Ext       string // ".ogg" or ".mp3"
}

// Speech turns text into audio with one provider.
type Speech struct {
	Provider     string // openai | elevenlabs | edge
	Key          string
	KeyEnv       string
	BaseURL      string
	Model        string
	Voice        string
	Format       string // opus | mp3 (elevenlabs also takes a raw output_format)
	Instructions string
	MaxChars     int // longest text spoken; 0 is no limit
	HTTP         *http.Client
	// Now is the clock for the edge endpoint's request token; tests set it.
	Now func() time.Time
}

// ErrTooLong means the text is over the configured MaxChars.
var ErrTooLong = errors.New("text is too long to speak")

// Synthesize speaks text with the configured voice, or voice when set.
func (s *Speech) Synthesize(ctx context.Context, text, voice string) (Audio, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Audio{}, errors.New("nothing to say: the text is empty")
	}
	if s.MaxChars > 0 && utf8.RuneCountInString(text) > s.MaxChars {
		return Audio{}, fmt.Errorf("%w: %d characters, the limit is %d", ErrTooLong, utf8.RuneCountInString(text), s.MaxChars)
	}
	if voice == "" {
		voice = s.Voice
	}
	var (
		data []byte
		err  error
	)
	switch s.Provider {
	case "openai":
		data, err = s.openai(ctx, text, voice)
	case "elevenlabs":
		data, err = s.elevenlabs(ctx, text, voice)
	case "edge":
		data, err = s.edge(ctx, text, voice)
	default:
		return Audio{}, fmt.Errorf("unknown speech provider %q", s.Provider)
	}
	if err != nil {
		return Audio{}, fmt.Errorf("%s speech: %w", s.Provider, err)
	}
	if len(data) == 0 {
		return Audio{}, fmt.Errorf("%s speech: no audio came back", s.Provider)
	}
	mt, ext := sniffAudio(data)
	return Audio{Data: data, MediaType: mt, Ext: ext}, nil
}

func (s *Speech) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return newClient(2 * time.Minute)
}

func (s *Speech) openai(ctx context.Context, text, voice string) ([]byte, error) {
	if s.Key == "" {
		return nil, missingKey("openai speech", s.KeyEnv)
	}
	base := strings.TrimRight(s.BaseURL, "/")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	format := "opus" // Ogg Opus, what voice notes use
	if s.Format == "mp3" {
		format = "mp3"
	}
	body := map[string]any{
		"model":           firstNonEmpty(s.Model, "gpt-4o-mini-tts"),
		"input":           text,
		"voice":           firstNonEmpty(voice, "alloy"),
		"response_format": format,
	}
	if s.Instructions != "" {
		body["instructions"] = s.Instructions
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, base+"/audio/speech", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.Key)
	req.Header.Set("Content-Type", "application/json")
	return doRaw(ctx, s.client(), req, 50<<20)
}

func (s *Speech) elevenlabs(ctx context.Context, text, voice string) ([]byte, error) {
	if s.Key == "" {
		return nil, missingKey("elevenlabs speech", s.KeyEnv)
	}
	if voice == "" {
		return nil, errors.New("no voice id configured")
	}
	base := strings.TrimRight(s.BaseURL, "/")
	if base == "" {
		base = "https://api.elevenlabs.io"
	}
	format := s.Format
	switch format {
	case "", "opus":
		format = "opus_48000_64"
	case "mp3":
		format = "mp3_44100_128"
	}
	raw, _ := json.Marshal(map[string]string{"text": text, "model_id": firstNonEmpty(s.Model, "eleven_multilingual_v2")})
	u := base + "/v1/text-to-speech/" + url.PathEscape(voice) + "?output_format=" + url.QueryEscape(format)
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("xi-api-key", s.Key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "audio/*")
	return doRaw(ctx, s.client(), req, 50<<20)
}

// sniffAudio names the container of synthesized audio.
func sniffAudio(data []byte) (mediaType, ext string) {
	switch {
	case bytes.HasPrefix(data, []byte("OggS")):
		return "audio/ogg", ".ogg"
	case bytes.HasPrefix(data, []byte("RIFF")):
		return "audio/wav", ".wav"
	case bytes.HasPrefix(data, []byte("fLaC")):
		return "audio/flac", ".flac"
	case bytes.HasPrefix(data, []byte{0x1a, 0x45, 0xdf, 0xa3}):
		return "audio/webm", ".webm"
	}
	return "audio/mpeg", ".mp3" // ID3 tag or a bare MPEG frame
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

var (
	reFence     = regexp.MustCompile("(?s)```.*?```")
	reInline    = regexp.MustCompile("`([^`]*)`")
	reImage     = regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`)
	reLink      = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)
	reURL       = regexp.MustCompile(`https?://\S+`)
	reHeading   = regexp.MustCompile(`(?m)^\s{0,3}#{1,6}\s*`)
	reQuote     = regexp.MustCompile(`(?m)^\s*>\s?`)
	reBullet    = regexp.MustCompile(`(?m)^\s*(?:[-*+]|\d+[.)])\s+`)
	reRule      = regexp.MustCompile(`(?m)^\s*(?:[-*_]\s*){3,}$`)
	reEmphasis  = regexp.MustCompile(`(\*\*|__|\*|~~)`)
	reTableRule = regexp.MustCompile(`(?m)^\s*\|?\s*:?-{2,}.*$`)
	reSpaces    = regexp.MustCompile(`[ \t]+`)
	reLines     = regexp.MustCompile(`\n{2,}`)
)

// SpeechText turns a Markdown reply into text worth reading aloud: code
// blocks are dropped, links keep their words, URLs and markup go.
func SpeechText(md string) string {
	s := reFence.ReplaceAllString(md, " ")
	s = reImage.ReplaceAllString(s, "$1")
	s = reLink.ReplaceAllString(s, "$1")
	s = reURL.ReplaceAllString(s, "")
	s = reInline.ReplaceAllString(s, "$1")
	s = reTableRule.ReplaceAllString(s, "")
	s = reRule.ReplaceAllString(s, "")
	s = reHeading.ReplaceAllString(s, "")
	s = reQuote.ReplaceAllString(s, "")
	s = reBullet.ReplaceAllString(s, "")
	s = reEmphasis.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "|", ", ")
	s = reSpaces.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	s = reLines.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
	return strings.TrimSpace(s)
}
