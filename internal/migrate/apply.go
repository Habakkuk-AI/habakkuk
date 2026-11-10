package migrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/app"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory"
	"github.com/habakkuk-ai/habakkuk/internal/work/schedule"
)

// Result is what Apply did.
type Result struct {
	Done     []*Item           // items written
	Failed   map[*Item]error   // items that could not be written
	Paused   map[string]string // schedule name (agent/name) -> its new ID
	Copied   []string          // .env variables written
	ToSet    []string          // variables the user still has to set
	WroteCfg bool
}

// Apply carries out the plan. copySecrets writes the keys and tokens into
// $HABAKKUK_HOME/.env; otherwise their names are returned in ToSet. It
// goes on past an item that fails and reports it in Failed.
func (p *Plan) Apply(ctx context.Context, copySecrets bool) (*Result, error) {
	if p.ConfigErr != nil {
		return nil, p.ConfigErr
	}
	res := &Result{Failed: map[*Item]error{}, Paused: map[string]string{}}
	if err := os.MkdirAll(p.Home, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(p.Home, 0o700); err != nil { // it holds chats, memory and keys
		return nil, err
	}
	if p.Config != nil {
		path := filepath.Join(p.Home, "config.yaml")
		if p.OldConfig != nil {
			if err := store.WriteFileAtomic(path+".bak", p.OldConfig, 0o600); err != nil {
				return nil, err
			}
		}
		if err := store.WriteFileAtomic(path, p.Config, 0o600); err != nil {
			return nil, err
		}
		res.WroteCfg = true
		for _, it := range p.Items {
			if it.Changes() && (it.Kind == "agent" || it.Kind == "provider" || it.Kind == "mcp" || it.Kind == "channel") {
				res.Done = append(res.Done, it)
			}
		}
	}
	var mem *memory.Store
	defer func() {
		if mem != nil {
			mem.Close()
		}
	}()
	openMem := func() (*memory.Store, error) {
		if mem == nil {
			var err error
			if mem, err = memory.Open(ctx, p.Home); err != nil {
				return nil, err
			}
		}
		return mem, nil
	}
	secrets := map[string]string{}
	for _, it := range p.Items {
		if it.Kind == "secret" {
			switch {
			case copySecrets && it.Changes():
				secrets[it.Name] = p.Found.Secrets[it.Name]
			case !copySecrets && it.Status != Same:
				res.ToSet = append(res.ToSet, it.Name)
			}
			continue
		}
		if !it.Changes() || it.Kind == "schedule" {
			continue
		}
		var err error
		switch it.Kind {
		case "file":
			err = p.writeFile(it)
		case "profile":
			var m *memory.Store
			if m, err = openMem(); err == nil {
				err = p.writeProfile(ctx, m, it)
			}
		case "note":
			var m *memory.Store
			if m, err = openMem(); err == nil {
				_, _, err = m.Put(ctx, it.Agent, memory.Note{Title: it.note.Title, Content: it.note.Body, Tags: it.note.Tags, Source: memory.SourceUser})
			}
		case "skill":
			err = p.copySkill(it)
		default:
			continue
		}
		if err != nil {
			res.Failed[it] = err
			continue
		}
		res.Done = append(res.Done, it)
	}
	if len(secrets) > 0 {
		if err := setDotEnv(filepath.Join(p.Home, ".env"), secrets); err != nil {
			return res, err
		}
		for _, it := range p.Items {
			if it.Kind == "secret" && secrets[it.Name] != "" {
				res.Done = append(res.Done, it)
				res.Copied = append(res.Copied, it.Name)
			}
		}
	}
	res.ToSet = append(res.ToSet, p.Found.Missing...)
	if err := p.applySchedules(ctx, res); err != nil {
		return res, err
	}
	return res, nil
}

// writeFile writes a persona file, keeping the replaced one as NAME.bak.
func (p *Plan) writeFile(it *Item) error {
	dir := filepath.Join(p.Home, "agents", it.Agent)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, it.Name)
	if it.Status == Replace {
		if old, err := os.ReadFile(path); err == nil {
			if err := store.WriteFileAtomic(path+".bak", old, 0o600); err != nil {
				return err
			}
		}
	}
	if it.ident != nil {
		return agent.WriteIdentity(dir, *it.ident)
	}
	return store.WriteFileAtomic(path, []byte(it.file), 0o600)
}

// writeProfile adds the imported sections to USER.md; a profile too long
// for USER.md becomes a memory note.
func (p *Plan) writeProfile(ctx context.Context, m *memory.Store, it *Item) error {
	a := p.Found.agent(it.Agent)
	if err := memory.CheckSecrets(a.Profile); err != nil {
		return errors.New("the profile looks like it holds a key or password; not imported")
	}
	if len(strings.TrimSpace(a.Profile)) > memory.MaxProfile || strings.Contains(it.Detail, "memory note") {
		_, _, err := m.Put(ctx, it.Agent, memory.Note{Title: "Imported user profile", Content: a.Profile, Tags: []string{"imported", "profile"}, Source: memory.SourceUser})
		return err
	}
	for _, s := range splitSections(a.Profile) {
		name := s.name
		if name == "" {
			name = "About you"
		}
		if _, err := m.UpdateProfile(ctx, it.Agent, name, s.body, memory.ProfileAppend); err != nil {
			_, _, err := m.Put(ctx, it.Agent, memory.Note{Title: "Imported user profile", Content: a.Profile, Tags: []string{"imported", "profile"}, Source: memory.SourceUser})
			return err
		}
	}
	return nil
}

// copySkill copies a skill folder into a staging folder next to its place,
// then moves it in; a replaced skill is kept as NAME.bak (a folder name
// skills never load).
func (p *Plan) copySkill(it *Item) error {
	dest := p.skillDir(it.Agent, it.skill)
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".migrate-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	staged := filepath.Join(tmp, it.skill.Name)
	if err := copyTree(it.skill.Dir, staged); err != nil {
		return err
	}
	if it.Status == Replace {
		bak := dest + ".bak"
		if err := os.RemoveAll(bak); err != nil {
			return err
		}
		if err := os.Rename(dest, bak); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return os.Rename(staged, dest)
}

// applySchedules creates the schedules, each paused.
func (p *Plan) applySchedules(ctx context.Context, res *Result) error {
	var todo []*Item
	for _, it := range p.Items {
		if it.Kind == "schedule" && it.Changes() {
			todo = append(todo, it)
		}
	}
	if len(todo) == 0 {
		return nil
	}
	db, err := app.OpenControl(ctx, p.Home)
	if err != nil {
		return err
	}
	defer db.Close()
	st := schedule.NewStore(db, p.location())
	for _, it := range todo {
		if err := p.createSchedule(ctx, st, db, it, res); err != nil {
			res.Failed[it] = err
			continue
		}
		res.Done = append(res.Done, it)
	}
	return nil
}

func (p *Plan) createSchedule(ctx context.Context, st *schedule.Store, db *sql.DB, it *Item, res *Result) error {
	sc := it.schedule
	s := schedule.Schedule{Agent: sc.Agent, Name: sc.Name, Spec: sc.Spec, Prompt: sc.Prompt,
		Command: sc.Command, OnOutput: sc.OnOutput, Workdir: sc.Workdir}
	if sc.Command != "" {
		s.Timeout = importedScriptTimeout(sc.Spec)
	}
	if sc.To != nil {
		s.DeliverTo, _ = json.Marshal(sc.To)
	}
	if it.oldID != "" {
		if err := st.Delete(ctx, it.oldID); err != nil {
			return err
		}
	}
	created, err := st.Create(ctx, s)
	if err != nil {
		return err
	}
	// Paused from the start: next_run is in the future, and this runs
	// before any worker could claim it.
	if _, err := db.ExecContext(ctx, `UPDATE schedules SET enabled = 0 WHERE id = ?`, created.ID); err != nil {
		return err
	}
	res.Paused[sc.Agent+"/"+sc.Name] = created.ID
	return nil
}

// location is the time zone schedules use: config.yaml's, else local.
func (p *Plan) location() *time.Location {
	raw := p.Config
	if raw == nil {
		raw = p.OldConfig
	}
	if raw != nil {
		if cfg, err := config.Parse(raw); err == nil && cfg.Timezone != "" {
			if loc, err := time.LoadLocation(cfg.Timezone); err == nil {
				return loc
			}
		}
	}
	return time.Local
}

// setDotEnv sets variables in a .env file, replacing earlier lines for the
// same names and keeping everything else, readable only by its owner.
func setDotEnv(path string, vars map[string]string) error {
	for k, v := range vars {
		if !envName.MatchString(k) || strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("cannot store %s in .env", k)
		}
	}
	var lines []string
	if data, err := os.ReadFile(path); err == nil {
		if t := strings.TrimRight(string(data), "\n"); t != "" {
			lines = strings.Split(t, "\n")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	done := map[string]bool{}
	var out []string
	for _, line := range lines {
		k, _, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(line), "export "), "=")
		k = strings.TrimSpace(k)
		if v, set := vars[k]; ok && set {
			if !done[k] {
				out, done[k] = append(out, k+"="+quoteEnv(v)), true
			}
			continue
		}
		out = append(out, line)
	}
	for _, k := range sortedKeys(vars) {
		if !done[k] {
			out = append(out, k+"="+quoteEnv(vars[k]))
		}
	}
	return store.WriteFileAtomic(path, []byte(strings.Join(out, "\n")+"\n"), 0o600)
}

// quoteEnv quotes a value that would not read back as itself.
func quoteEnv(v string) string {
	if strings.ContainsAny(v, " #'\"") || strings.TrimSpace(v) != v {
		if !strings.Contains(v, "'") {
			return "'" + v + "'"
		}
		return `"` + v + `"`
	}
	return v
}

// importedScriptTimeout is the time limit for an imported script schedule.
// The agents Habakkuk imports from let scripts run past Habakkuk's own
// two-minute default, and a script that used to finish in four minutes
// would otherwise fail on every run. Imported scripts get ten minutes, or
// their interval when an "@every" interval is shorter, never less than the
// default.
func importedScriptTimeout(spec string) time.Duration {
	limit := 10 * time.Minute
	if rest, ok := strings.CutPrefix(strings.TrimSpace(spec), "@every "); ok {
		if d, err := time.ParseDuration(strings.TrimSpace(rest)); err == nil && d < limit {
			limit = d
		}
	}
	return max(limit, 2*time.Minute)
}
