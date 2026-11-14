package discord

import (
	"encoding/json"
	"testing"
)

func TestConvertQuotedReply(t *testing.T) {
	a := New("T", "")
	a.botID = "99"
	read := func(raw string) *message {
		t.Helper()
		var m message
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		return &m
	}
	m, ok := a.convert(read(`{"id":"m9","channel_id":"D1","type":19,"author":{"id":"7","username":"sam"},"content":"yes",
		"referenced_message":{"author":{"id":"99","username":"habakkuk"},"content":"Merge it?"}}`))
	if !ok || m.Text != "yes" || m.Quote == nil || !m.Quote.Self || m.Quote.Text != "Merge it?" {
		t.Fatalf("reply to the bot: %+v %+v", m, m.Quote)
	}
	m, _ = a.convert(read(`{"id":"m10","channel_id":"D1","type":19,"author":{"id":"7","username":"sam"},"content":"nice",
		"referenced_message":{"author":{"id":"8","username":"bo","global_name":"Bo"},"content":"","attachments":[{}]}}`))
	if m.Quote == nil || m.Quote.Self || m.Quote.From != "Bo" || m.Quote.Text != "(attachment)" {
		t.Fatalf("reply to a file: %+v", m.Quote)
	}
	if m, _ = a.convert(read(`{"id":"m11","channel_id":"D1","type":0,"author":{"id":"7","username":"sam"},"content":"plain"}`)); m.Quote != nil {
		t.Fatalf("plain message got a quote: %+v", m.Quote)
	}
}
