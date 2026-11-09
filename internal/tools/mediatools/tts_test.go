package mediatools

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// oggOpus builds a tiny Ogg Opus stream lasting secs seconds.
func oggOpus(secs int) []byte {
	page := func(granule uint64, payload []byte) []byte {
		h := make([]byte, 27, 28+len(payload))
		copy(h, "OggS")
		binary.LittleEndian.PutUint64(h[6:], granule)
		h[26] = 1
		h = append(h, byte(len(payload)))
		return append(h, payload...)
	}
	head := []byte("OpusHead")
	head = append(head, 1, 1)
	head = binary.LittleEndian.AppendUint16(head, 312) // pre-skip
	head = binary.LittleEndian.AppendUint32(head, 48000)
	head = append(head, 0, 0, 0)
	out := page(0, head)
	return append(out, page(uint64(secs*48000+312), []byte("audio"))...)
}

func TestOpenAISpeech(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/speech" || r.Header.Get("Authorization") != "Bearer sk-test" {
			http.Error(w, "bad request "+r.URL.Path, 400)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Write(oggOpus(2))
	}))
	defer srv.Close()
	s := &Speech{Provider: "openai", Key: "sk-test", BaseURL: srv.URL + "/v1", Voice: "coral", Format: "opus", Instructions: "warm"}
	a, err := s.Synthesize(context.Background(), "Hello there", "")
	if err != nil {
		t.Fatal(err)
	}
	if a.MediaType != "audio/ogg" || a.Ext != ".ogg" || !IsOggOpus(a.Data) {
		t.Fatalf("audio: %s %s", a.MediaType, a.Ext)
	}
	if got["model"] != "gpt-4o-mini-tts" || got["input"] != "Hello there" || got["voice"] != "coral" ||
		got["response_format"] != "opus" || got["instructions"] != "warm" {
		t.Fatalf("request: %v", got)
	}
	if _, err := s.Synthesize(context.Background(), "x", "nova"); err != nil || got["voice"] != "nova" {
		t.Fatalf("voice override: %v %v", err, got["voice"])
	}
}

func TestSpeechLimitsAndErrors(t *testing.T) {
	s := &Speech{Provider: "openai", Key: "k", MaxChars: 5}
	if _, err := s.Synthesize(context.Background(), "far too long", ""); !errors.Is(err, ErrTooLong) {
		t.Fatalf("want ErrTooLong, got %v", err)
	}
	s = &Speech{Provider: "openai", KeyEnv: "OPENAI_API_KEY"}
	if _, err := s.Synthesize(context.Background(), "hi", ""); err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Fatalf("missing key: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"quota"}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()
	s = &Speech{Provider: "openai", Key: "k", BaseURL: srv.URL}
	if _, err := s.Synthesize(context.Background(), "hi", ""); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("HTTP error: %v", err)
	}
}

func TestElevenLabsSpeech(t *testing.T) {
	var path, format, key string
	var body map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, format, key = r.URL.Path, r.URL.Query().Get("output_format"), r.Header.Get("xi-api-key")
		json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte("ID3\x03\x00fake mp3"))
	}))
	defer srv.Close()
	s := &Speech{Provider: "elevenlabs", Key: "xi", BaseURL: srv.URL, Voice: "VOICE1", Format: "mp3"}
	a, err := s.Synthesize(context.Background(), "Good morning", "")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/v1/text-to-speech/VOICE1" || format != "mp3_44100_128" || key != "xi" {
		t.Fatalf("request: %s %s %s", path, format, key)
	}
	if body["text"] != "Good morning" || body["model_id"] != "eleven_multilingual_v2" {
		t.Fatalf("body: %v", body)
	}
	if a.MediaType != "audio/mpeg" || a.Ext != ".mp3" {
		t.Fatalf("audio: %+v", a.MediaType)
	}
	s.Format = "opus"
	if _, err := s.Synthesize(context.Background(), "x", ""); err != nil || format != "opus_48000_64" {
		t.Fatalf("opus format: %v %s", err, format)
	}
}

// edgeServer fakes the read-aloud websocket service. When refuseFirst is
// set it answers the first handshake 403 with its own clock in Date.
type edgeServer struct {
	t           *testing.T
	now         time.Time // the server's clock
	refuseFirst bool

	mu       sync.Mutex
	dials    int
	gecs     []string
	messages []string
	headers  http.Header
}

func (e *edgeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	e.dials++
	first := e.dials == 1
	e.gecs = append(e.gecs, r.URL.Query().Get("Sec-MS-GEC"))
	e.headers = r.Header.Clone()
	e.mu.Unlock()
	if r.URL.Query().Get("TrustedClientToken") != edgeToken || r.URL.Query().Get("Sec-MS-GEC-Version") != edgeGECVersion ||
		r.URL.Query().Get("ConnectionId") == "" {
		http.Error(w, "bad query", 400)
		return
	}
	if r.URL.Query().Get("Sec-MS-GEC") != edgeGEC(e.now) || (e.refuseFirst && first) {
		w.Header().Set("Date", e.now.UTC().Format(http.TimeFormat))
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true}) // the client sends the extension origin
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx := r.Context()
	for range 2 {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		e.mu.Lock()
		e.messages = append(e.messages, string(data))
		e.mu.Unlock()
	}
	c.Write(ctx, websocket.MessageText, []byte("X-RequestId:1\r\nPath:turn.start\r\n\r\n{}"))
	for _, chunk := range []string{"MP3-", "DATA"} {
		header := "X-RequestId:1\r\nContent-Type:audio/mpeg\r\nPath:audio\r\n"
		frame := binary.BigEndian.AppendUint16(nil, uint16(len(header)))
		frame = append(append(frame, header...), chunk...)
		c.Write(ctx, websocket.MessageBinary, frame)
	}
	c.Write(ctx, websocket.MessageText, []byte("X-RequestId:1\r\nPath:audio.metadata\r\n\r\n{}"))
	c.Write(ctx, websocket.MessageText, []byte("X-RequestId:1\r\nPath:turn.end\r\n\r\n{}"))
	c.Read(ctx) // until the client closes
}

func TestEdgeSpeech(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	es := &edgeServer{t: t, now: now}
	srv := httptest.NewServer(es)
	defer srv.Close()
	s := &Speech{Provider: "edge", BaseURL: "ws" + strings.TrimPrefix(srv.URL, "http"), Now: func() time.Time { return now }}
	a, err := s.Synthesize(context.Background(), "Fish & chips <now>", "en-GB-SoniaNeural")
	if err != nil {
		t.Fatal(err)
	}
	if string(a.Data) != "MP3-DATA" || a.MediaType != "audio/mpeg" {
		t.Fatalf("audio %q %s", a.Data, a.MediaType)
	}
	es.mu.Lock()
	defer es.mu.Unlock()
	if len(es.messages) != 2 {
		t.Fatalf("messages: %d", len(es.messages))
	}
	cfg, ssml := es.messages[0], es.messages[1]
	if !strings.Contains(cfg, "Path:speech.config") || !strings.Contains(cfg, `"outputFormat":"audio-24khz-48kbitrate-mono-mp3"`) ||
		!strings.Contains(cfg, "X-Timestamp:Sat Nov 07 2026 12:00:00 GMT+0000 (Coordinated Universal Time)\r\n") {
		t.Fatalf("config message: %q", cfg)
	}
	if !strings.Contains(ssml, "Path:ssml\r\n\r\n<speak") || !strings.Contains(ssml, "Content-Type:application/ssml+xml") ||
		!strings.Contains(ssml, "<voice name='Microsoft Server Speech Text to Speech Voice (en-GB, SoniaNeural)'>") ||
		!strings.Contains(ssml, "Fish &amp; chips &lt;now&gt;") || !strings.Contains(ssml, "(Coordinated Universal Time)Z\r\n") {
		t.Fatalf("ssml message: %q", ssml)
	}
	if es.headers.Get("Origin") != edgeOrigin || !strings.HasPrefix(es.headers.Get("Cookie"), "muid=") {
		t.Fatalf("headers: %v", es.headers)
	}
}

func TestEdgeRetriesWithServerClock(t *testing.T) {
	server := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	es := &edgeServer{t: t, now: server}
	srv := httptest.NewServer(es)
	defer srv.Close()
	local := server.Add(-17 * time.Minute) // a slow clock gets a stale token
	s := &Speech{Provider: "edge", BaseURL: "ws" + strings.TrimPrefix(srv.URL, "http"), Now: func() time.Time { return local }}
	if _, err := s.Synthesize(context.Background(), "hello", ""); err != nil {
		t.Fatal(err)
	}
	es.mu.Lock()
	defer es.mu.Unlock()
	if es.dials != 2 || es.gecs[1] != edgeGEC(server) {
		t.Fatalf("dials %d, tokens %v", es.dials, es.gecs)
	}
	if !strings.Contains(es.messages[1], "Microsoft Server Speech Text to Speech Voice (en-US, AriaNeural)") {
		t.Fatalf("default voice: %q", es.messages[1])
	}
}

func TestEdgeHelpers(t *testing.T) {
	if got := edgeVoiceName("zh-CN-liaoning-XiaobeiNeural"); got != "Microsoft Server Speech Text to Speech Voice (zh-CN-liaoning, XiaobeiNeural)" {
		t.Fatal(got)
	}
	// Regression value for 2024-01-01T00:00Z (file time 133485408000000000).
	// The algorithm was checked against the live service, which refuses a
	// wrong token with 403.
	if got := edgeGEC(time.Date(2024, 1, 1, 0, 2, 0, 0, time.UTC)); got != "9586E5FBB4BD2CFF-DE636BA8369C7431ED826BDEEB00ECA1CC52826A6BB736DCB150A2AF37DFC498" {
		t.Fatalf("token %s", got)
	}
	if edgeGEC(time.Date(2024, 1, 1, 0, 5, 0, 0, time.UTC)) == edgeGEC(time.Date(2024, 1, 1, 0, 4, 59, 0, time.UTC)) {
		t.Fatal("tokens across a five-minute boundary match")
	}
	text := strings.Repeat("word &amp; ", 1000)
	parts := splitSpeech(text, 300)
	if len(parts) < 30 {
		t.Fatalf("split into %d parts", len(parts))
	}
	for _, p := range parts {
		if len(p) > 300 || strings.HasSuffix(p, "&") || strings.HasPrefix(p, "amp;") {
			t.Fatalf("bad part %q", p)
		}
	}
	if got := strings.Join(parts, " "); strings.Join(strings.Fields(got), " ") != strings.TrimSpace(strings.Join(strings.Fields(text), " ")) {
		t.Fatal("splitting lost text")
	}
	if edgeEscape("a\x01b") != "a b" {
		t.Fatal("control characters kept")
	}
}

func TestSpeechText(t *testing.T) {
	md := "# Plan\n\n**Bold** move: see [the docs](https://x.y/z) or https://example.com.\n\n```go\nfmt.Println()\n```\n- one\n- `two`\n> quoted\n\n| a | b |\n|---|---|\n| 1 | 2 |"
	got := SpeechText(md)
	for _, bad := range []string{"#", "**", "https", "fmt.Println", "`", "](", "---"} {
		if strings.Contains(got, bad) {
			t.Fatalf("%q left in %q", bad, got)
		}
	}
	for _, want := range []string{"Plan", "Bold move", "the docs", "one", "two", "quoted"} {
		if !strings.Contains(got, want) {
			t.Fatalf("%q missing from %q", want, got)
		}
	}
}

func TestOggHelpers(t *testing.T) {
	data := oggOpus(3)
	if !IsOggOpus(data) || IsOggOpus([]byte("ID3 mp3")) {
		t.Fatal("IsOggOpus")
	}
	if d := OggDuration(data); d != 3*time.Second {
		t.Fatalf("duration %v", d)
	}
	if OggDuration([]byte("nope")) != 0 {
		t.Fatal("duration of junk")
	}
}

func TestVoiceNoteWithoutFFmpeg(t *testing.T) {
	old := lookFFmpeg
	lookFFmpeg = func() (string, error) { return "", errors.New("not found") }
	defer func() { lookFFmpeg = old }()
	mp3 := Audio{Data: []byte("ID3 mp3"), MediaType: "audio/mpeg", Ext: ".mp3"}
	out, ok, err := VoiceNote(context.Background(), mp3)
	if ok || err != nil || out.MediaType != "audio/mpeg" {
		t.Fatalf("mp3 without ffmpeg: %v %v %+v", ok, err, out.MediaType)
	}
	ogg := Audio{Data: oggOpus(1), MediaType: "audio/ogg", Ext: ".ogg"}
	if _, ok, _ := VoiceNote(context.Background(), ogg); !ok {
		t.Fatal("ogg needs no conversion")
	}
}

func TestVoiceNoteConvertsWithFFmpeg(t *testing.T) {
	if _, err := lookFFmpeg(); err != nil {
		t.Skip("ffmpeg not installed")
	}
	// One second of a 440 Hz tone as 16-bit mono WAV.
	const rate = 16000
	var pcm []byte
	for i := range rate {
		v := int16(8000 * sinApprox(float64(i)*440/rate))
		pcm = binary.LittleEndian.AppendUint16(pcm, uint16(v))
	}
	wav := []byte("RIFF")
	wav = binary.LittleEndian.AppendUint32(wav, uint32(36+len(pcm)))
	wav = append(wav, "WAVEfmt "...)
	wav = binary.LittleEndian.AppendUint32(wav, 16)
	wav = binary.LittleEndian.AppendUint16(wav, 1)
	wav = binary.LittleEndian.AppendUint16(wav, 1)
	wav = binary.LittleEndian.AppendUint32(wav, rate)
	wav = binary.LittleEndian.AppendUint32(wav, rate*2)
	wav = binary.LittleEndian.AppendUint16(wav, 2)
	wav = binary.LittleEndian.AppendUint16(wav, 16)
	wav = append(wav, "data"...)
	wav = binary.LittleEndian.AppendUint32(wav, uint32(len(pcm)))
	wav = append(wav, pcm...)
	out, ok, err := VoiceNote(context.Background(), Audio{Data: wav, MediaType: "audio/wav", Ext: ".wav"})
	if err != nil || !ok || out.MediaType != "audio/ogg" || !IsOggOpus(out.Data) {
		t.Fatalf("convert: %v %v %s", ok, err, out.MediaType)
	}
	if d := OggDuration(out.Data); d < 900*time.Millisecond || d > 1100*time.Millisecond {
		t.Fatalf("converted duration %v", d)
	}
}

// sinApprox is a sine good enough for a test tone.
func sinApprox(cycles float64) float64 {
	x := cycles - float64(int(cycles))
	if x < 0.5 {
		return 1 - 16*(x-0.25)*(x-0.25)
	}
	return 16*(x-0.75)*(x-0.75) - 1
}
