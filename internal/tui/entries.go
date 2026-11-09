package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

type entryKind int

const (
	kindUser entryKind = iota
	kindAssistant
	kindThinking
	kindTool
	kindNotice
	kindError
)

type toolStatus int

const (
	toolRunning toolStatus = iota
	toolWaiting            // waiting for an approval
	toolOK
	toolFailed
	toolNoResult // from history: the call has no recorded result
)

// entry is one block of the transcript.
type entry struct {
	kind      entryKind
	text      string
	streaming bool // text is still arriving
	expanded  bool // tool output or thinking shown in full

	// Tool calls.
	toolID, tool string
	input        json.RawMessage
	status       toolStatus
	preview      string // one line of the result, from the live event
	output       string // the full result, from the transcript
	start, end   time.Time

	// Rendered Markdown, kept until the text or width changes. While text
	// streams in, it is re-rendered at most every streamRender.
	cache      string
	cacheWidth int
	stale      bool
	renderedAt time.Time
}

// renderer draws entries for one width.
type renderer struct {
	t     Theme
	md    *markdown
	width int
	now   time.Time
	frame string // spinner frame for running tools
	all   bool   // expand every tool card
	think bool   // show every thinking block

	pending bool // some streaming text is not drawn yet
}

func (r *renderer) render(e *entry) string {
	switch e.kind {
	case kindUser:
		return r.t.User.Render("› ") + wrap(e.text, r.width-2, "  ")
	case kindAssistant:
		if e.cache == "" || e.cacheWidth != r.width || e.stale && (!e.streaming || r.now.Sub(e.renderedAt) >= streamRender) {
			e.cache, e.cacheWidth, e.stale, e.renderedAt = r.md.render(e.text), r.width, false, r.now
		}
		if e.stale {
			r.pending = true
		}
		return e.cache
	case kindThinking:
		return r.thinking(e)
	case kindTool:
		return r.tool(e)
	case kindError:
		return r.t.Error.Render(wrap("✗ "+e.text, r.width, "  "))
	default:
		return r.t.Dim.Render(wrap(e.text, r.width, ""))
	}
}

func (r *renderer) thinking(e *entry) string {
	text := strings.TrimSpace(e.text)
	lines := strings.Split(text, "\n")
	switch {
	case e.expanded || r.think:
		return r.t.Thinking.Render("▾ thinking\n  " + wrap(text, r.width-2, "  "))
	case e.streaming:
		tail := lines[max(0, len(lines)-3):]
		body := "  " + wrap(strings.Join(tail, "\n"), r.width-2, "  ")
		return r.t.Thinking.Render("▾ thinking…\n" + lastLines(body, 3))
	}
	return r.t.Thinking.Render(fmt.Sprintf("▸ thinking (%d line%s; click or Ctrl+T to show)", len(lines), plural(len(lines))))
}

const maxToolLines = 200

func (r *renderer) tool(e *entry) string {
	var icon string
	style := r.t.Dim
	switch e.status {
	case toolRunning:
		icon, style = r.frame, r.t.Accent
	case toolWaiting:
		icon, style = "?", r.t.Warn
	case toolOK:
		icon, style = "✓", r.t.OK
	case toolFailed:
		icon, style = "✗", r.t.Error
	default:
		icon = "·"
	}
	dur := ""
	switch {
	case !e.start.IsZero() && !e.end.IsZero():
		dur = duration(e.end.Sub(e.start))
	case !e.start.IsZero() && e.status == toolRunning:
		dur = duration(r.now.Sub(e.start))
	}
	head := style.Render(icon) + " " + r.t.Accent.Render(e.tool)
	room := r.width - ansi.StringWidth(head) - len(dur) - 3
	if in := summarizeInput(e.input); in != "" && room > 4 {
		head += " " + ansi.Truncate(in, room, "…")
	}
	if dur != "" {
		head += "  " + r.t.Dim.Render(dur)
	}
	out := e.output
	if out == "" {
		out = e.preview
	}
	if !e.expanded && !r.all {
		if line := firstLine(out); line != "" && e.status != toolRunning {
			return head + "\n" + r.t.Dim.Render("  └ "+ansi.Truncate(line, r.width-5, "…"))
		}
		return head
	}
	var b strings.Builder
	b.WriteString(head)
	bar := r.t.Border.Render("  │ ")
	section := func(title, body string) {
		b.WriteString("\n" + r.t.Dim.Render("  ┌ "+title))
		lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
		cut := 0
		if len(lines) > maxToolLines {
			cut, lines = len(lines)-maxToolLines, lines[:maxToolLines]
		}
		for _, l := range lines {
			b.WriteString("\n" + bar + ansi.Truncate(expandTabs(l), r.width-4, "…"))
		}
		if cut > 0 {
			b.WriteString("\n" + r.t.Dim.Render(fmt.Sprintf("  └ %d more lines", cut)))
		}
	}
	if len(e.input) > 0 {
		section("input", prettyJSON(e.input))
	}
	switch {
	case e.output != "":
		section("output", e.output)
	case e.preview != "":
		section("output (preview)", e.preview)
	}
	return b.String()
}

// summarizeInput picks the field that says what a call does (a command, a
// path, a URL), or the compact JSON.
func summarizeInput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		for _, k := range []string{"command", "cmd", "path", "url", "query", "pattern", "question", "name", "title", "task", "text"} {
			if s, ok := m[k].(string); ok && s != "" {
				return oneLine(s)
			}
		}
	}
	var b bytes.Buffer
	if json.Compact(&b, raw) == nil {
		return b.String()
	}
	return oneLine(string(raw))
}

func prettyJSON(raw json.RawMessage) string {
	var b bytes.Buffer
	if json.Indent(&b, raw, "", "  ") == nil {
		return b.String()
	}
	return string(raw)
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}

func expandTabs(s string) string { return strings.ReplaceAll(s, "\t", "    ") }

// wrap breaks s to width, indenting continuation lines.
func wrap(s string, width int, indent string) string {
	width = max(width, 10)
	w := ansi.Wrap(expandTabs(s), width, "")
	return strings.ReplaceAll(w, "\n", "\n"+indent)
}

func duration(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
