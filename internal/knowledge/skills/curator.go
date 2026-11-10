package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/frontmatter"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory/history"
)

// The curator keeps the skills agents write for themselves from piling up.
// Run on a schedule (skills.curate) or with `hk skills curate`, it
// looks at each agent's own skills (never shared, installed or plugin
// ones) and:
//
//   - merges near-duplicates: two skills with the same body, the same name
//     but for a plural, or descriptions that share most of their words.
//     The one used more recently keeps its name and description; the
//     other's instructions are appended under a heading and its supporting
//     files copied into merged/<name>/;
//   - archives skills nobody has loaded for skills.archive_after_days
//     (default 90), counting from their last change when never loaded.
//
// Every change is backed up first ($HABAKKUK_HOME/skills/.backups), so
// `hk skills restore NAME` undoes it, and recorded in the memory
// history when the library has one. Pinned skills are left alone.

// CurateOptions say what the curator looks at.
type CurateOptions struct {
	Agents       []string
	ArchiveAfter time.Duration // 0 never archives
	DryRun       bool          // report what would change, change nothing
	Now          time.Time
}

// CurateAction is one change the curator made (or would make).
type CurateAction struct {
	Agent  string
	Skill  string
	Action string // merged, archived
	Into   string // merged: the skill it went into
	Reason string
}

func (a CurateAction) String() string {
	if a.Action == "merged" {
		return fmt.Sprintf("%s: merged %s into %s (%s)", a.Agent, a.Skill, a.Into, a.Reason)
	}
	return fmt.Sprintf("%s: archived %s (%s)", a.Agent, a.Skill, a.Reason)
}

type curated struct {
	Skill
	body     string
	words    map[string]bool
	lastUsed time.Time
}

// Curate runs one pass of the curator.
func (l *Library) Curate(o CurateOptions) ([]CurateAction, error) {
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	used := l.LastUsed()
	ctx := history.WithActor(context.Background(), history.Actor{By: history.ByCurator, Tool: "skills_curator", Background: true})
	var out []CurateAction
	for _, agent := range o.Agents {
		dir, err := l.agentDir(agent)
		if err != nil {
			return out, err
		}
		own := map[string]Skill{}
		if err := scan(dir, false, own); err != nil {
			return out, err
		}
		var list []*curated
		for _, name := range slices.Sorted(maps.Keys(own)) {
			if l.Pinned(ctx, agent, name) {
				continue // never rewritten, merged away or archived
			}
			s, body, err := load(own[name].Dir)
			if err != nil {
				continue
			}
			c := &curated{Skill: s, body: body, words: words(s.Name + " " + s.Description)}
			c.lastUsed = used[l.usageKey(s.Dir)]
			if info, err := os.Stat(filepath.Join(s.Dir, FileName)); err == nil && info.ModTime().After(c.lastUsed) {
				c.lastUsed = info.ModTime()
			}
			list = append(list, c)
		}
		gone := map[string]bool{}
		for _, p := range duplicates(list) {
			a, b := p.a, p.b
			if gone[a.Name] || gone[b.Name] {
				continue
			}
			keep, drop := a, b
			if b.lastUsed.After(a.lastUsed) || (b.lastUsed.Equal(a.lastUsed) && len(b.body) > len(a.body)) {
				keep, drop = b, a
			}
			act := CurateAction{Agent: agent, Skill: drop.Name, Action: "merged", Into: keep.Name, Reason: p.reason}
			if !o.DryRun {
				if err := l.merge(ctx, agent, keep, drop); err != nil {
					return out, fmt.Errorf("merge %s into %s: %w", drop.Name, keep.Name, err)
				}
			}
			gone[drop.Name] = true
			out = append(out, act)
		}
		if o.ArchiveAfter <= 0 {
			continue
		}
		for _, c := range list {
			if gone[c.Name] || o.Now.Sub(c.lastUsed) < o.ArchiveAfter {
				continue
			}
			act := CurateAction{Agent: agent, Skill: c.Name, Action: "archived",
				Reason: "unused since " + c.lastUsed.Format("2006-01-02")}
			if !o.DryRun {
				ctx := history.WithActor(ctx, curatorActor(ctx, "archived: "+act.Reason))
				if err := l.removeSkill(ctx, agent, c.Name, func() error {
					_, err := l.Backups().Save(c.Name, c.Dir, "archived by the curator: "+act.Reason, true)
					return err
				}); err != nil {
					return out, err
				}
			}
			out = append(out, act)
		}
	}
	return out, nil
}

// dup is two skills that look like the same one.
type dup struct {
	a, b   *curated
	score  float64
	reason string
}

// duplicates finds pairs that look like the same skill, most alike first.
func duplicates(list []*curated) []dup {
	var out []dup
	for i := range list {
		for j := i + 1; j < len(list); j++ {
			a, b := list[i], list[j]
			switch {
			case strings.TrimSpace(a.body) == strings.TrimSpace(b.body):
				out = append(out, dup{a, b, 2, "same instructions"})
			case singular(a.Name) == singular(b.Name):
				out = append(out, dup{a, b, 1.5, "same name"})
			default:
				if s := jaccard(a.words, b.words); s >= 0.7 && len(a.words) >= 3 && len(b.words) >= 3 {
					out = append(out, dup{a, b, s, fmt.Sprintf("descriptions %.0f%% alike", s*100)})
				}
			}
		}
	}
	slices.SortStableFunc(out, func(x, y dup) int {
		switch {
		case x.score > y.score:
			return -1
		case x.score < y.score:
			return 1
		}
		return 0
	})
	return out
}

// merge appends drop's instructions to keep's, copies drop's supporting
// files into keep/merged/<drop>/, and moves drop to the backups. keep is
// backed up first.
func (l *Library) merge(ctx context.Context, agent string, keep, drop *curated) error {
	if _, err := l.Backups().Save(keep.Name, keep.Dir, "before merging "+drop.Name+" into it", false); err != nil {
		return err
	}
	body := keep.body
	if strings.TrimSpace(drop.body) != strings.TrimSpace(keep.body) {
		body += fmt.Sprintf("\n\n## Merged from %s\n\n%s\n\n%s", drop.Name, drop.Description, drop.body)
		if len(body) > MaxBody {
			body = body[:MaxBody]
		}
	}
	err := filepath.WalkDir(drop.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(drop.Dir, p)
		if err != nil || rel == FileName || rel == BackupName {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		dst := filepath.Join(keep.Dir, "merged", drop.Name, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		return store.WriteFileAtomic(dst, data, 0o600)
	})
	if err != nil {
		return err
	}
	data, err := frontmatter.Format(meta{Name: keep.Name, Description: keep.Description}, body)
	if err != nil {
		return err
	}
	if err := l.writeSkill(history.WithActor(ctx, curatorActor(ctx, "merged "+drop.Name+" into it")), agent, keep.Name, data); err != nil {
		return err
	}
	return l.removeSkill(history.WithActor(ctx, curatorActor(ctx, "merged into "+keep.Name)), agent, drop.Name, func() error {
		_, err := l.Backups().Save(drop.Name, drop.Dir, "merged into "+keep.Name+" by the curator", true)
		return err
	})
}

// curatorActor is ctx's actor with a note on what the curator did.
func curatorActor(ctx context.Context, note string) history.Actor {
	a := history.ActorFrom(ctx)
	a.Note = note
	return a
}

var stopWords = map[string]bool{"the": true, "and": true, "for": true, "with": true, "use": true, "when": true,
	"this": true, "that": true, "from": true, "into": true, "how": true, "your": true, "you": true, "are": true}

func words(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	}) {
		if len(w) > 2 && !stopWords[w] {
			out[singular(w)] = true
		}
	}
	return out
}

func singular(w string) string {
	if len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") {
		return w[:len(w)-1]
	}
	return w
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	both := 0
	for w := range a {
		if b[w] {
			both++
		}
	}
	return float64(both) / float64(len(a)+len(b)-both)
}

// curatorState remembers when the curator last ran.
type curatorState struct {
	LastRun time.Time `json:"last_run"`
}

const curatorFile = ".curator.json"

// CurateDue reports whether a scheduled curator run is due.
func (l *Library) CurateDue(every time.Duration, now time.Time) bool {
	if every <= 0 {
		return false
	}
	var st curatorState
	if data, err := os.ReadFile(filepath.Join(l.SharedDir(), curatorFile)); err == nil {
		_ = json.Unmarshal(data, &st)
	}
	return now.Sub(st.LastRun) >= every
}

// MarkCurated records a curator run.
func (l *Library) MarkCurated(now time.Time) error {
	data, _ := json.Marshal(curatorState{LastRun: now.UTC()})
	if err := os.MkdirAll(l.SharedDir(), 0o700); err != nil {
		return err
	}
	return store.WriteFileAtomic(filepath.Join(l.SharedDir(), curatorFile), data, 0o600)
}
