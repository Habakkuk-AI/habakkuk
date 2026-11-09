package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/tools/meeting"
)

func TestMeetingsCmd(t *testing.T) {
	home := t.TempDir()
	st := meeting.NewStore(filepath.Join(home, "meetings"))
	now := time.Now()
	rec := &meeting.Record{ID: "mt_0123abcd", Agent: "habakkuk", URL: "https://meet.google.com/abc-defg-hij", Platform: meeting.GoogleMeet,
		Name: "Habakkuk (notetaker)", LimitMS: 3600_000, Status: meeting.StatusInCall, Detail: "in the call, reading captions",
		Created: now, JoinedAt: now, Captions: true, Lines: 1}
	if err := st.Create(rec); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendLines(rec.ID, []meeting.Line{{Time: now, Speaker: "Alice", Text: "Hello all."}}); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		if err := meetingsCmd(context.Background(), home, args, &out); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return out.String()
	}
	if out := run(); !strings.Contains(out, "mt_0123abcd") || !strings.Contains(out, "Google Meet") {
		t.Fatalf("list: %s", out)
	}
	if out := run("show", "-transcript", "mt_0123abcd"); !strings.Contains(out, "Alice: Hello all.") || !strings.Contains(out, "captions: on, 1 lines") {
		t.Fatalf("show: %s", out)
	}
	if out := run("leave", "mt_0123abcd"); !strings.Contains(out, "Asked mt_0123abcd to leave") || !st.LeaveRequested(rec.ID) {
		t.Fatalf("leave: %s", out)
	}
	var out bytes.Buffer
	if err := meetingsCmd(context.Background(), home, []string{"show", "mt_ffffffff"}, &out); err == nil {
		t.Fatal("showed a meeting that does not exist")
	}
}
