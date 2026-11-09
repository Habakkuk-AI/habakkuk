package session

import (
	"encoding/json"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// Export formats.
const (
	FormatMarkdown = "md"
	FormatJSON     = "json"
	FormatHTML     = "html"
)

// Export renders a transcript as Markdown, JSON or a standalone HTML page.
func Export(sess *Session, recs []Record, format string, now time.Time) ([]byte, error) {
	switch format {
	case FormatMarkdown, "markdown", "":
		return []byte(ExportMarkdown(sess, recs, now)), nil
	case FormatJSON:
		return exportJSON(sess, recs, now)
	case FormatHTML:
		return []byte(exportHTML(sess, recs, now)), nil
	}
	return nil, fmt.Errorf("format %q must be md, json or html", format)
}

func exportTitle(sess *Session) string {
	if sess.Title != "" {
		return sess.Title
	}
	return "Conversation " + sess.ID
}

// part is one rendered block of a message.
type part struct {
	kind string // text | call | result | error | media
	tool string
	text string
}

// parts renders a message's blocks; names maps tool call ids to tool names
// and is filled as calls go by.
func parts(m llm.Message, names map[string]string) []part {
	var out []part
	for _, bl := range m.Blocks {
		switch bl.Type {
		case llm.Text:
			if t := strings.TrimSpace(bl.Text); t != "" {
				out = append(out, part{kind: "text", text: t})
			}
		case llm.ToolUse:
			names[bl.ToolUseID] = bl.ToolName
			out = append(out, part{kind: "call", tool: bl.ToolName, text: cutText(string(bl.Input), 2000)})
		case llm.ToolResult:
			k := "result"
			if bl.IsError {
				k = "error"
			}
			out = append(out, part{kind: k, tool: names[bl.ToolUseID], text: cutText(bl.Text, 4000)})
		case llm.Image, llm.Document:
			out = append(out, part{kind: "media", text: fmt.Sprintf("%s %s", bl.Type, bl.Name)})
		}
	}
	return out
}

// speaker names who wrote a message: the user, the agent, or the tools.
func speaker(sess *Session, m llm.Message) string {
	switch {
	case m.Role == llm.Assistant:
		return sess.Agent
	case len(m.Blocks) > 0 && m.Blocks[0].Type == llm.ToolResult:
		return "Tools"
	}
	return "User"
}

// ExportMarkdown renders a transcript as a Markdown document.
func ExportMarkdown(sess *Session, recs []Record, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\nSession `%s` with agent **%s**, started %s, exported %s.\n",
		exportTitle(sess), sess.ID, sess.Agent, sess.CreatedAt.Format("2006-01-02 15:04"), now.Format("2006-01-02 15:04"))
	names := map[string]string{}
	for _, r := range recs {
		ps := parts(r.Message, names)
		if len(ps) == 0 {
			continue
		}
		out := make([]string, len(ps))
		for i, p := range ps {
			switch p.kind {
			case "text":
				out[i] = p.text
			case "call":
				out[i] = fmt.Sprintf("**Tool call `%s`**\n\n```json\n%s\n```", p.tool, p.text)
			case "result", "error":
				label := "Result"
				if p.kind == "error" {
					label = "Error"
				}
				out[i] = fmt.Sprintf("**%s of `%s`**\n\n```\n%s\n```", label, p.tool, strings.ReplaceAll(p.text, "```", "'''"))
			case "media":
				out[i] = "_(" + p.text + ")_"
			}
		}
		fmt.Fprintf(&b, "\n## %s\n\n%s\n", speaker(sess, r.Message), strings.Join(out, "\n\n"))
	}
	return b.String()
}

// exportJSON keeps the blocks as stored, so the export is complete.
func exportJSON(sess *Session, recs []Record, now time.Time) ([]byte, error) {
	type msg struct {
		Seq    int         `json:"seq"`
		Role   llm.Role    `json:"role"`
		Model  string      `json:"model,omitempty"`
		Usage  *llm.Usage  `json:"usage,omitempty"`
		Blocks []llm.Block `json:"blocks"`
	}
	out := struct {
		ID         string    `json:"id"`
		Agent      string    `json:"agent"`
		Title      string    `json:"title"`
		Parent     string    `json:"parent,omitempty"`
		CreatedAt  time.Time `json:"created_at"`
		UpdatedAt  time.Time `json:"updated_at"`
		ExportedAt time.Time `json:"exported_at"`
		Messages   []msg     `json:"messages"`
	}{ID: sess.ID, Agent: sess.Agent, Title: sess.Title, Parent: sess.Parent, CreatedAt: sess.CreatedAt,
		UpdatedAt: sess.UpdatedAt, ExportedAt: now, Messages: []msg{}}
	for _, r := range recs {
		out.Messages = append(out.Messages, msg{Seq: r.Seq, Role: r.Message.Role, Model: r.Model, Usage: r.Usage, Blocks: r.Message.Blocks})
	}
	return json.MarshalIndent(out, "", "  ")
}

const htmlStyle = `body{font:15px/1.5 system-ui,sans-serif;max-width:860px;margin:2rem auto;padding:0 1rem;color:#1d1d1f;background:#fff}
h1{font-size:1.4rem}.meta{color:#666;font-size:.85rem}.msg{margin:1rem 0;padding:.75rem 1rem;border-radius:8px;background:#f4f4f6}
.msg.assistant{background:#eef4ff}.msg.tools{background:#f7f7f2}.who{font-weight:600;font-size:.85rem;margin-bottom:.3rem}
pre{white-space:pre-wrap;word-break:break-word;background:#fff;border:1px solid #ddd;border-radius:6px;padding:.5rem;font-size:.8rem}
.text{white-space:pre-wrap}.error pre{border-color:#d33}.media{color:#666;font-style:italic}
@media (prefers-color-scheme:dark){body{background:#111;color:#eee}.msg{background:#1c1c1f}.msg.assistant{background:#172033}
.msg.tools{background:#1f1f18}pre{background:#000;border-color:#333}.meta,.media{color:#999}}`

// exportHTML renders a self-contained page with no scripts; every piece of
// transcript text is escaped.
func exportHTML(sess *Session, recs []Record, now time.Time) string {
	e := html.EscapeString
	var b strings.Builder
	title := exportTitle(sess)
	fmt.Fprintf(&b, "<!doctype html>\n<html><head><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width,initial-scale=1\">"+
		"<title>%s</title><style>%s</style></head><body>\n<h1>%s</h1>\n<p class=\"meta\">Session %s with agent %s, started %s, exported %s.</p>\n",
		e(title), htmlStyle, e(title), e(sess.ID), e(sess.Agent), sess.CreatedAt.Format("2006-01-02 15:04"), now.Format("2006-01-02 15:04"))
	names := map[string]string{}
	for _, r := range recs {
		ps := parts(r.Message, names)
		if len(ps) == 0 {
			continue
		}
		who := speaker(sess, r.Message)
		class := "user"
		switch {
		case r.Message.Role == llm.Assistant:
			class = "assistant"
		case who == "Tools":
			class = "tools"
		}
		fmt.Fprintf(&b, "<div class=\"msg %s\"><div class=\"who\">%s</div>\n", class, e(who))
		for _, p := range ps {
			switch p.kind {
			case "text":
				fmt.Fprintf(&b, "<div class=\"text\">%s</div>\n", e(p.text))
			case "call":
				fmt.Fprintf(&b, "<div>Tool call <code>%s</code><pre>%s</pre></div>\n", e(p.tool), e(p.text))
			case "result", "error":
				label := "Result"
				if p.kind == "error" {
					label = "Error"
				}
				fmt.Fprintf(&b, "<div class=\"%s\">%s of <code>%s</code><pre>%s</pre></div>\n", p.kind, label, e(p.tool), e(p.text))
			case "media":
				fmt.Fprintf(&b, "<div class=\"media\">(%s)</div>\n", e(p.text))
			}
		}
		b.WriteString("</div>\n")
	}
	b.WriteString("</body></html>\n")
	return b.String()
}
