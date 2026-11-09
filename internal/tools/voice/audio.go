package voice

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Capture is how ffmpeg reads the microphone.
type Capture struct {
	Input  string // avfoundation | pulse | alsa; "" picks one for the OS
	Device string // "" is the default device
}

// CaptureCommand is the ffmpeg command that streams the microphone to
// stdout as raw PCM in the package's format. goos is runtime.GOOS; have
// reports whether a program is installed (it picks pulse over alsa).
func CaptureCommand(goos string, c Capture, have func(string) bool) (string, []string, error) {
	input := c.Input
	if input == "" {
		switch goos {
		case "darwin":
			input = "avfoundation"
		case "linux":
			// PulseAudio, and PipeWire through its pulse server, mix the mic
			// for every program; raw ALSA is for a bare Raspberry Pi.
			input = "alsa"
			if have("pactl") || have("pipewire-pulse") {
				input = "pulse"
			}
		default:
			return "", nil, fmt.Errorf("talk: no default microphone input on %s; set talk.input (avfoundation, pulse or alsa)", goos)
		}
	}
	dev := c.Device
	switch input {
	case "avfoundation":
		if dev == "" {
			dev = ":0" // no video, the first audio device
		} else if !strings.Contains(dev, ":") {
			dev = ":" + dev
		}
	case "pulse", "alsa":
		if dev == "" {
			dev = "default"
		}
	default:
		return "", nil, fmt.Errorf("talk: input %q must be avfoundation, pulse or alsa", input)
	}
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-f", input, "-i", dev,
		"-ac", "1", "-ar", fmt.Sprint(SampleRate), "-f", "s16le", "-acodec", "pcm_s16le", "-"}
	return "ffmpeg", args, nil
}

// PlayerCommand is the command that plays an audio file, and whether the
// file must be WAV first (afplay, paplay and aplay may not read Ogg Opus or
// MP3; ffplay reads anything). player forces a program.
func PlayerCommand(goos, player string, have func(string) bool, path string) (name string, args []string, wantWAV bool, err error) {
	if player == "" {
		order := []string{"ffplay", "paplay", "aplay"}
		if goos == "darwin" {
			order = []string{"afplay", "ffplay"}
		}
		for _, p := range order {
			if have(p) {
				player = p
				break
			}
		}
	}
	switch player {
	case "afplay":
		return "afplay", []string{path}, true, nil
	case "paplay":
		return "paplay", []string{path}, true, nil
	case "aplay":
		return "aplay", []string{"-q", path}, true, nil
	case "ffplay":
		return "ffplay", []string{"-nodisp", "-autoexit", "-loglevel", "quiet", path}, false, nil
	case "":
		return "", nil, false, errors.New("talk: no audio player found; install ffmpeg (ffplay), or use afplay, paplay or aplay")
	}
	return "", nil, false, fmt.Errorf("talk: player %q must be afplay, paplay, aplay or ffplay", player)
}

// ConvertCommand is the ffmpeg command that turns any audio file into WAV.
func ConvertCommand(in, out string) (string, []string) {
	return "ffmpeg", []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-i", in, out}
}

// WAV wraps PCM in the package's format in a WAV header.
func WAV(pcm []byte) []byte {
	var b bytes.Buffer
	w := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	b.WriteString("RIFF")
	w(uint32(36 + len(pcm)))
	b.WriteString("WAVEfmt ")
	w(uint32(16))
	w(uint16(1)) // PCM
	w(uint16(1)) // mono
	w(uint32(SampleRate))
	w(uint32(SampleRate * 2))
	w(uint16(2))
	w(uint16(16))
	b.WriteString("data")
	w(uint32(len(pcm)))
	b.Write(pcm)
	return b.Bytes()
}

// Tone is a short sine beep as PCM, the cue that a wake phrase was heard.
func Tone(hz float64, d int) []byte {
	n := SampleRate * d / 1000
	out := make([]byte, 2*n)
	for i := range n {
		env := math.Min(1, math.Min(float64(i), float64(n-i))/400) // soft edges, no click
		v := int16(0.25 * env * 32767 * math.Sin(2*math.Pi*hz*float64(i)/SampleRate))
		binary.LittleEndian.PutUint16(out[2*i:], uint16(v))
	}
	return out
}

// Device runs the external programs: the microphone stream and playback.
type Device struct {
	GOOS    string
	Capture Capture
	Player  string
	Dir     string // where audio files to play are written
	Have    func(string) bool
}

// HaveProgram reports whether name is on PATH.
func HaveProgram(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// Listen starts ffmpeg and returns its PCM stream; closing it stops ffmpeg.
func (d *Device) Listen(ctx context.Context) (io.ReadCloser, error) {
	name, args, err := CaptureCommand(d.GOOS, d.Capture, d.Have)
	if err != nil {
		return nil, err
	}
	if !d.Have(name) {
		return nil, errors.New("talk: ffmpeg is not installed (brew install ffmpeg, or apt install ffmpeg)")
	}
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &proc{ReadCloser: out, cmd: cmd, stderr: &stderr}, nil
}

type proc struct {
	io.ReadCloser
	cmd    *exec.Cmd
	stderr *bytes.Buffer
}

func (p *proc) Read(b []byte) (int, error) {
	n, err := p.ReadCloser.Read(b)
	if errors.Is(err, io.EOF) && p.stderr.Len() > 0 {
		return n, fmt.Errorf("ffmpeg: %s", strings.TrimSpace(p.stderr.String()))
	}
	return n, err
}

func (p *proc) Close() error {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	_ = p.ReadCloser.Close()
	_ = p.cmd.Wait()
	return nil
}

// Play plays audio (ext is its file extension, e.g. ".ogg") until it ends
// or ctx is canceled, which stops it at once (barge-in).
func (d *Device) Play(ctx context.Context, data []byte, ext string) error {
	f, err := os.CreateTemp(d.Dir, "habakkuk-talk-*"+ext)
	if err != nil {
		return err
	}
	path := f.Name()
	defer os.Remove(path)
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	name, args, wantWAV, err := PlayerCommand(d.GOOS, d.Player, d.Have, path)
	if err != nil {
		return err
	}
	if wantWAV && ext != ".wav" {
		wav := strings.TrimSuffix(path, filepath.Ext(path)) + ".wav"
		defer os.Remove(wav)
		cn, ca := ConvertCommand(path, wav)
		if out, err := exec.CommandContext(ctx, cn, ca...).CombinedOutput(); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("convert reply to wav: %v: %s", err, strings.TrimSpace(string(out)))
		}
		name, args, _, _ = PlayerCommand(d.GOOS, name, d.Have, wav)
	}
	err = exec.CommandContext(ctx, name, args...).Run()
	if ctx.Err() != nil {
		return nil // stopped on purpose
	}
	return err
}
