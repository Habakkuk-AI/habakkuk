package whatsapp

import (
	"context"
	"testing"

	"go.mau.fi/whatsmeow"
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
