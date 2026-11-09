package meeting

import (
	"strings"
	"time"
	"unicode"
)

// Live captions arrive as partial updates: a caption block on the page grows
// word by word, its last words are revised as recognition improves, a long
// block may lose its start as it scrolls, and a block's element may be
// reused for the next sentence. The page script reports every change of a
// block (by an ID it gives each element) and when a block disappears; the
// Assembler turns that stream into finished lines without repeats.

// Assembler collects caption updates into finished lines. It is not safe
// for concurrent use.
type Assembler struct {
	// Settle is how long a block must stay unchanged to count as finished
	// (default 5s). A block that disappears is finished at once.
	Settle time.Duration

	open   map[string]*segment
	order  []string // open keys, oldest first
	recent []Line   // the last finished lines, for dropping repeats
}

type segment struct {
	speaker, text string
	first, last   time.Time
}

// NewAssembler returns an assembler that already knows the given lines
// (the end of a transcript being resumed), so it does not repeat them.
func NewAssembler(settle time.Duration, known []Line) *Assembler {
	if settle <= 0 {
		settle = 5 * time.Second
	}
	a := &Assembler{Settle: settle, open: map[string]*segment{}}
	if len(known) > maxRecent {
		known = known[len(known)-maxRecent:]
	}
	a.recent = append(a.recent, known...)
	return a
}

const maxRecent = 20

// Update records the current speaker and text of block key. It returns
// lines finished by it: the block's earlier sentence when the element was
// reused for a new one.
func (a *Assembler) Update(key, speaker, text string, now time.Time) []Line {
	speaker, text = clean(speaker), clean(text)
	if text == "" {
		return nil
	}
	s := a.open[key]
	if s == nil {
		a.start(key, speaker, text, now)
		return nil
	}
	if speaker != "" && s.speaker != "" && speaker != s.speaker {
		out := a.finish(key)
		a.start(key, speaker, text, now)
		return out
	}
	if merged, ok := merge(s.text, text); ok {
		if merged != s.text {
			s.text, s.last = merged, now
		}
		if s.speaker == "" {
			s.speaker = speaker
		}
		return nil
	}
	out := a.finish(key)
	a.start(key, speaker, text, now)
	return out
}

// Gone finishes block key, which left the page.
func (a *Assembler) Gone(key string) []Line {
	if a.open[key] == nil {
		return nil
	}
	return a.finish(key)
}

// Flush finishes the blocks that have not changed for Settle.
func (a *Assembler) Flush(now time.Time) []Line {
	var out []Line
	for _, k := range append([]string(nil), a.order...) {
		if s := a.open[k]; s != nil && now.Sub(s.last) >= a.Settle {
			out = append(out, a.finish(k)...)
		}
	}
	return out
}

// Close finishes every open block.
func (a *Assembler) Close() []Line {
	var out []Line
	for _, k := range append([]string(nil), a.order...) {
		out = append(out, a.finish(k)...)
	}
	return out
}

// Pending is the number of blocks still open.
func (a *Assembler) Pending() int { return len(a.open) }

func (a *Assembler) start(key, speaker, text string, now time.Time) {
	a.open[key] = &segment{speaker: speaker, text: text, first: now, last: now}
	a.order = append(a.order, key)
}

func (a *Assembler) finish(key string) []Line {
	s := a.open[key]
	delete(a.open, key)
	for i, k := range a.order {
		if k == key {
			a.order = append(a.order[:i], a.order[i+1:]...)
			break
		}
	}
	if s == nil {
		return nil
	}
	l, ok := a.dedupe(Line{Time: s.first, Speaker: s.speaker, Text: s.text})
	if !ok {
		return nil
	}
	a.recent = append(a.recent, l)
	if len(a.recent) > maxRecent {
		a.recent = a.recent[len(a.recent)-maxRecent:]
	}
	return []Line{l}
}

// dedupe drops a line that repeats a recent one of the same speaker (a block
// shown again, or a fragment of one), and trims the start it shares with
// the end of one (a block that came back after scrolling away).
func (a *Assembler) dedupe(l Line) (Line, bool) {
	w := words(l.Text)
	for i := len(a.recent) - 1; i >= 0 && i >= len(a.recent)-10; i-- {
		r := a.recent[i]
		if r.Speaker != "" && l.Speaker != "" && r.Speaker != l.Speaker {
			continue
		}
		gap := l.Time.Sub(r.Time).Abs()
		if gap > 2*time.Minute {
			continue
		}
		rw := words(r.Text)
		if len(w) < 3 {
			// "Yes." said twice is two answers, unless the same block
			// came straight back.
			if i == len(a.recent)-1 && gap < 15*time.Second && len(rw) == len(w) && commonPrefix(rw, w) == len(w) {
				return l, false
			}
			continue
		}
		if contains(rw, w) {
			return l, false
		}
		if k := overlap(rw, w); k >= min(3, len(w)) && k > 0 {
			rest := w[k:]
			if len(rest) == 0 {
				return l, false
			}
			l.Text = joinWords(rest)
			w = rest
		}
	}
	return l, true
}

// merge combines a block's earlier text with its new text. It reports
// false when the new text is a different sentence.
func merge(old, cur string) (string, bool) {
	ow, nw := words(old), words(cur)
	if len(ow) == 0 {
		return cur, true
	}
	p := commonPrefix(ow, nw)
	switch {
	case p == len(ow): // grew, or unchanged
		return cur, true
	case p == len(nw): // shrank at the end: a revision
		return cur, true
	case p >= max(1, (len(ow)+1)/2): // the end was revised
		return cur, true
	}
	// The start scrolled away while the end grew.
	if k := overlap(ow, nw); k >= min(3, len(nw)) && k > 0 {
		if k == len(nw) {
			return old, true
		}
		return old + " " + joinWords(nw[k:]), true
	}
	if contains(ow, nw) { // the start scrolled away and nothing was added
		return old, true
	}
	return "", false
}

type word struct{ raw, key string }

func words(s string) []word {
	f := strings.Fields(s)
	out := make([]word, 0, len(f))
	for _, w := range f {
		k := strings.ToLower(strings.Map(func(r rune) rune {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				return r
			}
			return -1
		}, w))
		if k == "" {
			k = w
		}
		out = append(out, word{raw: w, key: k})
	}
	return out
}

func joinWords(ws []word) string {
	s := make([]string, len(ws))
	for i, w := range ws {
		s[i] = w.raw
	}
	return strings.Join(s, " ")
}

func commonPrefix(a, b []word) int {
	n := 0
	for n < len(a) && n < len(b) && a[n].key == b[n].key {
		n++
	}
	return n
}

// overlap is the longest k such that the last k words of a are the first k
// words of b.
func overlap(a, b []word) int {
	for k := min(len(a), len(b)); k > 0; k-- {
		ok := true
		for i := 0; i < k; i++ {
			if a[len(a)-k+i].key != b[i].key {
				ok = false
				break
			}
		}
		if ok {
			return k
		}
	}
	return 0
}

// contains reports whether b occurs in a as a run of words.
func contains(a, b []word) bool {
	if len(b) == 0 || len(b) > len(a) {
		return len(b) == 0
	}
	for i := 0; i+len(b) <= len(a); i++ {
		ok := true
		for j := range b {
			if a[i+j].key != b[j].key {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// clean collapses whitespace and straightens typographic apostrophes.
func clean(s string) string {
	s = strings.NewReplacer("‘", "'", "’", "'").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}
