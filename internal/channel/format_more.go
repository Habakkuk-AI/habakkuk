package channel

import "strings"

// FormatPlain turns Markdown into plain text for platforms with no markup
// (SMS, iMessage, IRC, LINE): markers go, links show their URL.
func FormatPlain(md string) string {
	var b strings.Builder
	for _, s := range segments(md) {
		switch s.kind {
		case segFence:
			b.WriteString(s.text)
		case segInline:
			b.WriteString(s.text)
		default:
			t := mdHeader.ReplaceAllString(s.text, "$1")
			t = mdBullet.ReplaceAllString(t, "$1• ")
			t = mdBold.ReplaceAllStringFunc(t, func(x string) string { return firstGroup(mdBold.FindStringSubmatch(x)) })
			t = mdItalic.ReplaceAllString(t, "$1$2$3")
			t = mdStrike.ReplaceAllString(t, "$1")
			t = mdLink.ReplaceAllStringFunc(t, func(x string) string {
				m := mdLink.FindStringSubmatch(x)
				if m[1] == m[2] {
					return m[2]
				}
				return m[1] + " (" + m[2] + ")"
			})
			b.WriteString(t)
		}
	}
	return b.String()
}

// FormatGoogleChat converts Markdown to Google Chat's text markup: like
// Slack's, but Chat shows text as written, so nothing is escaped.
func FormatGoogleChat(md string) string {
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
			t = mdLink.ReplaceAllString(t, "<$2|$1>")
			b.WriteString(strings.ReplaceAll(t, boldMarker, "*"))
		}
	}
	return b.String()
}
