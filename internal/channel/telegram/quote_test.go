package telegram

import (
	"encoding/json"
	"testing"
)

func TestConvertQuotedReply(t *testing.T) {
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

	m, ok := a.convert(read(`{"message_id":20,"from":{"id":7,"first_name":"Ay"},"chat":{"id":7,"type":"private"},"text":"yes",
		"reply_to_message":{"message_id":19,"from":{"id":99,"is_bot":true,"first_name":"Habakkuk"},"chat":{"id":7,"type":"private"},"text":"Tests pass. Merge it?"}}`))
	if !ok || m.Text != "yes" || m.Quote == nil || !m.Quote.Self || m.Quote.Text != "Tests pass. Merge it?" {
		t.Fatalf("reply to the bot: %+v %+v", m, m.Quote)
	}

	// The part the user picked wins over the whole message.
	m, _ = a.convert(read(`{"message_id":21,"from":{"id":7,"first_name":"Ay"},"chat":{"id":7,"type":"private"},"text":"this part",
		"quote":{"text":"Merge it?"},
		"reply_to_message":{"message_id":19,"from":{"id":99,"is_bot":true},"chat":{"id":7,"type":"private"},"text":"Tests pass. Merge it?"}}`))
	if m.Quote == nil || m.Quote.Text != "Merge it?" {
		t.Fatalf("partial quote: %+v", m.Quote)
	}

	m, _ = a.convert(read(`{"message_id":22,"from":{"id":8,"first_name":"Bo"},"chat":{"id":-5,"type":"group"},"text":"nice",
		"reply_to_message":{"message_id":3,"from":{"id":7,"first_name":"Ay","last_name":"O"},"chat":{"id":-5,"type":"group"},"photo":[{"file_id":"p"}]}}`))
	if m.Quote == nil || m.Quote.Self || m.Quote.From != "Ay O" || m.Quote.Text != "(photo)" {
		t.Fatalf("reply to a photo: %+v", m.Quote)
	}

	// In a forum topic the topic's first message is not a quote.
	m, _ = a.convert(read(`{"message_id":30,"message_thread_id":25,"is_topic_message":true,"from":{"id":7,"first_name":"Ay"},
		"chat":{"id":-9,"type":"supergroup"},"text":"hi",
		"reply_to_message":{"message_id":25,"from":{"id":7},"chat":{"id":-9,"type":"supergroup"},"text":"Topic"}}`))
	if m.Quote != nil {
		t.Fatalf("topic root taken as a quote: %+v", m.Quote)
	}
}
