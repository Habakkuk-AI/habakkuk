package meeting

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFixtureStates(t *testing.T) {
	srv := fixtureSite(t, nil)
	cases := map[Platform]map[string]State{
		GoogleMeet: {"prejoin": StatePreJoin, "waiting": StateWaiting, "incall": StateInCall, "denied": StateDenied,
			"ended": StateEnded, "left": StateEnded, "invalid": StateInvalid},
		Zoom: {"prejoin": StatePreJoin, "waiting": StateWaiting, "incall": StateInCall, "denied": StateDenied,
			"ended": StateEnded, "left": StateEnded, "invalid": StateInvalid},
		Teams: {"launcher": StateInterstitial, "prejoin": StatePreJoin, "waiting": StateWaiting, "incall": StateInCall,
			"denied": StateDenied, "ended": StateEnded, "left": StateEnded, "invalid": StateInvalid},
	}
	for p, states := range cases {
		tpl := fetchTemplates(t, srv.URL+"/"+string(p))
		for stage, want := range states {
			if tpl[stage] == nil {
				t.Fatalf("%s: no template %q", p, stage)
			}
			pr := probeHTML(t, combine(tpl[stage]), p)
			if got := Classify(p, pr); got != want {
				t.Errorf("%s %s: state %s, want %s (probe %+v)", p, stage, got, want, pr)
			}
		}
		// What people say never changes the state: the sample captions
		// say "the meeting has ended".
		pr := probeHTML(t, combine(tpl["incall"], tpl["captions-sample"]), p)
		if got := Classify(p, pr); got != StateInCall || !pr.Captions || !CaptionsOn(p, pr) {
			t.Errorf("%s in call with captions: state %s, captions %v (text %q)", p, got, pr.Captions, pr.Text)
		}
		if strings.Contains(pr.Text, "ended early") {
			t.Errorf("%s: caption text leaked into the page text: %q", p, pr.Text)
		}
		if Classify(p, probeHTML(t, combine(tpl["incall"]), p)) == StateInCall && CaptionsOn(p, probeHTML(t, combine(tpl["incall"]), p)) {
			t.Errorf("%s: captions look on before they are", p)
		}
	}
}

func TestFixtureCaptions(t *testing.T) {
	srv := fixtureSite(t, nil)
	for p, speakers := range map[Platform][2]string{
		GoogleMeet: {"Alice Chen", "Bob Ortiz"}, Teams: {"Alice Chen", "Bob Ortiz"}, Zoom: {"", ""},
	} {
		tpl := fetchTemplates(t, srv.URL+"/"+string(p))
		got := captionsHTML(t, combine(tpl["captions-sample"]), p)
		want := [][2]string{{speakers[0], "I think the meeting has ended early."}, {speakers[1], "No, we still have the budget to cover."}}
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("%s captions = %q, want %q", p, got, want)
		}
	}
}

func TestJoinSteps(t *testing.T) {
	srv := fixtureSite(t, nil)
	tests := []struct {
		p     Platform
		stage string
		fill  bool
		muted []string
		want  Step
	}{
		{GoogleMeet, "prejoin", false, nil, Step{"fill", specs[GoogleMeet].nameField}},
		{GoogleMeet, "prejoin", true, nil, Step{"click", `^Turn off microphone \(ctrl \+ d\)$`}},
		{GoogleMeet, "prejoin", true, []string{"Turn off microphone (ctrl + d)"}, Step{"click", `^Turn off camera \(ctrl \+ e\)$`}},
		{GoogleMeet, "prejoin", true, []string{"Turn off microphone (ctrl + d)", "Turn off camera (ctrl + e)"}, Step{"click", specs[GoogleMeet].join}},
		{GoogleMeet, "waiting", true, nil, Step{Kind: "wait"}},
		{GoogleMeet, "incall", true, nil, Step{Kind: "done"}},
		{Zoom, "prejoin", true, nil, Step{"click", `^Mute$`}},
		{Zoom, "prejoin", true, []string{"Mute", "Stop Video"}, Step{"click", specs[Zoom].join}},
		{Teams, "launcher", false, nil, Step{"click", specs[Teams].interstitial}},
		{Teams, "prejoin", false, nil, Step{"fill", specs[Teams].nameField}},
		{Teams, "prejoin", true, nil, Step{"click", `^Camera \[on\]$`}},
	}
	for _, tc := range tests {
		tpl := fetchTemplates(t, srv.URL+"/"+string(tc.p))
		muted := map[string]bool{}
		for _, m := range tc.muted {
			muted[m] = true
		}
		got := NextJoinStep(tc.p, probeHTML(t, combine(tpl[tc.stage]), tc.p), tc.fill, muted)
		if got != tc.want {
			t.Errorf("%s %s (filled %v, muted %v): %+v, want %+v", tc.p, tc.stage, tc.fill, tc.muted, got, tc.want)
		}
	}
}

func TestReasonQuotesThePage(t *testing.T) {
	pr := Probe{Text: "Someone in the call denied your request to join"}
	if r := Reason(GoogleMeet, Classify(GoogleMeet, pr), pr); !strings.Contains(r, `says: "Someone in the call denied your request to join"`) {
		t.Fatalf("reason %q", r)
	}
	if !Alone(Teams, Probe{Text: "You're the only one here. Waiting for others to join"}) || Alone(Teams, Probe{Text: "Weekly sync"}) {
		t.Fatal("alone detection")
	}
}

func TestDetect(t *testing.T) {
	ok := []struct{ in, platform, join string }{
		{"https://meet.google.com/abc-defg-hij", "meet", "https://meet.google.com/abc-defg-hij"},
		{"meet.google.com/abc-defg-hij", "", ""},
		{"http://meet.google.com/abc-defg-hij?authuser=1", "meet", "https://meet.google.com/abc-defg-hij?authuser=1"},
		{"https://us02web.zoom.us/j/81234567890?pwd=abc123", "zoom", "https://us02web.zoom.us/wc/join/81234567890?pwd=abc123"},
		{"https://zoom.us/wc/join/81234567890", "zoom", "https://zoom.us/wc/join/81234567890"},
		{"https://acme.zoom.us/my/alice", "zoom", "https://acme.zoom.us/my/alice"},
		{"https://teams.microsoft.com/l/meetup-join/19%3ameeting_abc%40thread.v2/0?context=%7b%7d", "teams",
			"https://teams.microsoft.com/l/meetup-join/19%3ameeting_abc%40thread.v2/0?context=%7b%7d"},
		{"https://teams.live.com/meet/9876543210?p=xyz", "teams", "https://teams.live.com/meet/9876543210?p=xyz"},
	}
	for _, tc := range ok {
		p, join, err := Detect(tc.in)
		if tc.platform == "" {
			if err == nil {
				t.Errorf("Detect(%q) accepted a link without a scheme", tc.in)
			}
			continue
		}
		if err != nil || string(p) != tc.platform || join != tc.join {
			t.Errorf("Detect(%q) = %s, %s, %v; want %s, %s", tc.in, p, join, err, tc.platform, tc.join)
		}
	}
	for _, bad := range []string{
		"https://example.com/abc-defg-hij", "https://meet.google.com.evil.com/abc-defg-hij", "https://evilzoom.us/j/81234567890",
		"https://zoom.us/signin", "https://meet.google.com/", "https://teams.microsoft.com/", "file:///etc/passwd",
		"https://user:pw@meet.google.com/abc-defg-hij", "https://meet.google.com:8443/abc-defg-hij", "javascript:alert(1)",
		"https://127.0.0.1/j/81234567890",
	} {
		if _, _, err := Detect(bad); err == nil {
			t.Errorf("Detect(%q) accepted it", bad)
		}
	}
}

func TestPageCallHeader(t *testing.T) {
	expr := pageCall("find", map[string]string{"pattern": "a*/b"})
	op, arg, ok := parseCall(expr)
	var a struct{ Pattern string }
	if !ok || op != "find" || json.Unmarshal(arg, &a) != nil || a.Pattern != "a*/b" {
		t.Fatalf("header: %q %q %v", op, arg, ok)
	}
	if !strings.Contains(expr, "MutationObserver") || !strings.HasSuffix(expr, `("find", {"pattern":"a*/b"})`) {
		t.Fatalf("expression tail: %s", expr[len(expr)-60:])
	}
}
