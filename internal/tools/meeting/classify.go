package meeting

import "strings"

// Probe is what the page script reports about the page: enough to tell
// where in the join flow Habakkuk is, without the model reading the page.
type Probe struct {
	URL     string   `json:"url"`
	Title   string   `json:"title"`
	Text    string   `json:"text"`    // visible text, without captions and chat, clipped
	Buttons []string `json:"buttons"` // labels of visible clickable elements
	Fields  []string `json:"fields"`  // labels of visible text fields: aria-label | placeholder | label | id | name
	// Captions is true when a captions container is on the page.
	Captions bool `json:"captions"`
}

// State is where the page is in the join flow.
type State string

const (
	StateUnknown      State = "unknown"      // nothing recognized yet (loading, or a page Habakkuk does not know)
	StateInterstitial State = "interstitial" // a launcher page offering to join in the browser
	StatePreJoin      State = "prejoin"      // name entry and a join button
	StateWaiting      State = "waiting"      // the lobby
	StateInCall       State = "in_call"
	StateDenied       State = "denied"  // refused at the door, or removed
	StateEnded        State = "ended"   // the call is over, or Habakkuk left it
	StateInvalid      State = "invalid" // a bad link, or a meeting guests cannot join
)

// Classify tells the state of a page of the platform from a probe. Text of
// what people said (captions, chat) is not in the probe, so a participant
// saying "the meeting has ended" does not end anything.
func Classify(p Platform, pr Probe) State {
	s := specFor(p)
	text := strings.ToLower(clean(pr.Text))
	switch {
	case re(s.invalid).MatchString(text):
		return StateInvalid
	case re(s.denied).MatchString(text):
		return StateDenied
	case re(s.waiting).MatchString(text):
		return StateWaiting
	case re(s.ended).MatchString(text) || hasButton(pr, s.rejoin):
		return StateEnded
	case hasButton(pr, s.leave):
		return StateInCall
	case hasField(pr, s.nameField) || hasButton(pr, s.join):
		return StatePreJoin
	case hasButton(pr, s.interstitial):
		return StateInterstitial
	}
	return StateUnknown
}

// Reason is a short description of a final state for people, quoting the
// page where it can.
func Reason(p Platform, st State, pr Probe) string {
	s := specFor(p)
	quote := func(pattern string) string {
		text := clean(pr.Text)
		loc := re(pattern).FindStringIndex(text)
		if loc == nil {
			return ""
		}
		// The sentence around the match.
		start, end := loc[0], loc[1]
		if i := strings.LastIndexAny(text[:start], ".!?"); i >= 0 {
			start = i + 1
		} else {
			start = 0
		}
		if i := strings.IndexAny(text[end:], ".!?"); i >= 0 {
			end += i + 1
		} else {
			end = len(text)
		}
		return " (the page says: \"" + clip(text[start:end], 160) + "\")"
	}
	switch st {
	case StateInvalid:
		return "the meeting link does not work for a guest" + quote(s.invalid)
	case StateDenied:
		return "the meeting refused Habakkuk or removed it" + quote(s.denied)
	case StateEnded:
		return "the meeting ended" + quote(s.ended)
	}
	return string(st)
}

// CaptionsOn reports whether the page shows captions are on.
func CaptionsOn(p Platform, pr Probe) bool {
	return pr.Captions || hasButton(pr, specFor(p).captionsOn)
}

// Alone reports whether the page says nobody else is in the call.
func Alone(p Platform, pr Probe) bool {
	return re(specFor(p).alone).MatchString(strings.ToLower(clean(pr.Text)))
}

func hasButton(pr Probe, pattern string) bool {
	_, ok := firstMatch(pr.Buttons, pattern)
	return ok
}

func hasField(pr Probe, pattern string) bool {
	_, ok := firstMatch(pr.Fields, pattern)
	return ok
}

func firstMatch(labels []string, pattern string) (string, bool) {
	if pattern == "" {
		return "", false
	}
	r := re(pattern)
	for _, l := range labels {
		if r.MatchString(clean(l)) {
			return l, true
		}
	}
	return "", false
}

// Step is the next thing to do on a page to get into the call.
type Step struct {
	Kind    string // "fill" (type the name into Pattern's field), "click" (a button matching Pattern), "wait" or "done"
	Pattern string
}

// NextJoinStep decides what to do on a page that is not yet the call: fill
// in the name, turn the microphone and camera off, then join; follow a
// launcher page to the browser; dismiss dialogs in the way. muted counts
// the mute clicks already made (each button is pressed once), and filled
// says the name is in.
func NextJoinStep(p Platform, pr Probe, filled bool, muted map[string]bool) Step {
	s := specFor(p)
	switch Classify(p, pr) {
	case StateInCall:
		return Step{Kind: "done"}
	case StateWaiting:
		return Step{Kind: "wait"}
	case StateInterstitial:
		return Step{Kind: "click", Pattern: s.interstitial}
	case StatePreJoin:
		if !filled && hasField(pr, s.nameField) {
			return Step{Kind: "fill", Pattern: s.nameField}
		}
		r := re(s.mute)
		for _, b := range pr.Buttons {
			if r.MatchString(clean(b)) && !muted[clean(b)] {
				return Step{Kind: "click", Pattern: "^" + regexpQuote(clean(b)) + "$"}
			}
		}
		if hasButton(pr, s.join) {
			return Step{Kind: "click", Pattern: s.join}
		}
	}
	if hasButton(pr, dismissJoin) {
		return Step{Kind: "click", Pattern: dismissJoin}
	}
	return Step{Kind: "wait"}
}

// regexpQuote escapes a label for a pattern both Go and JavaScript read
// the same way.
func regexpQuote(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`\.+*?()|[]{}^$/`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
