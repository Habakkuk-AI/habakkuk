package whatsapp

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func incoming(msg *waE2E.Message) *events.Message {
	user := types.NewJID("15550002", types.DefaultUserServer)
	return &events.Message{Info: types.MessageInfo{
		MessageSource: types.MessageSource{Chat: user, Sender: user},
		ID:            "MSG1", PushName: "Ay", Timestamp: time.Now(),
	}, Message: msg}
}

// A reply quoting the bot's message, or the user's own, carries the quoted
// text; one without a quote carries none.
func TestConvertQuotedReply(t *testing.T) {
	bot := types.NewJID("15550001", types.DefaultUserServer)
	cli := &whatsmeow.Client{Store: &store.Device{ID: &bot}}

	reply := func(participant string, quoted *waE2E.Message) *waE2E.Message {
		return &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String("yes"),
			ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("OLD1"), Participant: proto.String(participant),
				QuotedMessage: quoted},
		}}
	}
	m, ok := convert(cli, incoming(reply("15550001@s.whatsapp.net", &waE2E.Message{Conversation: proto.String("Merge it?")})))
	if !ok || m.Text != "yes" || m.Quote == nil || !m.Quote.Self || m.Quote.Text != "Merge it?" {
		t.Fatalf("reply to the bot: %+v %+v", m, m.Quote)
	}

	m, _ = convert(cli, incoming(reply("15550002@s.whatsapp.net", &waE2E.Message{ImageMessage: &waE2E.ImageMessage{}})))
	if m.Quote == nil || m.Quote.Self || m.Quote.From != "Ay" || m.Quote.Text != "(photo)" {
		t.Fatalf("reply to own photo: %+v", m.Quote)
	}

	// A photo sent as a reply carries the context on the image.
	photo := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("this one"),
		ContextInfo: &waE2E.ContextInfo{Participant: proto.String("15550001@s.whatsapp.net"),
			QuotedMessage: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("send me the chart")}}}}}
	m, _ = convert(cli, incoming(photo))
	if m.Text != "this one" || m.Quote == nil || !m.Quote.Self || m.Quote.Text != "send me the chart" {
		t.Fatalf("photo reply: %+v %+v", m, m.Quote)
	}

	m, _ = convert(cli, incoming(&waE2E.Message{Conversation: proto.String("plain")}))
	if m.Quote != nil {
		t.Fatalf("plain message got a quote: %+v", m.Quote)
	}
}
