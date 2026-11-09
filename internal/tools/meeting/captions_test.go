package meeting

import (
	"strings"
	"testing"
	"time"
)

func TestMerge(t *testing.T) {
	tests := []struct {
		old, cur, want string
		ok             bool
	}{
		{"Good morning", "Good morning everyone", "Good morning everyone", true},                          // grew
		{"the budjet", "the budget is", "the budget is", true},                                            // revised, then grew
		{"we should ship on Habakkuk", "we should ship on", "we should ship on", true},                    // the end taken back
		{"one two three four five six", "four five six seven", "one two three four five six seven", true}, // start scrolled away
		{"one two three four five six", "four five six", "one two three four five six", true},             // scrolled, nothing new
		{"I think we should go now", "Sounds good to me", "", false},                                      // a new sentence
		{"Let's meet tomorrow at", "lets meet tomorrow at noon.", "lets meet tomorrow at noon.", true},    // punctuation and case
	}
	for _, tc := range tests {
		got, ok := merge(tc.old, tc.cur)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("merge(%q, %q) = %q, %v; want %q, %v", tc.old, tc.cur, got, ok, tc.want, tc.ok)
		}
	}
}

func texts(lines []Line) []string {
	var out []string
	for _, l := range lines {
		s := l.Text
		if l.Speaker != "" {
			s = l.Speaker + ": " + s
		}
		out = append(out, s)
	}
	return out
}

func TestAssemblerPartialUpdates(t *testing.T) {
	t0 := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	at := func(s float64) time.Time { return t0.Add(time.Duration(s * float64(time.Second))) }
	a := NewAssembler(5*time.Second, nil)
	var out []Line
	// A block grows word by word and has a word corrected.
	for i, s := range []string{"Good", "Good morning", "Good morning every", "Good morning everyone, the budjet", "Good morning everyone, the budget is approved."} {
		out = append(out, a.Update("e1:1", "Alice", s, at(float64(i)))...)
	}
	// A second speaker starts while the first block is still showing.
	out = append(out, a.Update("e1:2", "Bob", "We move", at(5))...)
	out = append(out, a.Update("e1:2", "Bob", "We move the launch", at(6))...)
	if len(out) != 0 {
		t.Fatalf("lines before anything finished: %q", texts(out))
	}
	// Alice's block has not changed for 5s: final.
	out = append(out, a.Flush(at(9))...)
	if got := texts(out); len(got) != 1 || got[0] != "Alice: Good morning everyone, the budget is approved." {
		t.Fatalf("after settle: %q", got)
	}
	if !out[0].Time.Equal(t0) {
		t.Fatalf("a line is timed by its first words: %v", out[0].Time)
	}
	// The same element is reused for Bob's next sentence.
	out = a.Update("e1:2", "Bob", "Any questions?", at(10))
	if got := texts(out); len(got) != 1 || got[0] != "Bob: We move the launch" {
		t.Fatalf("element reused: %q", got)
	}
	// Alice's block shows up again as a new element (re-rendered): no repeat.
	a.Update("e1:3", "Alice", "Good morning everyone, the budget is approved.", at(11))
	if got := a.Gone("e1:3"); len(got) != 0 {
		t.Fatalf("a repeated block was written again: %q", texts(got))
	}
	// A block that is removed is final at once.
	if got := texts(a.Gone("e1:2")); len(got) != 1 || got[0] != "Bob: Any questions?" {
		t.Fatalf("gone: %q", got)
	}
	// A block that came back with its start scrolled away keeps only the new words.
	a.Update("e2:1", "Bob", "move the launch to March", at(20))
	if got := texts(a.Close()); len(got) != 1 || got[0] != "Bob: to March" {
		t.Fatalf("overlap trimmed: %q", got)
	}
	if a.Pending() != 0 {
		t.Fatal("open blocks after Close")
	}
}

func TestAssemblerSpeakerChangeAndShortAnswers(t *testing.T) {
	t0 := time.Now()
	a := NewAssembler(time.Second, nil)
	a.Update("k", "Alice", "Are we ready?", t0)
	out := a.Update("k", "Bob", "Yes.", t0.Add(time.Second))
	out = append(out, a.Gone("k")...)
	// Two separate yeses a minute apart are both kept.
	a.Update("k2", "Bob", "Yes.", t0.Add(70*time.Second))
	out = append(out, a.Gone("k2")...)
	want := []string{"Alice: Are we ready?", "Bob: Yes.", "Bob: Yes."}
	if got := texts(out); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestAssemblerResumeSkipsKnownLines(t *testing.T) {
	t0 := time.Now()
	known := []Line{{Time: t0, Speaker: "Alice", Text: "We decided to move the launch to March."}}
	a := NewAssembler(time.Second, known)
	// After a rejoin the same caption is still on screen.
	a.Update("new:1", "Alice", "We decided to move the launch to March.", t0.Add(30*time.Second))
	a.Update("new:2", "Bob", "Great, thanks.", t0.Add(31*time.Second))
	if got := texts(a.Close()); len(got) != 1 || got[0] != "Bob: Great, thanks." {
		t.Fatalf("got %q", got)
	}
}

func TestFormatTranscript(t *testing.T) {
	t0 := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	lines := []Line{
		{t0, "Alice", "Hello."}, {t0.Add(5 * time.Second), "Alice", "Let's start."},
		{t0.Add(10 * time.Second), "Bob", "Sure."}, {t0.Add(2 * time.Minute), "", "Unattributed words."},
	}
	want := "[10:00:00] Alice: Hello. Let's start.\n[10:00:10] Bob: Sure.\n[10:02:00] Unattributed words."
	if got := FormatTranscript(lines, time.UTC); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestParseLimit(t *testing.T) {
	for in, want := range map[string]time.Duration{
		``: 0, `null`: 0, `45`: 45 * time.Minute, `"45m"`: 45 * time.Minute, `"1h30m"`: 90 * time.Minute, `"90"`: 90 * time.Minute, `" 1h 30m "`: 90 * time.Minute,
	} {
		got, err := parseLimit([]byte(in))
		if err != nil || got != want {
			t.Errorf("parseLimit(%s) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{`-5`, `"soon"`, `true`} {
		if _, err := parseLimit([]byte(bad)); err == nil {
			t.Errorf("parseLimit(%s) accepted it", bad)
		}
	}
}
