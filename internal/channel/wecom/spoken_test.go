package wecom

import "testing"

// A voice message with WeCom's own transcript is Spoken, never Plain.
func TestVoiceTranscriptIsSpoken(t *testing.T) {
	a := New(Config{})
	m, ok := a.convert(&message{FromUserName: "u1", MsgType: "voice", MediaID: "md1", Format: "amr",
		Recognition: "run echo approved-run", MsgID: "1"})
	if !ok || !m.Spoken || m.Plain {
		t.Fatalf("voice: %+v", m)
	}
}
