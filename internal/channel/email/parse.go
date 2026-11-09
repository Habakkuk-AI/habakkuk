package email

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"regexp"
	"strings"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // decode non-UTF-8 mail
	"github.com/emersion/go-message/mail"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// incoming is one parsed email.
type incoming struct {
	ID         string // Message-ID without brackets
	From       string // lower-cased address
	FromName   string
	Subject    string
	InReplyTo  []string
	References []string
	Text       string // plain text, quoted history stripped
	Files      []file
	Automated  string // why the message is automatic mail, or ""
	Authentic  bool   // the receiving server recorded a passing DMARC or aligned DKIM
}

type file struct {
	Name      string
	MediaType string
	Data      []byte
}

// root is the thread's first message: the head of References, else the
// message being answered, else the message itself.
func (m *incoming) root() string {
	switch {
	case len(m.References) > 0:
		return m.References[0]
	case len(m.InReplyTo) > 0:
		return m.InReplyTo[0]
	}
	return m.ID
}

const maxParts = 50 // attachments kept from one message

func parse(raw []byte) (*incoming, error) {
	r, err := mail.CreateReader(bytes.NewReader(raw))
	if err != nil && !message.IsUnknownCharset(err) {
		return nil, err
	}
	h := r.Header
	m := &incoming{}
	m.ID, _ = h.MessageID()
	if from, err := h.AddressList("From"); err == nil && len(from) > 0 {
		m.From, m.FromName = strings.ToLower(from[0].Address), from[0].Name
	}
	m.Subject, _ = h.Subject()
	m.InReplyTo, _ = h.MsgIDList("In-Reply-To")
	m.References, _ = h.MsgIDList("References")
	m.Automated = automated(h)
	m.Authentic = authentic(h, m.From)

	var plain, htm string
	for len(m.Files) < maxParts {
		p, err := r.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil && !message.IsUnknownCharset(err) {
			break // keep what was read; a broken tail should not lose the text
		}
		data, rerr := io.ReadAll(io.LimitReader(p.Body, maxMessage))
		if rerr != nil {
			continue
		}
		switch ph := p.Header.(type) {
		case *mail.InlineHeader:
			ct, _, _ := ph.ContentType()
			name := partName(ph.Header)
			switch {
			case ct == "text/plain" && name == "" && plain == "":
				plain = string(data)
			case ct == "text/html" && name == "" && htm == "":
				htm = string(data)
			case !strings.HasPrefix(ct, "text/") || name != "":
				m.Files = append(m.Files, file{Name: fallbackName(name, ct), MediaType: ct, Data: data})
			}
		case *mail.AttachmentHeader:
			ct, _, _ := ph.ContentType()
			name, _ := ph.Filename()
			m.Files = append(m.Files, file{Name: fallbackName(name, ct), MediaType: ct, Data: data})
		}
	}
	text := plain
	if strings.TrimSpace(text) == "" && htm != "" {
		text = htmlText(htm)
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if stripped := stripQuoted(text); stripped != "" {
		text = stripped
	}
	m.Text = strings.TrimSpace(text)
	return m, nil
}

func partName(h message.Header) string {
	if _, params, err := h.ContentDisposition(); err == nil && params["filename"] != "" {
		return params["filename"]
	}
	if _, params, err := h.ContentType(); err == nil {
		return params["name"]
	}
	return ""
}

func fallbackName(name, mediaType string) string {
	if name != "" {
		return name
	}
	if exts, _ := mime.ExtensionsByType(mediaType); len(exts) > 0 {
		return "attachment" + exts[0]
	}
	return "attachment"
}

// automated says why a message is automatic (auto-replies, bounces, lists
// and bulk mail), which Habakkuk never answers, or "" for a person's mail.
func automated(h mail.Header) string {
	if v := strings.ToLower(strings.TrimSpace(h.Get("Auto-Submitted"))); v != "" && v != "no" {
		return "Auto-Submitted: " + v
	}
	switch v := strings.ToLower(strings.TrimSpace(h.Get("Precedence"))); v {
	case "bulk", "junk", "list", "auto_reply":
		return "Precedence: " + v
	}
	for _, k := range []string{"List-Id", "List-Unsubscribe", "X-Autoreply", "X-Autorespond", "X-Auto-Response"} {
		if h.Get(k) != "" {
			return k
		}
	}
	if from, err := h.AddressList("From"); err == nil && len(from) > 0 {
		local, _, _ := strings.Cut(strings.ToLower(from[0].Address), "@")
		switch local {
		case "mailer-daemon", "postmaster":
			return "From: " + local
		}
	}
	if strings.EqualFold(h.Get("Content-Type"), "multipart/report") ||
		strings.HasPrefix(strings.ToLower(h.Get("Content-Type")), "multipart/report;") {
		return "delivery report"
	}
	return ""
}

var (
	authDMARC = regexp.MustCompile(`(?i)\bdmarc\s*=\s*pass\b`)
	authDKIM  = regexp.MustCompile(`(?i)\bdkim\s*=\s*pass\b[^;]*?\bheader\.(?:d|i)\s*=\s*@?([a-z0-9.-]+)`)
)

// authentic reports whether the receiving server's Authentication-Results
// (the topmost one: servers prepend theirs) shows DMARC passing, or DKIM
// passing for the From domain or a parent of it.
func authentic(h mail.Header, from string) bool {
	results := h.Values("Authentication-Results")
	if len(results) == 0 || from == "" {
		return false
	}
	top := results[0]
	if authDMARC.MatchString(top) {
		return true
	}
	_, domain, _ := strings.Cut(from, "@")
	for _, m := range authDKIM.FindAllStringSubmatch(top, -1) {
		d := strings.ToLower(m[1])
		if domain == d || strings.HasSuffix(domain, "."+d) {
			return true
		}
	}
	return false
}

var (
	quoteHeader = regexp.MustCompile(`^(On|Le|Am|El|Il|Op)\s.+(wrote|écrit|schrieb|escribió|scritto|geschreven)\s*:\s*$`)
	quoteStart  = regexp.MustCompile(`^On\s.{4,}`)
	outlookSep  = regexp.MustCompile(`^(-{3,}\s*Original Message\s*-{3,}|_{20,})\s*$`)
	headerLine  = regexp.MustCompile(`^\*?(From|Sent|Date|To|Subject)\*?:\s`)
)

// stripQuoted cuts the quoted history from a reply: everything from an
// "On ... wrote:" line, an Outlook "Original Message" block or a From/Sent
// header block, a trailing run of "> " lines, and the signature after "-- ".
func stripQuoted(text string) string {
	lines := strings.Split(text, "\n")
	cut := len(lines)
	for i, line := range lines {
		t := strings.TrimSpace(line)
		switch {
		case quoteHeader.MatchString(t),
			quoteStart.MatchString(t) && i+1 < len(lines) && quoteHeader.MatchString(t+" "+strings.TrimSpace(lines[i+1])),
			outlookSep.MatchString(t),
			line == "-- ",
			headerLine.MatchString(t) && strings.HasPrefix(t, "From") && i+1 < len(lines) && headerLine.MatchString(strings.TrimSpace(lines[i+1])):
			cut = i
		}
		if cut < len(lines) {
			break
		}
	}
	lines = lines[:cut]
	// Drop the trailing quote block; quotes between answers stay as context.
	end := len(lines)
	for end > 0 {
		t := strings.TrimSpace(lines[end-1])
		if t != "" && !strings.HasPrefix(t, ">") {
			break
		}
		end--
	}
	return strings.TrimSpace(strings.Join(lines[:end], "\n"))
}

// htmlText renders an HTML body as plain text. Quoted history inside a
// blockquote or a mail client's quote container is left out.
func htmlText(src string) string {
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		return ""
	}
	var b strings.Builder
	var walk func(n *html.Node) bool // false stops the walk (reply history began)
	walk = func(n *html.Node) bool {
		if n.Type == html.TextNode {
			b.WriteString(strings.Join(strings.Fields(n.Data), " "))
			if strings.HasSuffix(n.Data, " ") || strings.HasSuffix(n.Data, "\n") {
				b.WriteString(" ")
			}
			return true
		}
		if n.Type == html.ElementNode {
			switch n.DataAtom {
			case atom.Script, atom.Style, atom.Head, atom.Title, atom.Blockquote:
				return true
			}
			for _, a := range n.Attr {
				if (a.Key == "class" && (strings.Contains(a.Val, "gmail_quote") || strings.Contains(a.Val, "moz-cite-prefix") || strings.Contains(a.Val, "yahoo_quoted"))) ||
					(a.Key == "id" && (a.Val == "divRplyFwdMsg" || a.Val == "appendonsend")) {
					return false
				}
			}
			switch n.DataAtom {
			case atom.Br:
				b.WriteString("\n")
			case atom.Li:
				b.WriteString("\n• ")
			case atom.P, atom.Div, atom.Tr, atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6, atom.Ul, atom.Ol, atom.Table, atom.Hr:
				b.WriteString("\n")
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if !walk(c) {
				return false
			}
		}
		if n.Type == html.ElementNode {
			switch n.DataAtom {
			case atom.A:
				for _, a := range n.Attr {
					if a.Key == "href" && strings.HasPrefix(a.Val, "http") && !strings.Contains(textOf(n), a.Val) {
						b.WriteString(" (" + a.Val + ")")
					}
				}
			case atom.P, atom.Div, atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
				b.WriteString("\n")
			}
		}
		return true
	}
	walk(doc)
	// Collapse the blank lines block elements leave behind.
	var out []string
	blank := 0
	for _, line := range strings.Split(b.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func textOf(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}
