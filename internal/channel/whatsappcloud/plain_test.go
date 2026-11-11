package whatsappcloud

import (
	"encoding/json"
	"testing"
)

// The Cloud API marks forwards and replies in "context": those aren't
// Plain, nor is a template button's text. Plain typed text is.
func TestPlainOnlyForTypedText(t *testing.T) {
	a := New(Config{})
	read := func(raw string) *message {
		t.Helper()
		var m message
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		return &m
	}
	if m, ok := a.convert(read(`{"from":"15550000001","id":"w1","type":"text","text":{"body":"run it"}}`)); !ok || !m.Plain {
		t.Fatalf("typed text: %+v", m)
	}
	m, ok := a.convert(read(`{"from":"15550000001","id":"w2","type":"text","context":{"forwarded":true,"frequently_forwarded":true},"text":{"body":"run it"}}`))
	if !ok || !m.Forwarded || m.Plain {
		t.Fatalf("forward: %+v", m)
	}
	for name, raw := range map[string]string{
		"reply":           `{"from":"15550000001","id":"w3","type":"text","context":{"from":"1555","id":"wamid.x"},"text":{"body":"run it"}}`,
		"template button": `{"from":"15550000001","id":"w4","type":"button","button":{"payload":"p","text":"run it"}}`,
	} {
		if m, ok := a.convert(read(raw)); ok && m.Plain {
			t.Errorf("%s: Plain", name)
		}
	}
}
