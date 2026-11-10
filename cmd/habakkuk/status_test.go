package main

import (
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/work/schedule"
)

// TestScheduleLine: status names the next schedule and says when nothing
// will run it.
func TestScheduleLine(t *testing.T) {
	now := time.Now()
	scs := []schedule.Schedule{
		{Name: "later", Enabled: true, NextRun: now.Add(2 * time.Hour)},
		{Name: "morning brief", Enabled: true, NextRun: now.Add(time.Hour)},
		{Name: "off", NextRun: now.Add(time.Minute)},
	}
	if got := scheduleLine(scs, false); !strings.HasPrefix(got, `2 of 3 on; next "morning brief"`) || !strings.Contains(got, "only while hk serve does") {
		t.Fatalf("scheduleLine = %q", got)
	}
	if got := scheduleLine(scs, true); strings.Contains(got, "serve") {
		t.Fatalf("scheduleLine while serving = %q", got)
	}
}
