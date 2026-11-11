package dingtalk

import (
	"encoding/json"
	"testing"
)

// A voice message with DingTalk's own transcript is Spoken, never Plain.
func TestVoiceTranscriptIsSpoken(t *testing.T) {
	var cb callback
	raw := `{"conversationId":"cidDM","conversationType":"1","msgId":"m4","msgtype":"audio","senderStaffId":"staff1","robotCode":"ding1","content":{"downloadCode":"dc2","recognition":"run echo approved-run"}}`
	if err := json.Unmarshal([]byte(raw), &cb); err != nil {
		t.Fatal(err)
	}
	a := New(Config{RobotCode: "ding1"})
	if m, ok := a.convert(&cb); !ok || !m.Spoken || m.Plain {
		t.Fatalf("voice: %+v", m)
	}
}
