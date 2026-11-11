package whatsappagent

import (
	"encoding/json"
	"testing"
)

// Text is Plain; a file or a voice note is not.
func TestPlainOnlyForText(t *testing.T) {
	a := New(Config{})
	var text message
	if err := json.Unmarshal([]byte(`{"from":"user:9","id":"t","type":"text","text":{"body":"run it"}}`), &text); err != nil {
		t.Fatal(err)
	}
	if m, ok := a.convert(&text); !ok || !m.Plain {
		t.Fatalf("text: %+v", m)
	}
	if m, ok := a.convert(&message{From: "user:9", ID: "v", Type: "audio", Audio: &media{ID: "A", Voice: true}}); ok && m.Plain {
		t.Fatalf("voice note: %+v", m)
	}
}
