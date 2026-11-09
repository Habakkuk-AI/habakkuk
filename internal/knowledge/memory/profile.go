package memory

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory/history"
)

// The user profile is agents/<agent>/USER.md: durable facts and preferences
// about the user, in "## Section" blocks, that go into every new session's
// system prompt. It is kept short (MaxProfile) so it never crowds the
// prompt, and it is edited one section at a time so a change cannot clobber
// the rest.

// MaxProfile caps USER.md; the prompt includes the whole file.
const MaxProfile = 6000

// Profile modes for UpdateProfile.
const (
	ProfileReplace = "replace" // the section's text becomes content
	ProfileAppend  = "append"  // content is added to the section, unless it is already there
	ProfileRemove  = "remove"  // the section is deleted
)

const maxSection = 60

// ProfilePath is an agent's USER.md.
func (s *Store) ProfilePath(agent string) string {
	return filepath.Join(s.AgentDir(agent), "USER.md")
}

// AgentDir is an agent's directory, which holds memory/ and USER.md.
func (s *Store) AgentDir(agent string) string {
	return filepath.Join(s.home, "agents", agent)
}

// Profile reads an agent's USER.md; "" when there is none.
func (s *Store) Profile(agent string) (string, error) {
	if _, err := s.dir(agent); err != nil {
		return "", err
	}
	data, err := os.ReadFile(s.ProfilePath(agent))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return strings.TrimSpace(string(data)), err
}

type section struct {
	name string // "" for text before the first heading
	body string
}

func parseProfile(text string) []section {
	var out []section
	cur := section{}
	var body []string
	flush := func() {
		cur.body = strings.TrimSpace(strings.Join(body, "\n"))
		if cur.name != "" || cur.body != "" {
			out = append(out, cur)
		}
		body = nil
	}
	for _, line := range strings.Split(text, "\n") {
		if name, ok := strings.CutPrefix(strings.TrimRight(line, " \t\r"), "## "); ok {
			flush()
			cur = section{name: strings.TrimSpace(name)}
			continue
		}
		body = append(body, line)
	}
	flush()
	return out
}

func formatProfile(secs []section) string {
	var b strings.Builder
	for i, sec := range secs {
		if i > 0 {
			b.WriteString("\n\n")
		}
		if sec.name != "" {
			b.WriteString("## " + sec.name)
			if sec.body != "" {
				b.WriteString("\n\n")
			}
		}
		b.WriteString(sec.body)
	}
	return strings.TrimSpace(b.String()) + "\n"
}

// UpdateProfile edits one section of an agent's USER.md and returns the new
// profile. Appending text the section already holds changes nothing, so the
// same update applied twice is applied once. Content that looks like a
// secret, and a profile that would grow past MaxProfile, are refused.
func (s *Store) UpdateProfile(ctx context.Context, agent, name, content, mode string) (string, error) {
	name = strings.Join(strings.Fields(strings.TrimLeft(name, "# ")), " ")
	content = strings.TrimSpace(content)
	switch {
	case name == "":
		return "", errors.New("section is empty: name the section, e.g. Preferences or Work")
	case len(name) > maxSection:
		return "", fmt.Errorf("section name is longer than %d characters", maxSection)
	}
	switch mode {
	case "":
		mode = ProfileAppend
	case ProfileAppend, ProfileReplace, ProfileRemove:
	default:
		return "", fmt.Errorf("mode %q must be append, replace or remove", mode)
	}
	if mode != ProfileRemove && content == "" {
		return "", errors.New("content is empty; to delete the section use mode remove")
	}
	if err := CheckSecrets(name + "\n" + content); err != nil {
		return "", err
	}
	if _, err := s.dir(agent); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, err := s.Profile(agent)
	if err != nil {
		return "", err
	}
	secs := parseProfile(old)
	idx := -1
	for i, sec := range secs {
		if strings.EqualFold(sec.name, name) {
			idx = i
			break
		}
	}
	switch {
	case mode == ProfileRemove && idx < 0:
		return old, fmt.Errorf("no section %q", name)
	case mode == ProfileRemove:
		secs = append(secs[:idx], secs[idx+1:]...)
	case idx < 0:
		secs = append(secs, section{name: name, body: content})
	case mode == ProfileReplace:
		secs[idx].body = content
	case strings.Contains(secs[idx].body, content):
		return old, nil // already there
	default:
		secs[idx].body = strings.TrimSpace(secs[idx].body + "\n" + content)
	}
	text := formatProfile(secs)
	if len(secs) == 0 {
		text = ""
	}
	if len(text) > MaxProfile {
		return old, fmt.Errorf("the profile would be %d bytes; it is capped at %d. Shorten or remove a section first", len(text), MaxProfile)
	}
	if err := s.writeTracked(ctx, history.ProfileTarget(s.home, agent), []byte(text)); err != nil {
		return "", err
	}
	return strings.TrimSpace(text), nil
}
