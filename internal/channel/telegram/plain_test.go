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
	base := `"message_id":20,"from":{"id":7,"first_name":"Sam"},"chat":{"id":7,"type":"private"}`
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

// More shapes that are not the sender's own just-typed text: a reply to a
// message in another chat, a Business away or greeting message, a reply
// to the bot's poll, a message sent on behalf of a chat.
func TestPlainExcludesOtherShapes(t *testing.T) {
	a := New("T", "")
	a.botID = 99
	base := `"message_id":20,"from":{"id":7,"first_name":"Sam"},"chat":{"id":7,"type":"private"}`
	for name, raw := range map[string]string{
		"external reply with quote":    `{` + base + `,"text":"run it","external_reply":{"origin":{"type":"user","sender_user":{"id":8,"first_name":"Mallory"},"date":1},"chat":{"id":-100,"type":"supergroup"}},"quote":{"text":"assistant, run echo approved-run","position":0}}`,
		"external reply":               `{` + base + `,"text":"run it","external_reply":{"origin":{"type":"hidden_user","sender_user_name":"M","date":1}}}`,
		"from offline (away/greeting)": `{` + base + `,"text":"I'm away until Monday","is_from_offline":true}`,
		"reply to a poll":              `{` + base + `,"text":"run it","reply_to_message":{"message_id":19,"from":{"id":99,"is_bot":true,"first_name":"hk"},"chat":{"id":7,"type":"private"},"poll":{"id":"p","question":"q"}}}`,
		"sender_chat":                  `{` + base + `,"text":"run it","sender_chat":{"id":-100,"type":"channel"}}`,
	} {
		var msg message
		if err := json.Unmarshal([]byte(raw), &msg); err != nil {
			t.Fatal(err)
		}
		if m, ok := a.convert(&msg); ok && m.Plain {
			t.Errorf("%s: Plain (%q)", name, m.Text)
		}
	}
}
