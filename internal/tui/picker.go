package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// picker is a full-height list: recent sessions, or agents. Typing filters
// it.
type picker struct {
	title   string
	items   []pickItem
	filter  string
	sel     int
	startup bool // Esc starts a fresh session instead of going back
}

type pickItem struct {
	label, detail string
	session       *SessionInfo // a session to open
	agent         string       // or an agent to start a session with
}

func sessionPicker(list []SessionInfo, now time.Time, startup bool) *picker {
	p := &picker{title: "Resume a session", startup: startup}
	for i := range list {
		s := list[i]
		title := s.Title
		if title == "" {
			title = "(untitled)"
		}
		p.items = append(p.items, pickItem{label: title, detail: fmt.Sprintf("%s · %s · %s", s.Agent, ago(now.Sub(s.UpdatedAt)), s.ID), session: &s})
	}
	return p
}

func agentPicker(agents []string, current string) *picker {
	p := &picker{title: "Talk to which agent?"}
	for _, a := range agents {
		detail := ""
		if a == current {
			detail = "this session"
		}
		p.items = append(p.items, pickItem{label: a, detail: detail, agent: a})
	}
	return p
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func (p *picker) visible() []pickItem {
	if p.filter == "" {
		return p.items
	}
	f := strings.ToLower(p.filter)
	var out []pickItem
	for _, it := range p.items {
		if strings.Contains(strings.ToLower(it.label+" "+it.detail), f) {
			out = append(out, it)
		}
	}
	return out
}

func (p *picker) move(d int) {
	n := len(p.visible())
	if n == 0 {
		p.sel = 0
		return
	}
	p.sel = min(max(p.sel+d, 0), n-1)
}

func (m *Model) pickerKey(k tea.KeyMsg) tea.Cmd {
	p := m.picker
	switch k.String() {
	case "up", "ctrl+p":
		p.move(-1)
	case "down", "ctrl+n":
		p.move(1)
	case "pgup":
		p.move(-10)
	case "pgdown":
		p.move(10)
	case "esc":
		m.picker = nil
		if p.startup || m.sess.ID == "" {
			return m.newSession(m.opts.Agent, "")
		}
	case "enter":
		items := p.visible()
		if len(items) == 0 {
			return nil
		}
		it := items[p.sel]
		m.picker = nil
		if it.session != nil {
			return m.open(*it.session, "Resumed this session.")
		}
		if it.agent == m.sess.Agent && m.sess.ID != "" {
			return nil
		}
		return m.newSession(it.agent, "You're now talking to "+it.agent+", in a fresh session.")
	case "backspace":
		if p.filter != "" {
			r := []rune(p.filter)
			p.filter = string(r[:len(r)-1])
			p.sel = 0
		}
	default:
		if k.Type == tea.KeyRunes || k.Type == tea.KeySpace {
			p.filter += string(k.Runes)
			p.sel = 0
		}
	}
	return nil
}

func (p *picker) view(t Theme, width, height int) string {
	lines := []string{t.Accent.Render(p.title)}
	hint := "↑/↓ choose · Enter open · type to filter · Esc "
	if p.startup {
		hint += "new session"
	} else {
		hint += "back"
	}
	if p.filter != "" {
		lines = append(lines, "filter: "+p.filter)
	} else {
		lines = append(lines, t.Dim.Render(hint))
	}
	items := p.visible()
	rows := max(height-len(lines)-1, 1)
	start := max(0, min(p.sel-rows/2, len(items)-rows))
	lines = append(lines, "")
	for i := start; i < len(items) && i < start+rows; i++ {
		it := items[i]
		row := ansi.Truncate(" "+it.label, width*3/5, "…")
		row += strings.Repeat(" ", max(2, width*3/5+2-ansi.StringWidth(row)))
		row = ansi.Truncate(row+it.detail, width-1, "…")
		if i == p.sel {
			row = t.Selected.Render(row + strings.Repeat(" ", max(0, width-1-ansi.StringWidth(row))))
		} else if it.detail != "" {
			label, _, _ := strings.Cut(row, it.detail)
			row = label + t.Dim.Render(strings.TrimPrefix(row, label))
		}
		lines = append(lines, row)
	}
	if len(items) == 0 {
		lines = append(lines, t.Dim.Render(" nothing matches"))
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	return strings.Join(lines[:height], "\n")
}
