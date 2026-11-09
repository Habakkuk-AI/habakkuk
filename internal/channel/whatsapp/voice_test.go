package whatsapp

import (
	"context"
	"testing"

	"go.mau.fi/whatsmeow"
)

func TestVoiceMessage(t *testing.T) {
	var kind whatsmeow.MediaType
	upload := func(_ context.Context, data []byte, k whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
		kind = k
		return whatsmeow.UploadResponse{URL: "https://mmg/a", DirectPath: "/v/a", MediaKey: []byte("k"),
			FileSHA256: []byte("s"), FileEncSHA256: []byte("e"), FileLength: uint64(len(data))}, nil
	}
	msg, err := voiceMessage(context.Background(), upload, []byte("OggS"), 7)
	if err != nil {
		t.Fatal(err)
	}
	am := msg.GetAudioMessage()
	if kind != whatsmeow.MediaAudio || am == nil || !am.GetPTT() || am.GetSeconds() != 7 ||
		am.GetMimetype() != "audio/ogg; codecs=opus" || am.GetDirectPath() != "/v/a" || am.GetFileLength() != 4 {
		t.Fatalf("audio message: %v (kind %v)", msg, kind)
	}
}
