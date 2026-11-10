package whatsapp

import (
	"context"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func TestFileMessage(t *testing.T) {
	var kinds []whatsmeow.MediaType
	upload := func(_ context.Context, data []byte, kind whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
		kinds = append(kinds, kind)
		return whatsmeow.UploadResponse{URL: "https://mmg/x", DirectPath: "/v/x", MediaKey: []byte("k"),
			FileSHA256: []byte("s"), FileEncSHA256: []byte("e"), FileLength: uint64(len(data))}, nil
	}
	ctx := context.Background()

	img, err := fileMessage(ctx, upload, "chart.png", "image/png", []byte("PNG"), "*Q3*")
	if err != nil {
		t.Fatal(err)
	}
	im := img.GetImageMessage()
	if im == nil || im.GetCaption() != "*Q3*" || im.GetMimetype() != "image/png" || im.GetDirectPath() != "/v/x" || im.GetFileLength() != 3 {
		t.Fatalf("image: %v", img)
	}

	doc, err := fileMessage(ctx, upload, "report.pdf", "application/pdf", []byte("%PDF"), "")
	if err != nil {
		t.Fatal(err)
	}
	dm := doc.GetDocumentMessage()
	if dm == nil || dm.GetFileName() != "report.pdf" || dm.Caption != nil || string(dm.GetMediaKey()) != "k" {
		t.Fatalf("document: %v", doc)
	}

	big := make([]byte, maxImage+1)
	if m, _ := fileMessage(ctx, upload, "huge.jpg", "image/jpeg", big, ""); m.GetDocumentMessage() == nil {
		t.Fatal("an image over the inline limit should go as a document")
	}
	if len(kinds) != 3 || kinds[0] != whatsmeow.MediaImage || kinds[1] != whatsmeow.MediaDocument || kinds[2] != whatsmeow.MediaDocument {
		t.Fatalf("upload kinds: %v", kinds)
	}
}

// lidMap is an in-memory stand-in for whatsmeow's LID store.
type lidMap map[types.JID]types.JID // LID -> phone number

func (l lidMap) PutManyLIDMappings(context.Context, []wastore.LIDMapping) error { return nil }
func (l lidMap) PutLIDMapping(context.Context, types.JID, types.JID) error      { return nil }
func (l lidMap) GetPNForLID(_ context.Context, lid types.JID) (types.JID, error) {
	return l[lid], nil
}
func (l lidMap) GetLIDForPN(_ context.Context, pn types.JID) (types.JID, error) {
	for lid, p := range l {
		if p == pn {
			return lid, nil
		}
	}
	return types.JID{}, nil
}
func (l lidMap) GetManyLIDsForPNs(context.Context, []types.JID) (map[types.JID]types.JID, error) {
	return nil, nil
}

func TestSenderMatchesPhoneNumberAndLID(t *testing.T) {
	lid := types.NewJID("98765432101234", types.HiddenUserServer)
	pn := types.NewJID("12025550142", types.DefaultUserServer)
	cli := &whatsmeow.Client{Store: &wastore.Device{LIDs: lidMap{lid: pn}}}
	msg := func(sender, alt types.JID) *events.Message {
		return &events.Message{
			Info:    types.MessageInfo{MessageSource: types.MessageSource{Chat: sender, Sender: sender, SenderAlt: alt}, ID: "M1"},
			Message: &waE2E.Message{Conversation: proto.String("hi")},
		}
	}
	cases := []struct {
		name        string
		sender, alt types.JID
	}{
		{"lid only, phone number from the store", lid, types.JID{}},
		{"lid with the phone number alongside", lid, pn},
		{"phone number only, lid from the store", pn, types.JID{}},
	}
	for _, c := range cases {
		m, ok := convert(cli, msg(c.sender, c.alt))
		if !ok || m.Sender != "12025550142" || m.SenderAlt != "98765432101234" {
			t.Errorf("%s: sender %q alt %q", c.name, m.Sender, m.SenderAlt)
		}
	}
	// No mapping known: the LID is all there is, and allow_from may list it.
	other := types.NewJID("99999999999999", types.HiddenUserServer)
	if m, _ := convert(cli, msg(other, types.JID{})); m.Sender != "99999999999999" || m.SenderAlt != "" {
		t.Errorf("unmapped lid: sender %q alt %q", m.Sender, m.SenderAlt)
	}
}
