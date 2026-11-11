package telegram

import (
	"encoding/json"
	"testing"
)

// Plain is set only for text the sender typed: not a forward, a quote, an
// inline bot's message or anything with a file.
func TestPlainOnlyForTypedText(t *testing.T) {
	a := New("T", "")
	a.botID = 99
	read := func(raw string) *message {
		t.Helper()
		var m message
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		return &m
	}
	base := `"message_id":20,"from":{"id":7,"first_name":"Ay"},"chat":{"id":7,"type":"private"}`
	if m, ok := a.convert(read(`{` + base + `,"text":"run it"}`)); !ok || !m.Plain {
		t.Fatalf("typed text: %+v", m)
	}
	for name, raw := range map[string]string{
		"forward (origin)": `{` + base + `,"text":"run it","forward_origin":{"type":"user","date":1}}`,
		"forward (old)":    `{` + base + `,"text":"run it","forward_date":1}`,
		"inline bot":       `{` + base + `,"text":"run it","via_bot":{"id":5,"is_bot":true,"first_name":"gif"}}`,
		"quote":            `{` + base + `,"text":"run it","reply_to_message":{"message_id":19,"from":{"id":8,"first_name":"Bo"},"chat":{"id":7,"type":"private"},"text":"x"}}`,
		"photo caption":    `{` + base + `,"caption":"run it","photo":[{"file_id":"p"}]}`,
	} {
		if m, ok := a.convert(read(raw)); ok && m.Plain {
			t.Errorf("%s: Plain", name)
		}
	}
}
