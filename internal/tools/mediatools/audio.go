package mediatools

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// lookFFmpeg finds ffmpeg; tests replace it.
var lookFFmpeg = func() (string, error) { return exec.LookPath("ffmpeg") }

// IsOggOpus reports whether data is an Ogg stream carrying Opus, the format
// Telegram and WhatsApp play as voice notes.
func IsOggOpus(data []byte) bool {
	if !bytes.HasPrefix(data, []byte("OggS")) || len(data) < 28 {
		return false
	}
	head := data[:min(len(data), 128)]
	return bytes.Contains(head, []byte("OpusHead"))
}

// VoiceNote returns audio as Ogg Opus, converting it with ffmpeg when it is
// something else and ffmpeg is installed. ok is false when the audio could
// not be made a voice note; it is then returned unchanged, to be sent as an
// ordinary audio file.
func VoiceNote(ctx context.Context, a Audio) (out Audio, ok bool, err error) {
	if IsOggOpus(a.Data) {
		return a, true, nil
	}
	ffmpeg, lerr := lookFFmpeg()
	if lerr != nil {
		return a, false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-loglevel", "error", "-i", "pipe:0",
		"-vn", "-map_metadata", "-1", "-c:a", "libopus", "-b:a", "32k", "-ar", "48000", "-ac", "1",
		"-application", "voip", "-f", "ogg", "pipe:1")
	cmd.Stdin = bytes.NewReader(a.Data)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return a, false, fmt.Errorf("ffmpeg: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if !IsOggOpus(stdout.Bytes()) {
		return a, false, fmt.Errorf("ffmpeg produced no Ogg Opus")
	}
	return Audio{Data: stdout.Bytes(), MediaType: "audio/ogg", Ext: ".ogg"}, true, nil
}

// OggDuration is the length of an Ogg Opus stream, from the granule position
// of its last page less the encoder's pre-skip (Opus always counts 48 kHz
// samples). It is 0 when the stream cannot be read.
func OggDuration(data []byte) time.Duration {
	if !IsOggOpus(data) {
		return 0
	}
	var preSkip uint64
	if i := bytes.Index(data, []byte("OpusHead")); i >= 0 && i+12 <= len(data) {
		preSkip = uint64(binary.LittleEndian.Uint16(data[i+10 : i+12]))
	}
	last := bytes.LastIndex(data, []byte("OggS"))
	for last >= 0 {
		if last+14 <= len(data) {
			granule := binary.LittleEndian.Uint64(data[last+6 : last+14])
			if granule != ^uint64(0) && granule > preSkip {
				return time.Duration(granule-preSkip) * time.Second / 48000
			}
		}
		last = bytes.LastIndex(data[:last], []byte("OggS"))
	}
	return 0
}
