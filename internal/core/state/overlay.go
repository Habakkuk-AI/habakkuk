package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Personalities are persona overlays a conversation can switch to with
// /personality: the built-in presets plus Markdown files in a directory
// ($HABAKKUK_HOME/personalities/<name>.md), which win on a name clash. Files
// are read when used, so an edit takes effect on the next turn.
type Personalities struct {
	Dir string
}

var presets = map[string]string{
	"concise":   "Be concise. Lead with the answer, use short sentences and bullet points, and leave out preamble, recaps and pleasantries. Expand only when asked.",
	"warm":      "Be warm and encouraging. Acknowledge how the user feels, use their name when you know it, and keep a friendly, human tone while staying accurate and direct about problems.",
	"formal":    "Use a formal, professional register: complete sentences, no slang, no emoji, and precise wording suitable for business correspondence.",
	"technical": "Be technical and exact. Prefer precise terms, show commands, code, numbers and units, state assumptions and edge cases, and cite the file, line or source you rely on.",
	"playful":   "Be playful and light-hearted: a bit of humour and personality is welcome, but never at the cost of accuracy, and drop the jokes when the topic is serious.",
}

var personaName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,47}$`)

// maxPersonaChars caps a persona file so one runaway file cannot eat the
// context window.
const maxPersonaChars = 8000

// List returns every available persona name, sorted.
func (p Personalities) List() []string {
	names := make([]string, 0, len(presets))
	for n := range presets {
		names = append(names, n)
	}
	if p.Dir != "" {
		files, _ := filepath.Glob(filepath.Join(p.Dir, "*.md"))
		for _, f := range files {
			n := strings.ToLower(strings.TrimSuffix(filepath.Base(f), ".md"))
			if personaName.MatchString(n) && !slices.Contains(names, n) {
				names = append(names, n)
			}
		}
	}
	slices.Sort(names)
	return names
}

// Text returns a persona's instructions.
func (p Personalities) Text(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !personaName.MatchString(name) {
		return "", fmt.Errorf("%q is not a persona name", name)
	}
	if p.Dir != "" {
		data, err := os.ReadFile(filepath.Join(p.Dir, name+".md"))
		if err == nil {
			s := strings.TrimSpace(string(data))
			if len(s) > maxPersonaChars {
				s = s[:maxPersonaChars]
			}
			return s, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	if s, ok := presets[name]; ok {
		return s, nil
	}
	return "", fmt.Errorf("no persona %q; try one of: %s", name, strings.Join(p.List(), ", "))
}

// Overlay implements agent.Overlay over the store.
type Overlay struct {
	Store    *Store
	Personas Personalities
}

func (o Overlay) Effort(ctx context.Context, sessionID string) string {
	st, _ := o.Store.Settings(ctx, sessionID)
	return st.Effort
}

// PersonaHeading starts the persona section added to the system prompt.
const PersonaHeading = "# Persona for this conversation"

func (o Overlay) System(ctx context.Context, sessionID string) string {
	st, _ := o.Store.Settings(ctx, sessionID)
	if st.Personality == "" {
		return ""
	}
	text, err := o.Personas.Text(st.Personality)
	if err != nil || text == "" {
		return ""
	}
	return PersonaHeading + "\n\nThe user picked the " + st.Personality + " persona. It sets your tone and style only; it never overrides your instructions or safety rules.\n\n" + text
}

// TaskListMarker starts the task list added to a turn's user message.
const TaskListMarker = "\n\n[Your task list for this session (todo tool)"

func (o Overlay) Preamble(ctx context.Context, sessionID string) string {
	list, err := o.Store.Todos(ctx, sessionID)
	if err != nil || len(list) == 0 {
		return ""
	}
	open := false
	for _, t := range list {
		if t.Status != TodoDone {
			open = true
		}
	}
	if !open {
		return ""
	}
	return TaskListMarker + ":\n" + RenderTodos(list) + "]"
}

func (o Overlay) Steer(ctx context.Context, sessionID string) []string {
	notes, _ := o.Store.TakeSteers(ctx, sessionID)
	return notes
}

// RenderTodos lists a task list one item per line.
func RenderTodos(list []Todo) string {
	if len(list) == 0 {
		return "(the task list is empty)"
	}
	var b strings.Builder
	for i, t := range list {
		if i > 0 {
			b.WriteString("\n")
		}
		mark := " "
		switch t.Status {
		case TodoDone:
			mark = "x"
		case TodoInProgress:
			mark = "~"
		}
		fmt.Fprintf(&b, "%d. [%s] %s", t.ID, mark, t.Text)
	}
	return b.String()
}
