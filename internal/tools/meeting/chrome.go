package meeting

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/habakkuk-ai/habakkuk/internal/tools/browser"
)

// ChromeLauncher starts a Chrome of its own for each attendance through
// internal/tools/browser: a fresh profile in the attendance's directory, the
// URL guard, and fake media devices. The browser grants camera and
// microphone without asking, and what they send is silence and a black
// picture (files written next to the profile), so nothing leaks even if a
// platform turns them on; Habakkuk also mutes both before joining.
func ChromeLauncher(path string, headless bool, guard *browser.Guard, log *slog.Logger) Launcher {
	return func(ctx context.Context, rec *Record, dir string) (Page, error) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		wav, y4m := filepath.Join(dir, "silence.wav"), filepath.Join(dir, "black.y4m")
		if err := os.WriteFile(wav, silentWAV(), 0o600); err != nil {
			return nil, err
		}
		if err := os.WriteFile(y4m, blackY4M(320, 180), 0o600); err != nil {
			return nil, err
		}
		d, err := browser.LaunchDedicated(ctx, browser.DedicatedOptions{
			Path: path, Profile: filepath.Join(dir, "profile"), Headless: headless, Guard: guard, Log: log,
			Args: []string{
				"--use-fake-ui-for-media-stream",
				"--use-fake-device-for-media-stream",
				"--use-file-for-fake-audio-capture=" + wav,
				"--use-file-for-fake-video-capture=" + y4m,
				"--autoplay-policy=no-user-gesture-required",
				"--disable-notifications",
			},
		})
		if err != nil {
			return nil, err
		}
		return d, nil
	}
}

// silentWAV is one second of 16-bit mono silence at 48 kHz.
func silentWAV() []byte {
	const rate, samples = 48000, 48000
	var b bytes.Buffer
	data := samples * 2
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+data))
	b.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(1), uint32(rate), uint32(rate * 2), uint16(2), uint16(16)} {
		_ = binary.Write(&b, binary.LittleEndian, v)
	}
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(data))
	b.Write(make([]byte, data))
	return b.Bytes()
}

// blackY4M is one black frame in the YUV4MPEG2 format Chrome loops as a
// fake camera.
func blackY4M(w, h int) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "YUV4MPEG2 W%d H%d F30:1 Ip A1:1 C420jpeg\nFRAME\n", w, h)
	y := bytes.Repeat([]byte{16}, w*h)
	uv := bytes.Repeat([]byte{128}, (w/2)*(h/2))
	b.Write(y)
	b.Write(uv)
	b.Write(uv)
	return b.Bytes()
}
