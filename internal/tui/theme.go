package tui

import (
	"os"
	"strings"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// Theme holds the styles. Colors adapt to a light or dark background, and
// NoColor (NO_COLOR set, or a terminal without colors) drops them all.
type Theme struct {
	Dark    bool
	NoColor bool

	User, Accent, Dim, Thinking, Error, OK, Warn, Status, StatusBusy, Selected, Border lipgloss.Style

	ok bool // built by NewTheme
}

// DetectTheme asks the terminal for its background (which works over SSH
// when the terminal answers) and honors NO_COLOR. It sets lipgloss's color
// profile to match, so call it before the program starts.
func DetectTheme() Theme {
	noColor := os.Getenv("NO_COLOR") != "" || lipgloss.ColorProfile() == termenv.Ascii
	if noColor {
		lipgloss.SetColorProfile(termenv.Ascii)
	}
	return NewTheme(lipgloss.HasDarkBackground(), noColor)
}

// NewTheme builds the styles for a background.
func NewTheme(dark, noColor bool) Theme {
	t := Theme{Dark: dark, NoColor: noColor, ok: true}
	c := func(light, darkc string) lipgloss.TerminalColor {
		if noColor {
			return lipgloss.NoColor{}
		}
		if dark {
			return lipgloss.Color(darkc)
		}
		return lipgloss.Color(light)
	}
	s := lipgloss.NewStyle
	t.User = s().Foreground(c("25", "117")).Bold(true)
	t.Accent = s().Foreground(c("90", "213")).Bold(true)
	t.Dim = s().Foreground(c("244", "245"))
	t.Thinking = s().Foreground(c("245", "243")).Italic(true)
	t.Error = s().Foreground(c("160", "203"))
	t.OK = s().Foreground(c("28", "114"))
	t.Warn = s().Foreground(c("130", "221"))
	t.Status = s().Foreground(c("236", "252")).Background(c("254", "236"))
	t.StatusBusy = s().Foreground(c("90", "213")).Background(c("254", "236")).Bold(true)
	t.Selected = s().Foreground(c("231", "16")).Background(c("90", "213")).Bold(true)
	t.Border = s().Foreground(c("247", "240"))
	if noColor {
		t.Selected = s().Reverse(true)
		t.Status = s().Reverse(true)
		t.StatusBusy = s().Reverse(true).Bold(true)
	}
	return t
}

// markdown renders Markdown for one width, with highlighted code blocks.
type markdown struct {
	r     *glamour.TermRenderer
	width int
}

func newMarkdown(t Theme, width int) *markdown {
	style := styles.LightStyle
	switch {
	case t.NoColor:
		style = styles.NoTTYStyle
	case t.Dark:
		style = styles.DarkStyle
	}
	opts := []glamour.TermRendererOption{glamour.WithStandardStyle(style), glamour.WithWordWrap(max(width-4, 20))}
	if !t.NoColor {
		opts = append(opts, glamour.WithColorProfile(lipgloss.ColorProfile()))
	} else {
		opts = append(opts, glamour.WithColorProfile(termenv.Ascii))
	}
	r, err := glamour.NewTermRenderer(opts...)
	if err != nil {
		return &markdown{width: width}
	}
	return &markdown{r: r, width: width}
}

// render renders text, falling back to the text itself.
func (md *markdown) render(text string) string {
	if md.r == nil {
		return text
	}
	out, err := md.r.Render(text)
	if err != nil {
		return text
	}
	return strings.Trim(out, "\n")
}
