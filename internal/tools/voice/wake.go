package voice

import (
	"strings"
	"unicode"
)

// Wake detection works on transcripts: every utterance is transcribed and
// a turn starts only when the text begins with a wake phrase. Transcription
// mishears names ("hey Habakkuk" as "hey habakkuks", "a habakkuk", "hay
// Habakkuk"), so the match is fuzzy: a small edit distance over the leading
// words, after a few fillers ("um", "okay so").

// fillers may come before a wake phrase.
var fillers = map[string]bool{"um": true, "uh": true, "er": true, "erm": true, "hmm": true, "so": true, "oh": true, "well": true}

// Words lowercases text and splits it into words, dropping punctuation.
func Words(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\''
	})
}

// MatchWake reports whether text starts with one of the phrases, and
// returns the words after it (the request, possibly empty) with the
// original wording and punctuation.
func MatchWake(text string, phrases []string) (rest string, ok bool) {
	words := Words(text)
	skip := 0
	for skip < len(words) && skip < 2 && fillers[words[skip]] {
		skip++
	}
	best, bestEnd := -1, 0
	for _, p := range phrases {
		pw := Words(p)
		if len(pw) == 0 {
			continue
		}
		target := strings.Join(pw, " ")
		allowed := max(1, len([]rune(target))/4)
		// The heard phrase may have split or merged a word.
		for n := max(1, len(pw)-1); n <= len(pw)+1; n++ {
			for _, start := range []int{0, skip} {
				if start+n > len(words) {
					continue
				}
				got := strings.Join(words[start:start+n], " ")
				if n < len(pw) && len(strings.ReplaceAll(got, " ", "")) < len(strings.ReplaceAll(target, " ", ""))-1 {
					continue // fewer words is only a merge ("heyhabakkuk"), never a dropped word
				}
				d := editDistance(got, target)
				// Merged words ("heyhabakkuk") compare without spaces too.
				if d2 := editDistance(strings.ReplaceAll(got, " ", ""), strings.ReplaceAll(target, " ", "")); d2 < d {
					d = d2
				}
				if d <= allowed && (best < 0 || d < best) {
					best, bestEnd = d, start+n
				}
			}
		}
	}
	if best < 0 {
		return "", false
	}
	return afterWords(text, bestEnd), true
}

// afterWords returns text after its first n words, trimmed of the
// punctuation that followed the wake phrase ("Hey Habakkuk, what's up" ->
// "what's up").
func afterWords(text string, n int) string {
	i, seen := 0, 0
	rs := []rune(text)
	inWord := false
	for ; i < len(rs); i++ {
		r := rs[i]
		isWord := unicode.IsLetter(r) || unicode.IsDigit(r) || r == '\''
		if isWord && !inWord {
			if seen == n {
				break
			}
			seen++
		}
		inWord = isWord
	}
	return strings.TrimSpace(strings.TrimLeft(string(rs[i:]), " ,.!?;:-"))
}

// editDistance is the Levenshtein distance between a and b, in runes.
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// DefaultWake is the wake phrase list when none is configured.
var DefaultWake = []string{"hey habakkuk", "hi habakkuk", "okay habakkuk", "ok habakkuk"}

// ParsePhrases splits a comma-separated -wake value.
func ParsePhrases(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
