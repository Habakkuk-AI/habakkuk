package meeting

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
)

// parseCall reads the operation and argument from the comment pageCall
// puts first.
func parseCall(expr string) (string, json.RawMessage, bool) {
	if !strings.HasPrefix(expr, "/*") {
		return "", nil, false
	}
	end := strings.Index(expr, "*/")
	if end < 0 {
		return "", nil, false
	}
	var h struct {
		Op  string          `json:"op"`
		Arg json.RawMessage `json:"arg"`
	}
	if json.Unmarshal([]byte(expr[2:end]), &h) != nil {
		return "", nil, false
	}
	return h.Op, h.Arg, true
}

// fakeMeet is a Google Meet that a fake browser shows: a pre-join page, a
// lobby that lets Habakkuk in after a few looks, and a call with captions,
// chat and a leave button.
type fakeMeet struct {
	mu        sync.Mutex
	stage     string // prejoin, waiting, incall, left, denied, ended
	deny      bool   // the lobby refuses
	lobby     int    // probes seen in the lobby
	admit     int    // probes in the lobby before someone answers
	name      string
	mic, cam  bool
	captions  bool
	observer  int // times the caption observer was installed
	chatOpen  bool
	focused   string
	draft     string
	chat      []string
	navigated []string
	pages     int
	last      *fakePage
}

func newFakeMeet() *fakeMeet { return &fakeMeet{stage: "prejoin", mic: true, cam: true, admit: 2} }

func (f *fakeMeet) probe() Probe {
	onOff := func(on bool, what, keys string) string {
		if on {
			return "Turn off " + what + " (" + keys + ")"
		}
		return "Turn on " + what + " (" + keys + ")"
	}
	switch f.stage {
	case "prejoin":
		return Probe{Title: "Meet", Text: "Ready to join? No one else is here",
			Fields:  []string{"Your name | Your name"},
			Buttons: []string{onOff(f.mic, "microphone", "ctrl + d"), onOff(f.cam, "camera", "ctrl + e"), "Ask to join"}}
	case "waiting":
		if f.lobby++; f.lobby >= f.admit {
			if f.deny {
				f.stage = "denied"
			} else {
				f.stage = "incall"
			}
		}
		return Probe{Text: "Asking to be let in... You'll join the call when someone lets you in", Buttons: []string{"Cancel"}}
	case "incall":
		pr := Probe{Text: "Weekly sync", Captions: f.captions,
			Buttons: []string{onOff(f.mic, "microphone", "ctrl + d"), onOff(f.cam, "camera", "ctrl + e"), "Chat with everyone", "Leave call"}}
		if f.captions {
			pr.Buttons = append(pr.Buttons, "Turn off captions (c)")
		} else {
			pr.Buttons = append(pr.Buttons, "Turn on captions (c)")
		}
		if f.chatOpen {
			pr.Fields = []string{"Send a message | Send a message"}
		}
		return pr
	case "denied":
		return Probe{Text: "Someone in the call denied your request to join", Buttons: []string{"Return to home screen"}}
	case "ended":
		return Probe{Text: "The call has ended", Buttons: []string{"Rejoin"}}
	}
	return Probe{Text: "You left the meeting", Buttons: []string{"Rejoin", "Return to home screen"}}
}

func (f *fakeMeet) press(label string) {
	switch {
	case strings.HasPrefix(label, "Turn off microphone"):
		f.mic = false
	case strings.HasPrefix(label, "Turn off camera"):
		f.cam = false
	case label == "Ask to join":
		f.stage, f.lobby = "waiting", 0
	case strings.HasPrefix(label, "Turn on captions"):
		f.captions = true
	case label == "Chat with everyone":
		f.chatOpen = true
	case label == "Leave call":
		f.stage = "left"
	}
}

// fakePage is one browser showing the fake meeting.
type fakePage struct {
	f       *fakeMeet
	mu      sync.Mutex
	binding func(string)
	pending string
	done    chan struct{}
	once    sync.Once
}

func (f *fakeMeet) launcher() Launcher {
	return func(ctx context.Context, rec *Record, dir string) (Page, error) {
		p := &fakePage{f: f, done: make(chan struct{})}
		f.mu.Lock()
		f.pages++
		f.last = p
		f.mu.Unlock()
		return p, nil
	}
}

func (p *fakePage) Navigate(_ context.Context, url string) error {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	p.f.navigated = append(p.f.navigated, url)
	if p.f.stage == "left" || p.f.stage == "incall" {
		p.f.stage = "prejoin" // a new page starts at the door
	}
	p.f.captions, p.f.chatOpen = false, false
	return nil
}

func (p *fakePage) Eval(ctx context.Context, expr string) (json.RawMessage, error) {
	select {
	case <-p.done:
		return nil, errors.New("the browser connection closed")
	default:
	}
	op, arg, ok := parseCall(expr)
	if !ok {
		return nil, errors.New("not a page call")
	}
	var a struct {
		Pattern string `json:"pattern"`
	}
	_ = json.Unmarshal(arg, &a)
	f := p.f
	f.mu.Lock()
	defer f.mu.Unlock()
	switch op {
	case "probe":
		return json.Marshal(f.probe())
	case "find":
		r := regexp.MustCompile("(?i)" + a.Pattern)
		for _, b := range f.peek().Buttons {
			if r.MatchString(b) {
				p.mu.Lock()
				p.pending = b
				p.mu.Unlock()
				return json.Marshal(map[string]any{"x": 10, "y": 20, "label": b})
			}
		}
		return json.RawMessage("null"), nil
	case "focus":
		r := regexp.MustCompile("(?i)" + a.Pattern)
		for _, fl := range f.peek().Fields {
			if r.MatchString(fl) {
				value := ""
				if strings.HasPrefix(fl, "Your name") {
					f.focused, value = "name", f.name
				} else {
					f.focused, value = "chat", f.draft
				}
				return json.Marshal(map[string]string{"value": value, "label": fl})
			}
		}
		return json.RawMessage("null"), nil
	case "captions":
		f.observer++
		return json.RawMessage(`"started"`), nil
	}
	return nil, errors.New("unknown op " + op)
}

// peek is the page as it is, without the lobby counting a look.
func (f *fakeMeet) peek() Probe {
	lobby := f.lobby
	stage := f.stage
	pr := f.probe()
	f.lobby, f.stage = lobby, stage
	return pr
}

func (p *fakePage) InsertText(_ context.Context, text string) error {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	switch p.f.focused {
	case "name":
		p.f.name = text
	case "chat":
		p.f.draft = text
	}
	return nil
}

func (p *fakePage) Click(_ context.Context, x, y float64) error {
	p.mu.Lock()
	label := p.pending
	p.pending = ""
	p.mu.Unlock()
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	p.f.press(label)
	return nil
}

func (p *fakePage) Press(_ context.Context, key string) error {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	if key == "Enter" && p.f.focused == "chat" && p.f.draft != "" {
		p.f.chat = append(p.f.chat, p.f.draft)
		p.f.draft = ""
	}
	return nil
}

func (p *fakePage) Bind(_ context.Context, name string, fn func(string)) error {
	if name != bindingName {
		return errors.New("unexpected binding " + name)
	}
	p.mu.Lock()
	p.binding = fn
	p.mu.Unlock()
	return nil
}

// say sends caption changes as the page script would.
func (p *fakePage) say(epoch string, changes ...map[string]any) bool {
	p.mu.Lock()
	fn := p.binding
	p.mu.Unlock()
	if fn == nil {
		return false
	}
	data, _ := json.Marshal(map[string]any{"e": epoch, "c": changes})
	fn(string(data))
	return true
}

func (p *fakePage) crash() { p.once.Do(func() { close(p.done) }) }

// page is the browser showing the meeting now.
func (f *fakeMeet) page() *fakePage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

// state reads the meeting's state under its lock.
func (f *fakeMeet) state(fn func(f *fakeMeet)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (p *fakePage) Done() <-chan struct{} { return p.done }
func (p *fakePage) Close()                { p.crash() }
