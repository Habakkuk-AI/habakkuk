package whatsappagent

import "testing"

// Text is Plain; a file or a voice note is not.
func TestPlainOnlyForText(t *testing.T) {
	a := New(Config{})
	if m, ok := a.convert(&message{From: "user:9", ID: "t", Type: "text", Text: &struct {
		Body string `json:"body"`
	}{Body: "run it"}}); !ok || !m.Plain {
		t.Fatalf("text: %+v", m)
	}
	if m, ok := a.convert(&message{From: "user:9", ID: "v", Type: "audio", Audio: &media{ID: "A", Voice: true}}); ok && m.Plain {
		t.Fatalf("voice note: %+v", m)
	}
}
