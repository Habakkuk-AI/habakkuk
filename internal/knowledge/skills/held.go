package skills

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/frontmatter"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory/history"
)

// Held skills. A skill saved in a turn that read outside content (a web
// page, an email, a tool's output) or that came from someone other than
// the owner could carry planted text into every later prompt that lists
// it. So skill_save puts such a skill in agents/<agent>/skills-held/,
// which nothing lists or loads, and the owner looks at it and moves it in
// with hk skills approve.

// HeldFolder is the folder name, beside an agent's skills folder.
const HeldFolder = "skills-held"

// HeldSkill is a skill waiting for the owner.
type HeldSkill struct {
	Agent       string
	Name        string
	Description string
	Dir         string
	Saved       time.Time
}

// HeldDir is where an agent's held skills wait.
func (l *Library) HeldDir(agent string) string {
	return filepath.Join(l.Home, "agents", agent, HeldFolder)
}

// SaveHeld writes a skill to the agent's held folder instead of its
// skills, replacing a held one of the same name.
func (l *Library) SaveHeld(agent, name, description, body string) (HeldSkill, error) {
	if err := ValidName(name); err != nil {
		return HeldSkill{}, err
	}
	if _, err := l.agentDir(agent); err != nil {
		return HeldSkill{}, err
	}
	description = strings.Join(strings.Fields(description), " ")
	body = strings.TrimSpace(body)
	switch {
	case description == "":
		return HeldSkill{}, errors.New("description is empty: say in one line what the skill is for and when to use it")
	case len(description) > MaxDescription:
		return HeldSkill{}, fmt.Errorf("description is longer than %d characters", MaxDescription)
	case body == "":
		return HeldSkill{}, errors.New("body is empty")
	case len(body) > MaxBody:
		return HeldSkill{}, fmt.Errorf("body is longer than %d bytes", MaxBody)
	}
	dir := filepath.Join(l.HeldDir(agent), name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return HeldSkill{}, err
	}
	data, err := frontmatter.Format(meta{Name: name, Description: description}, body)
	if err != nil {
		return HeldSkill{}, err
	}
	if err := store.WriteFileAtomic(filepath.Join(dir, FileName), data, 0o600); err != nil {
		return HeldSkill{}, err
	}
	return HeldSkill{Agent: agent, Name: name, Description: description, Dir: dir, Saved: time.Now()}, nil
}

// Held lists every agent's held skills, by agent and name.
func (l *Library) Held() ([]HeldSkill, error) {
	agents, err := os.ReadDir(filepath.Join(l.Home, "agents"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []HeldSkill
	for _, a := range agents {
		if !a.IsDir() {
			continue
		}
		entries, _ := os.ReadDir(l.HeldDir(a.Name()))
		for _, e := range entries {
			if !e.IsDir() || ValidName(e.Name()) != nil {
				continue
			}
			dir := filepath.Join(l.HeldDir(a.Name()), e.Name())
			path := filepath.Join(dir, FileName)
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			var m meta
			_, _ = frontmatter.Parse(data, &m)
			h := HeldSkill{Agent: a.Name(), Name: e.Name(), Description: m.Description, Dir: dir}
			if info, err := os.Stat(path); err == nil {
				h.Saved = info.ModTime()
			}
			out = append(out, h)
		}
	}
	slices.SortFunc(out, func(a, b HeldSkill) int { return strings.Compare(a.Agent+"/"+a.Name, b.Agent+"/"+b.Name) })
	return out, nil
}

// FindHeld finds a held skill by NAME or AGENT/NAME. ok is false when
// there is none; a name held for several agents needs the AGENT/ form.
func (l *Library) FindHeld(ref string) (h HeldSkill, ok bool, err error) {
	agent, name, scoped := strings.Cut(ref, "/")
	if !scoped {
		agent, name = "", ref
	}
	held, err := l.Held()
	if err != nil {
		return h, false, err
	}
	var hits []HeldSkill
	for _, x := range held {
		if x.Name == name && (agent == "" || x.Agent == agent) {
			hits = append(hits, x)
		}
	}
	switch len(hits) {
	case 0:
		return h, false, nil
	case 1:
		return hits[0], true, nil
	}
	var refs []string
	for _, x := range hits {
		refs = append(refs, x.Agent+"/"+x.Name)
	}
	return h, false, fmt.Errorf("%s is held for several agents; name one: %s", name, strings.Join(refs, ", "))
}

// ApproveHeld moves a held skill into its agent's skills, as the owner's
// change, and removes it from the held folder.
func (l *Library) ApproveHeld(ctx context.Context, h HeldSkill) (Skill, error) {
	data, err := os.ReadFile(filepath.Join(h.Dir, FileName))
	if err != nil {
		return Skill{}, err
	}
	var m meta
	body, err := frontmatter.Parse(data, &m)
	if err != nil {
		return Skill{}, err
	}
	ctx = history.WithActor(ctx, history.Actor{By: history.ByUser, Tool: "hk skills approve"})
	s, _, err := l.SaveContext(ctx, h.Agent, h.Name, m.Description, string(body))
	if err != nil {
		return Skill{}, err
	}
	return s, os.RemoveAll(h.Dir)
}
