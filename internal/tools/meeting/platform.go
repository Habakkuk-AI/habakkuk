package meeting

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Platform is a video-call service Habakkuk can join as a guest.
type Platform string

const (
	GoogleMeet Platform = "meet"
	Zoom       Platform = "zoom"
	Teams      Platform = "teams"
)

// Title is the platform's name for people.
func (p Platform) Title() string {
	switch p {
	case GoogleMeet:
		return "Google Meet"
	case Zoom:
		return "Zoom"
	case Teams:
		return "Microsoft Teams"
	}
	return string(p)
}

// ErrUnsupported means the URL is not a meeting link of a supported platform.
var ErrUnsupported = errors.New("only Google Meet (meet.google.com), Zoom (zoom.us) and Microsoft Teams (teams.microsoft.com, teams.live.com) meeting links can be joined")

var (
	meetCode = regexp.MustCompile(`^/[a-z]{3,4}-[a-z]{4}-[a-z]{3,4}$|^/lookup/[A-Za-z0-9_-]+$|^/[a-z0-9-]{8,}$`)
	zoomJoin = regexp.MustCompile(`^/(?:j|w|wc/join|wc)/(\d{9,12})(?:/join)?/?$`)
)

// Detect checks that raw is a meeting link of a supported platform and
// returns the platform and the page to open: a Zoom /j/ link becomes the
// web client's join page, so no app is launched.
func Detect(raw string) (Platform, string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", "", fmt.Errorf("%q is not a URL: %w", raw, ErrUnsupported)
	}
	switch u.Scheme {
	case "https":
	case "http":
		u.Scheme = "https"
	default:
		return "", "", fmt.Errorf("%q: %w", raw, ErrUnsupported)
	}
	if u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return "", "", fmt.Errorf("%q: %w", raw, ErrUnsupported)
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	u.Host = host
	u.Fragment = ""
	switch {
	case host == "meet.google.com":
		if !meetCode.MatchString(u.Path) {
			return "", "", fmt.Errorf("%q is not a Google Meet meeting link (want meet.google.com/abc-defg-hij)", raw)
		}
		return GoogleMeet, u.String(), nil
	case host == "zoom.us" || strings.HasSuffix(host, ".zoom.us"):
		if m := zoomJoin.FindStringSubmatch(u.Path); m != nil {
			u.Path = "/wc/join/" + m[1]
			return Zoom, u.String(), nil
		}
		if strings.HasPrefix(u.Path, "/my/") {
			return Zoom, u.String(), nil // a personal link; it leads to the join page
		}
		return "", "", fmt.Errorf("%q is not a Zoom meeting link (want zoom.us/j/MEETING_ID)", raw)
	case host == "teams.microsoft.com" || host == "teams.live.com" || host == "teams.cloud.microsoft":
		if strings.Contains(u.Path, "meetup-join") || strings.HasPrefix(u.Path, "/meet/") || strings.HasPrefix(u.Path, "/v2/") {
			return Teams, u.String(), nil
		}
		return "", "", fmt.Errorf("%q is not a Teams meeting link (want teams.microsoft.com/l/meetup-join/... or teams.live.com/meet/...)", raw)
	}
	return "", "", fmt.Errorf("%s: %w", host, ErrUnsupported)
}

// Patterns are case-insensitive regular expressions written in the subset
// Go and JavaScript share, because the page script matches with the same
// ones. Labels are an element's aria-label, else its text; switches and
// checkboxes end in " [on]" or " [off]".

// captionSelectors find live captions in the page. Each list is tried in
// order; selectors are compound (no combinators), so tests can match them
// without a browser. Items default to the container's children; a speaker
// and text default to the item's first short line and the rest.
type captionSelectors struct {
	Containers []string `json:"containers"`
	Items      []string `json:"items"`
	Speaker    []string `json:"speaker"`
	Text       []string `json:"text"`
}

type spec struct {
	interstitial string // buttons that lead from a launcher page to the browser join page
	nameField    string // field labels of the display name
	join         string // buttons that join or ask to join
	mute         string // buttons that turn the microphone or camera off
	waiting      string // page text in the lobby
	denied       string // page text when refused or removed
	ended        string // page text after the call
	invalid      string // page text for a bad link, or a meeting that needs a signed-in account
	alone        string // page text when nobody else is in the call
	leave        string // the in-call button that leaves
	leaveConfirm string // a confirmation after it
	rejoin       string // a button offered after leaving
	captionSteps []string
	captionsOn   string // a button that shows captions are on
	captionKey   string // a keyboard shortcut that turns captions on
	chatOpen     string
	chatField    string
	captions     captionSelectors
	chat         []string // chat panels, whose text never counts as page state
}

// Shared by every platform.
const (
	dismissJoin   = `^(got it|dismiss|close|ok|okay|not now|no thanks|maybe later|reject all|decline|decline all|necessary cookies only|continue without (microphone|audio|video|audio or video|microphone and camera)|join without audio|skip)( \(.*\))?$`
	dismissInCall = `^(got it|dismiss|not now|no thanks|ok|okay|maybe later)$`
	muteCommon    = `^(turn off (microphone|camera|mic|video)|turn (microphone|camera|mic|video) off|mute( my)?( microphone| mic| audio)?|mute mic|stop( my)? video)( \(.*\))?$|^(microphone|mic|camera|video)( \(.*\))? \[on\]$`
	signIn        = `sign in to join|you must sign in|you need to sign in|only signed[- ]in|requires? (you to )?sign in|only authenticated users|authorized attendees only`
	aloneCommon   = `you're the only one here|you are the only one here|no one else is here|you are the only participant|waiting for others to join`
	leaveCommon   = `^(leave call|leave|leave meeting|hang up|end call|leave the meeting)( \(.*\))?$`
	moreButton    = `^(more|more actions|more options)( \(.*\))?$`
)

var specs = map[Platform]*spec{
	GoogleMeet: {
		interstitial: `^(continue without an account|use without an account|join as a guest)$`,
		nameField:    `your name|what's your name|enter your name|name`,
		join:         `^(ask to join|join now|join anyway|join)$`,
		mute:         muteCommon,
		waiting:      `asking to be let in|someone in the call lets you in|you'll join the call when someone lets you in|someone will let you in soon|waiting for (the )?host|please wait until a meeting host brings you`,
		denied:       `denied your request to join|someone in the call denied|you can't join this (video )?call|no one responded to your request|you've been removed from the meeting|removed you from the (meeting|call)|you were removed`,
		ended:        `you left the meeting|you've left the meeting|you left the call|you've left the call|the call has ended|this call has ended|the meeting has ended|return to home screen`,
		invalid:      `check your meeting code|invalid video call name|meeting code is invalid|video call link is invalid|this meeting doesn't exist|couldn't find the meeting|` + signIn,
		alone:        aloneCommon,
		leave:        leaveCommon,
		leaveConfirm: `^(just leave the call|leave call)$`,
		rejoin:       `^rejoin$`,
		captionSteps: []string{`^turn on captions( \(.*\))?$`, `^captions$`},
		captionsOn:   `^turn off captions( \(.*\))?$`,
		captionKey:   "c",
		chatOpen:     `^(chat with everyone|open chat|chat|in-call messages)( \(.*\))?$`,
		chatField:    `send a message|message|chat`,
		captions: captionSelectors{
			Containers: []string{`div[role="region"][aria-label="Captions"]`, `div[aria-label="Captions"]`, `[jsname="dsyhDe"]`, `.a4cQT`},
			Items:      []string{`.nMcdL`, `[data-caption-id]`},
			Speaker:    []string{`.NWpY1d`, `.KcIKyf`, `.zs7s8d`, `[data-speaker]`},
			Text:       []string{`.ygicle`, `.bh44bd`, `.iTTPOb`, `[data-caption-text]`},
		},
		chat: []string{`[aria-label="Side panel"]`, `[aria-label="In-call messages"]`, `[jsname="xySENc"]`, `[role="log"]`},
	},
	Zoom: {
		interstitial: `^(join from your browser|join from browser|join from browser instead)$`,
		nameField:    `your name|enter your name|name|input-for-name`,
		join:         `^(join|join meeting|join now)$`,
		mute:         muteCommon,
		waiting:      `host will let you in soon|meeting host will let you in|waiting for the host to start|we've let them know you're here|waiting room|please wait, the meeting host`,
		denied:       `removed (you )?from (this|the) meeting|host has removed you|you have been removed|denied your request|not allowed to join|cannot join this meeting`,
		ended:        `this meeting has been ended|meeting has been ended by|the meeting has ended|you have left the meeting|you left the meeting|meeting is over`,
		invalid:      `invalid meeting id|meeting id is not valid|this meeting link is invalid|meeting does not exist|incorrect passcode|wrong passcode|passcode is incorrect|` + signIn,
		alone:        aloneCommon,
		leave:        leaveCommon,
		leaveConfirm: `^(leave meeting)$`,
		rejoin:       `^rejoin$`,
		captionSteps: []string{`^(show captions|show subtitle|show subtitles|enable captions)$`, `^(captions|closed caption|closed captions|cc|show captions|live transcript)( \(.*\))?$`, moreButton},
		captionsOn:   `^(hide captions|hide subtitle|hide subtitles|disable captions)$`,
		chatOpen:     `^(chat|open the chat panel|open chat)( \(.*\))?$`,
		chatField:    `type message here|message|chat`,
		captions: captionSelectors{
			Containers: []string{`#live-transcription-subtitle`, `.live-transcription-subtitle__box`, `.lt-full-transcript__list`, `[class*="live-transcription-subtitle"]`},
			Items:      []string{`.live-transcription-subtitle__item`, `.lt-full-transcript__item`},
			Speaker:    []string{`.lt-full-transcript__display-name`, `.live-transcription-subtitle__name`},
			Text:       []string{`.lt-full-transcript__message`, `.live-transcription-subtitle__text`},
		},
		chat: []string{`.chat-container`, `#chat-list-content`, `.chat-content__chat-scrollbar`, `[role="log"]`},
	},
	Teams: {
		interstitial: `^(continue on this browser|join on the web instead|use the web app instead|join on the web|continue in this browser)$`,
		nameField:    `type your name|enter your name|your name|name|prejoin-display-name`,
		join:         `^(join now|join meeting|join)$`,
		mute:         muteCommon,
		waiting:      `someone in the meeting should let you in soon|we've let people in the meeting know you're waiting|we'll let people know you're waiting|when the meeting starts, we'll let|someone will let you in shortly|you're in the lobby`,
		denied:       `denied access to the meeting|you've been removed from this meeting|someone removed you|you can't join this meeting|no one let you in|nobody let you in|your request to join wasn't accepted|you were removed`,
		ended:        `the meeting has ended|meeting ended|you left the meeting|you've left the meeting|call ended|thanks for joining|how was the (call|meeting) quality`,
		invalid:      `this meeting link is invalid|we couldn't find the meeting|meeting not found|the link you used is invalid|` + signIn,
		alone:        aloneCommon,
		leave:        leaveCommon,
		leaveConfirm: `^(leave)$`,
		rejoin:       `^rejoin$`,
		captionSteps: []string{`^(turn on live captions|show live captions|turn on captions|live captions|captions)( \[off\])?$`, `^(language and speech|captions and transcripts)$`, moreButton},
		captionsOn:   `^(turn off live captions|hide live captions|turn off captions|(live captions|captions) \[on\])$`,
		chatOpen:     `^(chat|show conversation|open chat)( \(.*\))?$`,
		chatField:    `type a message|type a new message|type a reply|message`,
		captions: captionSelectors{
			Containers: []string{`[data-tid="closed-caption-v2-window-wrapper"]`, `[data-tid="closed-captions-renderer"]`, `[data-tid="closed-caption-renderer-wrapper"]`},
			Items:      []string{`.fui-ChatMessageCompact`, `[data-tid="closed-caption-message"]`, `.ui-chat__item`},
			Speaker:    []string{`[data-tid="author"]`, `.ui-chat__message__author`},
			Text:       []string{`[data-tid="closed-caption-text"]`, `.ui-chat__message__content`},
		},
		chat: []string{`[data-tid="message-pane-list-viewport"]`, `[data-tid="chat-pane-list"]`, `[role="log"]`},
	},
}

func specFor(p Platform) *spec {
	if s := specs[p]; s != nil {
		return s
	}
	return specs[GoogleMeet]
}

// skipSelectors are the regions whose text is what people said or wrote,
// never page state: captions and chat.
func (s *spec) skipSelectors() []string {
	return append(append([]string(nil), s.captions.Containers...), s.chat...)
}

var reCache = map[string]*regexp.Regexp{}

func init() {
	for _, s := range specs {
		for _, p := range append([]string{s.interstitial, s.nameField, s.join, s.mute, s.waiting, s.denied, s.ended,
			s.invalid, s.alone, s.leave, s.leaveConfirm, s.rejoin, s.captionsOn, s.chatOpen, s.chatField,
			dismissJoin, dismissInCall}, s.captionSteps...) {
			if p != "" {
				reCache[p] = regexp.MustCompile("(?i)" + p)
			}
		}
	}
}

// re compiles a pattern case-insensitively (the specs' patterns are
// compiled once at start).
func re(p string) *regexp.Regexp {
	if r := reCache[p]; r != nil {
		return r
	}
	return regexp.MustCompile("(?i)" + p)
}
