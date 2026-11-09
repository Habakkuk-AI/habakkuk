package channel

import (
	"html"
	"regexp"
	"strings"
	"unicode/utf8"
)

// The agent writes Markdown; each platform has its own markup. Conversions
// only touch prose: code spans and fenced blocks pass through untouched.

type segKind int

const (
	segText segKind = iota
	segInline
	segFence
)

type segment struct {
	kind segKind
	text string // for code: the code itself, without the backticks
	lang string
}

func segments(md string) []segment {
	var out []segment
	var prose strings.Builder
	flush := func() {
		if prose.Len() > 0 {
			out = append(out, splitInline(prose.String())...)
			prose.Reset()
		}
	}
	lines := strings.SplitAfter(md, "\n")
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(trimmed, "```") {
			prose.WriteString(lines[i])
			continue
		}
		// Find the closing fence; an unclosed fence runs to the end.
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) == "```" {
				end = j
				break
			}
		}
		flush()
		code := strings.Join(lines[i+1:min(end, len(lines))], "")
		out = append(out, segment{kind: segFence, text: strings.TrimSuffix(code, "\n"), lang: strings.TrimPrefix(trimmed, "```")})
		if end < len(lines) && strings.HasSuffix(lines[end], "\n") {
			prose.WriteString("\n")
		}
		i = end
	}
	flush()
	return out
}

var inlineCode = regexp.MustCompile("`([^`\n]+)`")

func splitInline(s string) []segment {
	var out []segment
	last := 0
	for _, m := range inlineCode.FindAllStringSubmatchIndex(s, -1) {
		if m[0] > last {
			out = append(out, segment{kind: segText, text: s[last:m[0]]})
		}
		out = append(out, segment{kind: segInline, text: s[m[2]:m[3]]})
		last = m[1]
	}
	if last < len(s) {
		out = append(out, segment{kind: segText, text: s[last:]})
	}
	return out
}

var (
	mdBold     = regexp.MustCompile(`\*\*([^*\n]+?)\*\*|__([^_\n]+?)__`)
	mdItalic   = regexp.MustCompile(`(^|[^*\w])\*([^*\n]+?)\*($|[^*\w])`)
	mdUItalic  = regexp.MustCompile(`(^|\W)_([^_\n]+?)_($|\W)`)
	mdStrike   = regexp.MustCompile(`~~([^~\n]+?)~~`)
	mdHeader   = regexp.MustCompile(`(?m)^#{1,6}[ \t]+(.+?)[ \t#]*$`)
	mdLink     = regexp.MustCompile(`\[([^\]\n]+)\]\(([^)\s]+)\)`)
	mdBullet   = regexp.MustCompile(`(?m)^([ \t]*)[-*][ \t]+`)
	boldMarker = "\x01"
)

func firstGroup(m []string) string {
	if m[1] != "" {
		return m[1]
	}
	return m[2]
}

// markBold replaces **x** and __x__ with markers so italic rules skip them.
func markBold(s string) string {
	return mdBold.ReplaceAllStringFunc(s, func(x string) string {
		return boldMarker + firstGroup(mdBold.FindStringSubmatch(x)) + boldMarker
	})
}

// FormatSlack converts Markdown to Slack mrkdwn.
func FormatSlack(md string) string {
	var b strings.Builder
	for _, s := range segments(md) {
		switch s.kind {
		case segFence:
			b.WriteString("```\n" + s.text + "\n```")
		case segInline:
			b.WriteString("`" + s.text + "`")
		default:
			t := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s.text)
			t = mdHeader.ReplaceAllString(t, boldMarker+"$1"+boldMarker)
			t = mdBullet.ReplaceAllString(t, "$1• ")
			t = markBold(t)
			t = mdItalic.ReplaceAllString(t, "${1}_${2}_$3")
			t = mdStrike.ReplaceAllString(t, "~$1~")
			t = mdLink.ReplaceAllString(t, "<$2|$1>")
			b.WriteString(strings.ReplaceAll(t, boldMarker, "*"))
		}
	}
	return b.String()
}

// FormatWhatsApp converts Markdown to WhatsApp's markup.
func FormatWhatsApp(md string) string {
	var b strings.Builder
	for _, s := range segments(md) {
		switch s.kind {
		case segFence:
			b.WriteString("```\n" + s.text + "\n```")
		case segInline:
			b.WriteString("`" + s.text + "`")
		default:
			t := mdHeader.ReplaceAllString(s.text, boldMarker+"$1"+boldMarker)
			t = mdBullet.ReplaceAllString(t, "$1• ")
			t = markBold(t)
			t = mdItalic.ReplaceAllString(t, "${1}_${2}_$3")
			t = mdStrike.ReplaceAllString(t, "~$1~")
			t = mdLink.ReplaceAllStringFunc(t, func(x string) string {
				m := mdLink.FindStringSubmatch(x)
				if m[1] == m[2] {
					return m[2]
				}
				return m[1] + " (" + m[2] + ")"
			})
			b.WriteString(strings.ReplaceAll(t, boldMarker, "*"))
		}
	}
	return b.String()
}

// FormatTelegram converts Markdown to Telegram's HTML parse mode.
func FormatTelegram(md string) string {
	var b strings.Builder
	for _, s := range segments(md) {
		switch s.kind {
		case segFence:
			if s.lang != "" {
				b.WriteString(`<pre><code class="language-` + html.EscapeString(s.lang) + `">` + html.EscapeString(s.text) + "</code></pre>")
			} else {
				b.WriteString("<pre>" + html.EscapeString(s.text) + "</pre>")
			}
		case segInline:
			b.WriteString("<code>" + html.EscapeString(s.text) + "</code>")
		default:
			t := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s.text)
			t = mdHeader.ReplaceAllString(t, "<b>$1</b>")
			t = mdBullet.ReplaceAllString(t, "$1• ")
			t = mdBold.ReplaceAllStringFunc(t, func(x string) string {
				return "<b>" + firstGroup(mdBold.FindStringSubmatch(x)) + "</b>"
			})
			t = mdItalic.ReplaceAllString(t, "$1<i>$2</i>$3")
			t = mdUItalic.ReplaceAllString(t, "$1<i>$2</i>$3")
			t = mdStrike.ReplaceAllString(t, "<s>$1</s>")
			t = mdLink.ReplaceAllStringFunc(t, func(x string) string {
				m := mdLink.FindStringSubmatch(x)
				return `<a href="` + strings.ReplaceAll(m[2], `"`, "&quot;") + `">` + m[1] + "</a>"
			})
			b.WriteString(t)
		}
	}
	return b.String()
}

// Split breaks Markdown into chunks of at most limit characters, preferring
// paragraph and line boundaries. A code fence cut in two is closed at the end
// of one chunk and reopened at the start of the next.
func Split(md string, limit int) []string {
	md = strings.TrimSpace(md)
	if utf8.RuneCountInString(md) <= limit {
		if md == "" {
			return nil
		}
		return []string{md}
	}
	var chunks []string
	var cur []string
	curLen := 0
	fence := "" // opening line of the fence we are inside, if any
	flush := func() {
		text := strings.TrimSpace(strings.Join(cur, "\n"))
		if fence != "" {
			text += "\n```"
		}
		if text != "" && text != "```" {
			chunks = append(chunks, text)
		}
		cur, curLen = nil, 0
		if fence != "" {
			cur, curLen = []string{fence}, utf8.RuneCountInString(fence)+1
		}
	}
	budget := limit - 4 // room to close a fence
	for _, line := range strings.Split(md, "\n") {
		n := utf8.RuneCountInString(line) + 1
		if curLen+n > budget && len(cur) > 0 {
			// Prefer to break at the last blank line outside a fence.
			if i := lastBlank(cur); fence == "" && i > 0 && curLen > budget/2 {
				rest := cur[i+1:]
				cur = cur[:i]
				flush()
				for _, l := range rest {
					cur = append(cur, l)
					curLen += utf8.RuneCountInString(l) + 1
				}
			} else {
				flush()
			}
		}
		for utf8.RuneCountInString(line) > budget {
			r := []rune(line)
			cut := budget - curLen
			if cut < budget/2 {
				flush()
				cut = budget - curLen
			}
			cur = append(cur, string(r[:cut]))
			curLen += cut
			flush()
			line = string(r[cut:])
		}
		cur = append(cur, line)
		curLen += utf8.RuneCountInString(line) + 1
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "```") {
			if fence == "" {
				fence = t
			} else if t == "```" {
				fence = ""
			}
		}
	}
	fence = ""
	flush()
	return chunks
}

func lastBlank(lines []string) int {
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			return i
		}
	}
	return -1
}
