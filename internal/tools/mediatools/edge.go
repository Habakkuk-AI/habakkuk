package mediatools

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
)

// The Edge browser's read-aloud service: a free speech endpoint that needs
// no account. It speaks over a websocket. The client sends two text frames,
// speech.config (output format) and ssml (the text); the service answers
// with text frames (turn.start, audio.metadata, turn.end) and binary frames
// carrying audio, each prefixed with a two-byte big-endian header length and
// a header naming Path:audio. turn.end closes one request.
//
// The connection URL must carry the fixed client token and Sec-MS-GEC, a
// token derived from the current time rounded down to five minutes (see
// edge_gec.go), with the browser version that token format belongs to. A
// clock off by more than that is refused with 403; the client then takes
// the time from the response's Date header and tries once more.
const (
	edgeURL          = "wss://speech.platform.bing.com/consumer/speech/synthesize/readaloud/edge/v1"
	edgeToken        = "6A5AA1D4EAFF4E9FB37E23D68491D6F4"
	edgeChromium     = "143.0.3650.75"
	edgeGECVersion   = "2-" + edgeChromium
	edgeOrigin       = "chrome-extension://jdiccldimpdaibmpdkjnbmckianbfold"
	edgeUserAgent    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36 Edg/143.0.0.0"
	edgeFormat       = "audio-24khz-48kbitrate-mono-mp3"
	edgeDefaultVoice = "en-US-AriaNeural"
	// edgeChunk is the most escaped text sent in one request.
	edgeChunk = 4096
	// winEpoch is the seconds from 1601-01-01 to 1970-01-01.
	winEpoch = 11644473600
)

func (s *Speech) edge(ctx context.Context, text, voice string) ([]byte, error) {
	if voice == "" {
		voice = edgeDefaultVoice
	}
	format := edgeFormat
	if strings.Contains(s.Format, "-") { // a raw service format name
		format = s.Format
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var out bytes.Buffer
	var skew time.Duration
	for _, chunk := range splitSpeech(edgeEscape(text), edgeChunk) {
		audio, newSkew, err := s.edgeRequest(ctx, chunk, edgeVoiceName(voice), format, skew)
		if err != nil {
			return nil, err
		}
		skew = newSkew
		out.Write(audio)
	}
	return out.Bytes(), nil
}

// edgeRequest speaks one chunk of escaped text on a fresh connection, as the
// browser does. It returns the clock skew it learned, if any.
func (s *Speech) edgeRequest(ctx context.Context, escaped, voice, format string, skew time.Duration) ([]byte, time.Duration, error) {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	conn, resp, err := s.edgeDial(ctx, now().Add(skew))
	if err != nil && resp != nil && resp.StatusCode == http.StatusForbidden {
		if server, perr := http.ParseTime(resp.Header.Get("Date")); perr == nil {
			skew = server.Sub(now())
			conn, _, err = s.edgeDial(ctx, now().Add(skew))
		}
	}
	if err != nil {
		return nil, skew, err
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(8 << 20)

	stamp := now().UTC().Format("Mon Jan 02 2006 15:04:05") + " GMT+0000 (Coordinated Universal Time)"
	config := "X-Timestamp:" + stamp + "\r\n" +
		"Content-Type:application/json; charset=utf-8\r\n" +
		"Path:speech.config\r\n\r\n" +
		`{"context":{"synthesis":{"audio":{"metadataoptions":{"sentenceBoundaryEnabled":"false","wordBoundaryEnabled":"false"},"outputFormat":"` + format + `"}}}}` + "\r\n"
	if err := conn.Write(ctx, websocket.MessageText, []byte(config)); err != nil {
		return nil, skew, err
	}
	ssml := "<speak version='1.0' xmlns='http://www.w3.org/2001/10/synthesis' xml:lang='en-US'>" +
		"<voice name='" + voice + "'><prosody pitch='+0Hz' rate='+0%' volume='+0%'>" +
		escaped + "</prosody></voice></speak>"
	// The trailing Z after a timestamp that already names its zone is what
	// the browser sends.
	msg := "X-RequestId:" + randomHex(16) + "\r\n" +
		"Content-Type:application/ssml+xml\r\n" +
		"X-Timestamp:" + stamp + "Z\r\n" +
		"Path:ssml\r\n\r\n" + ssml
	if err := conn.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
		return nil, skew, err
	}

	var audio bytes.Buffer
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return nil, skew, fmt.Errorf("edge: connection ended before the audio did: %w", err)
		}
		switch typ {
		case websocket.MessageText:
			header, _, _ := strings.Cut(string(data), "\r\n\r\n")
			if edgeHeader(header, "Path") == "turn.end" {
				if audio.Len() == 0 {
					return nil, skew, errors.New("edge: the service sent no audio (check the voice name)")
				}
				_ = conn.Close(websocket.StatusNormalClosure, "")
				return audio.Bytes(), skew, nil
			}
		case websocket.MessageBinary:
			if len(data) < 2 {
				continue
			}
			n := int(binary.BigEndian.Uint16(data[:2]))
			if 2+n > len(data) {
				return nil, skew, errors.New("edge: malformed audio frame")
			}
			if edgeHeader(string(data[2:2+n]), "Path") == "audio" {
				audio.Write(data[2+n:])
			}
		}
	}
}

func (s *Speech) edgeDial(ctx context.Context, now time.Time) (*websocket.Conn, *http.Response, error) {
	base := s.BaseURL
	if base == "" {
		base = edgeURL
	}
	u := base + "?TrustedClientToken=" + edgeToken + "&ConnectionId=" + randomHex(16) +
		"&Sec-MS-GEC=" + edgeGEC(now) + "&Sec-MS-GEC-Version=" + edgeGECVersion
	h := http.Header{}
	h.Set("Pragma", "no-cache")
	h.Set("Cache-Control", "no-cache")
	h.Set("Origin", edgeOrigin)
	h.Set("User-Agent", edgeUserAgent)
	h.Set("Accept-Language", "en-US,en;q=0.9")
	h.Set("Cookie", "muid="+strings.ToUpper(randomHex(16))+";")
	opts := &websocket.DialOptions{HTTPHeader: h}
	if s.HTTP != nil {
		opts.HTTPClient = s.HTTP
	}
	return websocket.Dial(ctx, u, opts)
}

// edgeVoiceName expands a short voice name ("en-US-AriaNeural", or with a
// region, "zh-CN-liaoning-XiaobeiNeural") into the service's long form.
func edgeVoiceName(short string) string {
	if strings.HasPrefix(short, "Microsoft Server Speech") {
		return short
	}
	parts := strings.SplitN(short, "-", 3)
	if len(parts) != 3 {
		return short
	}
	lang, name := parts[0]+"-"+parts[1], parts[2]
	if region, rest, ok := strings.Cut(name, "-"); ok {
		lang, name = lang+"-"+region, rest
	}
	return fmt.Sprintf("Microsoft Server Speech Text to Speech Voice (%s, %s)", lang, name)
}

// edgeHeader reads one "Name:value" line from a frame header.
func edgeHeader(header, name string) string {
	for line := range strings.SplitSeq(header, "\r\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(k, name) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// edgeEscape removes control characters the service rejects and escapes the
// text for SSML.
func edgeEscape(text string) string {
	text = strings.Map(func(r rune) rune {
		if r < 32 && r != '\n' && r != '\r' && r != '\t' {
			return ' '
		}
		return r
	}, text)
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
}

// splitSpeech cuts escaped text into pieces of at most limit bytes, at a
// newline or space where it can, never inside a character or an entity.
func splitSpeech(text string, limit int) []string {
	var out []string
	for len(text) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		if i := strings.LastIndex(text[:cut], "\n"); i > limit/2 {
			cut = i + 1
		} else if i := strings.LastIndex(text[:cut], " "); i > limit/2 {
			cut = i + 1
		} else if amp := strings.LastIndex(text[:cut], "&"); amp >= 0 && !strings.Contains(text[amp:cut], ";") {
			cut = amp
		}
		if cut == 0 {
			cut = limit
		}
		if piece := strings.TrimSpace(text[:cut]); piece != "" {
			out = append(out, piece)
		}
		text = text[cut:]
	}
	if piece := strings.TrimSpace(text); piece != "" {
		out = append(out, piece)
	}
	return out
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
