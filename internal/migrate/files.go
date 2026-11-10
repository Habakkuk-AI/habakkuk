package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/core/platform"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
)

// The text an import brings over often names files in the source home: a
// persona file that says where the briefs are, a schedule that runs a
// script from there, a skill that reads a state file. Left as it is, the
// new agent keeps reading and writing inside the old agent's folder, and
// that folder can never be removed.
//
// So the plan finds every such reference, groups them by the entry of the
// source home they name (workspaces, scripts, state), and sorts the entries
// in two: parts of the old agent itself, which stay, and the user's own
// data, which can be copied into the Habakkuk home. With Files set to
// "copy", Apply copies those entries and the imported text is rewritten to
// name the copies. The source home is only ever read.

// What Options.Files may be.
const (
	FilesCopy = "copy"
	FilesSkip = "skip"
)

const (
	// DefaultFilesMax is the largest file a copy takes unless FilesMax
	// says otherwise.
	DefaultFilesMax = 1 << 30
	// rewriteCap is the largest text file whose references are looked at
	// and rewritten.
	rewriteCap = 1 << 20
	// spaceMargin is the room a copy leaves free at the destination.
	spaceMargin = 32 << 20
	// filesManifest records, in the Habakkuk home, which entries of which
	// source home were copied and where to, so that a later run in any
	// mode knows the imported text already names the copies.
	filesManifest = "migrated-files.json"
)

// FileEntry is one entry of the source home that the imported text names.
type FileEntry struct {
	Name   string // "scripts", or "profiles/coder/scripts" in a profile's home
	Source string
	Dest   string // where its copy goes
	// Kind is "folder", "file", "link" or "missing" (named, but not in the
	// source home: only the references move).
	Kind   string
	LinkTo string
	// Why says why it stays in the source home; "" for the user's own data,
	// which a copy moves.
	Why string
	// Adopted: an earlier run copied it, and the imported text names the
	// copy.
	Adopted bool
	Refs    int      // how often the imported text and the copied files name it
	Where   []string // what names it
	// Stats says what it holds and what a copy would bring over now.
	Stats SyncStats
	// InsideFiles of its own files hold InsideRefs references that a copy
	// rewrites.
	InsideFiles, InsideRefs int

	key     string
	scanned bool
	inside  map[string]map[string]int // its file -> entry -> references
}

// FilesPlan is what the import does about the files the imported text
// names.
type FilesPlan struct {
	Mode string // FilesCopy or FilesSkip
	// Ask: planned as a copy, before the user was asked.
	Ask bool
	// Chosen: the mode was given, not defaulted.
	Chosen bool
	Dir    string // where copies go
	Max    int64  // the largest file copied; 0 is no limit
	// Moves are the user's own entries; Stays are the ones that do not
	// move, each with its reason.
	Moves, Stays []*FileEntry
	Need         int64  // bytes a copy would write now
	Free         uint64 // bytes free at Dir, when Need > 0
	// Err is why a copy is refused (no room, a destination inside the
	// source); Apply returns it.
	Err   error
	Notes []string
}

// Copies reports whether entries are copied by this plan.
func (fp *FilesPlan) Copies() bool { return fp != nil && fp.Mode == FilesCopy }

// Pending reports whether a copy has anything to do.
func (fp *FilesPlan) Pending() bool {
	if !fp.Copies() {
		return false
	}
	for _, e := range fp.Moves {
		if !e.Adopted || e.Stats.todo() {
			return true
		}
	}
	return false
}

// Waiting lists the entries a copy would move that no run has copied yet.
func (fp *FilesPlan) Waiting() []*FileEntry {
	if fp == nil {
		return nil
	}
	var out []*FileEntry
	for _, e := range fp.Moves {
		if !e.Adopted {
			out = append(out, e)
		}
	}
	return out
}

// Pointer is something in the source home that the imported text still
// names after the import.
type Pointer struct {
	Name  string
	Why   string
	Refs  int
	Where []string
}

// StillPoints lists what keeps the source home needed: the entries that do
// not move, and with Files "skip" the ones that were not copied.
func (p *Plan) StillPoints() []Pointer {
	fp := p.Files
	if fp == nil {
		return nil
	}
	var out []Pointer
	for _, e := range fp.Moves {
		if !fp.Copies() && !e.Adopted {
			out = append(out, Pointer{Name: e.Name, Why: "your own files, not copied", Refs: e.Refs, Where: e.Where})
		}
	}
	for _, e := range fp.Stays {
		out = append(out, Pointer{Name: e.Name, Why: e.Why, Refs: e.Refs, Where: e.Where})
	}
	return out
}

// hit is one imported thing whose text names entries of the source home.
type hit struct {
	kind, agent, name string // the plan item it is
	where             string // how the report names it
	keys              map[string]int
}

func hitID(kind, agent, name string) string { return kind + "\x00" + agent + "\x00" + name }

// internals are the entries of a source home that are part of the agent
// itself. They are never copied.
type internals struct {
	why   string
	names []string
}

const (
	whyProgram  = "%s's own program"
	whyApp      = "%s's own app files"
	whyCache    = "a cache %s rebuilds"
	whyLogs     = "%s's logs"
	whySessions = "%s's sessions and databases"
	whyRunning  = "%s's process and lock files"
	whyKeys     = "%s's keys and sign-ins (the keys the import uses go to Habakkuk's .env)"
	whyConfig   = "%s's config (its settings are imported)"
	whyLogin    = "a chat login that cannot be copied"
	whyPlugins  = "%s's plugins and hooks, which have no direct equivalent"
)

// hermesInternals: what a Hermes Agent home keeps for itself. Any entry
// with "cache" in its name is a cache too (see internalWhy).
var hermesInternals = []internals{
	// tools holds the runtimes it downloads for itself (node, a browser,
	// ffmpeg); local-patches and locales change its own program and wording.
	{whyProgram, []string{"hermes-agent", "hermes-setup", "bin", "node", "node_modules", "venv", ".venv",
		"tools", "local-patches", "locales"}},
	{whyApp, []string{"assets", "desktop", "desktop-plugins", "pets"}},
	{whyCache, []string{"browser_screenshots", "tmp", ".update_check", ".skills_prompt_snapshot.json"}},
	{whyLogs, []string{"logs", "interrupt_debug.log", "gateway-starts.log"}},
	{whySessions, []string{"sessions", "state.db", "kanban.db", "kanban", "response_store.db", "memory_store.db",
		"projects.db", "pending_messages", "checkpoints", "sandboxes", ".hermes_history", "channel_directory.json"}},
	{whyRunning, []string{"gateway.pid", "gateway_state.json", "processes.json"}},
	{whyKeys, []string{".env", "auth.json", "auth.lock", "mcp-tokens", "pairing"}},
	{whyConfig, []string{"config.yaml", "profile.yaml", "honcho.json", "skins"}},
	{whyLogin, []string{"platforms", "whatsapp"}},
	{whyPlugins, []string{"plugins", "hooks"}},
	{"already imported as the agent's SOUL.md", []string{"SOUL.md"}},
	{"already imported as memory notes and the profile", []string{"memories"}},
	{"already imported as skills (the ones that came with %s are left out)", []string{"skills"}},
	{"%s's schedules, already imported", []string{"cron"}},
	{"the homes of its other agents, imported one by one", []string{"profiles"}},
}

// openclawInternals: what an OpenClaw state folder keeps for itself. The
// agents' workspace folders are handled apart (see classify).
var openclawInternals = []internals{
	{whyProgram, []string{"bin", "node_modules"}},
	{whyCache, []string{"tmp", "media", "browser", "canvas", "completions", "update-check.json"}},
	{whyLogs, []string{"logs"}},
	{whySessions, []string{"agents", "state", "sessions", "sandboxes", "memory", "subagents", "delivery-queue",
		"telegram", "devices", "nodes"}},
	{whyKeys, []string{".env", "credentials", "identity"}},
	{whyConfig, []string{"openclaw.json", "exec-approvals.json"}},
	{whyPlugins, []string{"extensions", "plugins", "hooks"}},
	{"already imported as skills", []string{"skills"}},
	{"%s's schedules, already imported", []string{"cron"}},
}

// claudeCodeInternals: what a Claude Code folder (~/.claude) keeps for
// itself.
var claudeCodeInternals = []internals{
	{whyProgram, []string{"local", "bin", "node_modules", "ide"}},
	{whyCache, []string{"statsig", "shell-snapshots", "paste-cache", "image-cache", "file-history", "tmp"}},
	{whyLogs, []string{"logs", "debug", "telemetry"}},
	{whySessions, []string{"projects", "todos", "history.jsonl", "session-env", "sessions", "plans", "tasks", "teams"}},
	{whyKeys, []string{".credentials.json", ".claude.json", "backups"}}, // backups: copies of .claude.json
	{whyConfig, []string{"settings.json", "settings.local.json", "keybindings.json", "statusline.sh"}},
	{whyPlugins, []string{"plugins", "hooks", "agents", "output-styles"}},
	{"already imported as the agent's AGENTS.md", []string{"CLAUDE.md"}},
	{"already imported as skills", []string{"skills", "commands"}},
}

// codexInternals: what a Codex folder (~/.codex) keeps for itself.
var codexInternals = []internals{
	{whyProgram, []string{"bin", "node_modules", "vendor_imports", "computer-use", "node_repl", "process_manager"}},
	{whyRunning, []string{"ipc"}},
	{whyPlugins, []string{"plugins"}},
	{whyCache, []string{"tmp", ".tmp", "shell_snapshots"}},
	{whyLogs, []string{"log", "logs"}},
	{whySessions, []string{"sessions", "archived_sessions", "history.jsonl", "session_index.jsonl", "state.sqlite", "worktrees", "sqlite", "state"}},
	{whyKeys, []string{"auth.json", ".credentials.json", "installation_id", "browser"}},
	{whyConfig, []string{"config.toml", "version.json", "rules"}},
	{"already imported as the agent's AGENTS.md", []string{"AGENTS.md", "AGENTS.override.md"}},
	{"already imported as skills", []string{"skills", "prompts"}},
	{"already imported as memory notes", []string{"memories"}},
}

// internalWhy says why an entry name is part of the agent itself, or "".
func internalWhy(from, name string) string {
	list := hermesInternals
	switch from {
	case OpenClaw:
		list = openclawInternals
	case ClaudeCode:
		list = claudeCodeInternals
	case Codex:
		list = codexInternals
	}
	same := func(a, b string) bool {
		if runtime.GOOS == "windows" {
			return strings.EqualFold(a, b)
		}
		return a == b
	}
	why := ""
	for _, in := range list {
		if slices.ContainsFunc(in.names, func(n string) bool { return same(n, name) }) {
			why = in.why
		}
	}
	low := strings.ToLower(name)
	switch {
	case why != "":
	case strings.Contains(low, "cache"):
		why = whyCache
	case from == OpenClaw && strings.HasPrefix(low, "openclaw.json."):
		why = whyConfig // its own backups of the config
	case strings.HasSuffix(low, ".sock"), strings.HasSuffix(low, ".pid"), strings.HasSuffix(low, ".lock"):
		why = whyRunning
	case strings.HasSuffix(low, "-wal"), strings.HasSuffix(low, "-shm"), strings.HasSuffix(low, "-journal"):
		why = whySessions
	case IsCodingTool(from) && (strings.HasSuffix(low, ".sqlite") || strings.HasSuffix(low, ".sqlite3") || strings.HasSuffix(low, ".db") ||
		strings.Contains(low, "browser")):
		// In a coding tool's folder, a database or a browser profile the
		// import does not know may hold sign-ins and cookies. (In an
		// agent's home, the user's own scripts keep databases too.)
		why = whyKeys
	}
	if strings.Contains(why, "%s") {
		why = fmt.Sprintf(why, Label(from))
	}
	return why
}

// innerInternals are files the agent keeps inside a folder that is
// otherwise the user's: entry name -> patterns of the files directly in
// it. A Hermes Agent home's state folder holds the user's script state
// next to the gateway's own.
var innerInternals = map[string]map[string][]string{
	Hermes: {"state": {"gateway.*", "gateway_*"}},
}

// innerInternal reports whether the file rel (a slash path inside the
// entry) belongs to the agent itself.
func innerInternal(from, entry, rel string) bool {
	for _, pattern := range innerInternals[from][entry] {
		if ok, _ := path.Match(pattern, rel); ok {
			return true
		}
	}
	return false
}

// planFiles finds the references into the source home, decides what each
// named entry needs, and from here on plans with the text rewritten to
// name the copies. It reads both homes and changes nothing.
func (p *Plan) planFiles() error {
	f, o := p.Found, p.opts
	h := newHomeRefs(f.Dir, o.userHome(), f.nested, runtime.GOOS == "windows")
	h.know("", dirNames(f.Dir))
	for _, n := range f.nested {
		h.know(n, dirNames(filepath.Join(f.Dir, filepath.FromSlash(n))))
	}
	hits, err := textHits(f, h)
	if err != nil {
		return err
	}
	if len(hits) == 0 {
		return nil
	}
	fp := &FilesPlan{Mode: o.Files, Ask: o.FilesAsk, Chosen: o.Files != "", Max: o.FilesMax}
	if o.FilesAsk {
		fp.Mode = FilesCopy
	}
	if fp.Mode != FilesCopy {
		fp.Mode = FilesSkip
	}
	switch {
	case fp.Max == 0:
		fp.Max = DefaultFilesMax
	case fp.Max < 0:
		fp.Max = 0
	}
	fp.Dir = filepath.Join(p.Home, "files")
	if o.FilesDir != "" {
		if fp.Dir, err = filepath.Abs(expandHome(o.FilesDir, o.userHome())); err != nil {
			return err
		}
	}
	p.Files = fp
	if under(f.Dir, fp.Dir) || under(fp.Dir, f.Dir) {
		fp.Err = fmt.Errorf("the files cannot be copied to %s: it overlaps the folder they come from (%s); pass another --files-dir", fp.Dir, f.Dir)
	}
	adopted, err := readManifest(p.Home, f.Dir)
	if err != nil {
		return err
	}

	entries := map[string]*FileEntry{}
	name := func(key, where string, n int) {
		e := entries[key]
		if e == nil {
			e = p.classify(key, adopted)
			entries[key] = e
		}
		e.Refs += n
		if !slices.Contains(e.Where, where) {
			e.Where = append(e.Where, where)
		}
	}
	for _, ht := range hits {
		for _, key := range sortedKeys(ht.keys) {
			name(key, ht.where, ht.keys[key])
		}
	}
	// A copied script names files too, and those have to come along or it
	// goes on using the old folder. Look inside what is copied until
	// nothing new turns up.
	for more := true; more; {
		more = false
		for _, key := range sortedKeys(entries) {
			e := entries[key]
			if e.Why != "" || e.scanned || !fp.Copies() && !e.Adopted {
				continue
			}
			e.scanned, more = true, true
			if e.Kind != "folder" && e.Kind != "file" {
				continue
			}
			e.inside = scanInside(e.Source, fp.Max, h, func(rel string) bool { return innerInternal(f.From, e.base(), rel) })
			for _, rel := range sortedKeys(e.inside) {
				where := e.Name
				if rel != "." {
					where += "/" + filepath.ToSlash(rel)
				}
				for _, k := range sortedKeys(e.inside[rel]) {
					name(k, where, e.inside[rel][k])
				}
			}
		}
	}

	to := map[string]string{}
	for _, key := range sortedKeys(entries) {
		e := entries[key]
		if e.Why != "" {
			fp.Stays = append(fp.Stays, e)
			continue
		}
		fp.Moves = append(fp.Moves, e)
		if fp.Copies() || e.Adopted {
			to[key] = e.Dest
		}
	}
	if len(to) > 0 {
		p.rw = &rewriter{h: h, to: to}
	}
	for _, e := range fp.Moves {
		if e.Kind != "missing" {
			e.Stats, _ = p.syncEntry(context.Background(), e, false)
		}
		for _, counts := range e.inside {
			n := 0
			for k, c := range counts {
				if p.rw.moves(k) {
					n += c
				}
			}
			if n > 0 {
				e.InsideFiles++
				e.InsideRefs += n
			}
		}
		if fp.Copies() {
			fp.Need += e.Stats.CopyBytes
		}
	}
	if fp.Copies() && fp.Err == nil && fp.Need > 0 {
		free := o.FreeSpace
		if free == nil {
			free = freeSpace
		}
		if fp.Free, err = free(fp.Dir); err == nil && uint64(fp.Need)+spaceMargin > fp.Free { //nolint:gosec // Need is a sum of file sizes
			fp.Err = fmt.Errorf("not enough room in %s: the copy needs %s and %s is free; free some space, pass another --files-dir, or use --files skip",
				fp.Dir, humanSize(fp.Need), freeSize(fp.Free))
		}
	}
	if fp.Copies() && shellQuote(fp.Dir) != fp.Dir && shellQuote(f.Dir) == f.Dir {
		fp.Notes = append(fp.Notes, "The new folder's path has spaces or shell characters. Check the commands of the imported script schedules: a path may need quotes.")
	}

	// From here the plan works with the text as it will be written.
	p.refs = map[string]int{}
	for _, ht := range hits {
		for k, c := range ht.keys {
			if p.rw.moves(k) {
				p.refs[hitID(ht.kind, ht.agent, ht.name)] += c
			}
		}
	}
	p.Found = f.rewritten(p.rw)
	return nil
}

// classify decides what one named entry of the source home is.
func (p *Plan) classify(key string, adopted map[string]string) *FileEntry {
	f, fp := p.Found, p.Files
	e := &FileEntry{key: key, Name: key}
	if key == "" {
		e.Name, e.Source, e.Why = "(the folder itself)", f.Dir, "the text names the old folder as a whole"
		return e
	}
	if runtime.GOOS == "windows" {
		// A reference may spell the entry in any letter case there; name
		// it, and its copy, the way the folder does.
		e.Name = realName(f.Dir, key)
	}
	e.Source = filepath.Join(f.Dir, filepath.FromSlash(e.Name))
	e.Dest = filepath.Join(fp.Dir, filepath.FromSlash(e.Name))
	if dest, ok := adopted[key]; ok {
		e.Dest, e.Adopted = dest, true
	}
	base := key[strings.LastIndex(key, "/")+1:]
	info, err := os.Lstat(e.Source)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		e.Kind = "missing"
		// The text may name a longer entry in a way that hides part of the
		// name ("My\ Notes"). Moving such a reference would break it.
		for _, n := range dirNames(filepath.Dir(e.Source)) {
			if len(n) > len(base) && strings.EqualFold(n[:len(base)], base) && !nameByte(n[len(base)]) {
				e.Why = fmt.Sprintf("not in the old folder (did the text mean %q?); edit that text by hand", n)
			}
		}
	case err != nil:
		e.Why = "it cannot be read: " + err.Error()
	case info.Mode()&fs.ModeSymlink != 0:
		e.Kind = "link"
		e.LinkTo, _ = os.Readlink(e.Source)
	case info.IsDir():
		e.Kind = "folder"
	case info.Mode().IsRegular():
		e.Kind = "file"
		if fp.Max > 0 && info.Size() > fp.Max && !e.Adopted {
			e.Why = fmt.Sprintf("larger than the --files-max limit (%s; it is %s)", humanSize(fp.Max), humanSize(info.Size()))
		}
	default:
		e.Why = "a socket or device, never copied"
	}
	if e.Adopted {
		e.Why = ""
		return e
	}
	if why := internalWhy(f.From, base); why != "" {
		e.Why = why
	}
	for ws, ag := range f.workspaces {
		if under(e.Source, ws) {
			e.Why = fmt.Sprintf("agent %s's workspace, which Habakkuk uses where it is", ag)
		}
	}
	if e.Why != "" {
		e.Dest = ""
	}
	return e
}

// dirNames lists a folder's entries; none when it cannot be read.
func dirNames(dir string) []string {
	entries, _ := os.ReadDir(dir)
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// realName spells a slash path under dir the way the folders on disk do,
// matching each element without regard to letter case; an element that
// is not there is kept as written.
func realName(dir, key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		for _, n := range dirNames(dir) {
			if strings.EqualFold(n, p) {
				parts[i] = n
				break
			}
		}
		dir = filepath.Join(dir, parts[i])
	}
	return strings.Join(parts, "/")
}

// under reports whether p is dir or inside it.
func under(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// textHits finds the references into the source home in everything the
// import brings over.
func textHits(f *Found, h *homeRefs) ([]hit, error) {
	var hits []hit
	add := func(kind, agent, name, where string, keys map[string]int) {
		if len(keys) > 0 {
			hits = append(hits, hit{kind: kind, agent: agent, name: name, where: where, keys: keys})
		}
	}
	count := func(texts ...string) map[string]int {
		var out map[string]int
		for _, t := range texts {
			for k, n := range h.count(t) {
				if out == nil {
					out = map[string]int{}
				}
				out[k] += n
			}
		}
		return out
	}
	for _, a := range f.Agents {
		add("agent", a.Name, a.Name, "agent "+a.Name+"'s workspace setting", count(a.Workspace))
		for _, name := range sortedKeys(a.Files) {
			add("file", a.Name, name, a.Name+": "+name, count(a.Files[name]))
		}
		add("profile", a.Name, "USER.md", a.Name+": USER.md", count(a.Profile))
		for _, n := range a.Notes {
			add("note", a.Name, n.Title, fmt.Sprintf("%s: note %q", a.Name, n.Title), count(n.Body))
		}
		for _, s := range a.Skills {
			keys := map[string]int{}
			err := skillTexts(&s, func(_ string, data []byte) {
				for k, n := range h.count(string(data)) {
					keys[k] += n
				}
			})
			if err != nil {
				return nil, err
			}
			add("skill", a.Name, s.Name, "skill "+s.Name, keys)
		}
	}
	for _, sc := range f.Schedules {
		add("schedule", sc.Agent, sc.Name, fmt.Sprintf("schedule %q", sc.Name), count(sc.Prompt, sc.Command, sc.Workdir))
	}
	for _, m := range f.MCP {
		add("mcp", "", m.Name, "MCP server "+m.Name, count(append([]string{m.Command}, m.Args...)...))
	}
	return hits, nil
}

// skillFiles calls fn with each text file of a skill folder that a copy
// would take and that is small enough to rewrite.
func skillFiles(dir string, fn func(rel string, data []byte)) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != dir && skipCopy(d.Name()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if info, err := d.Info(); err != nil || info.Size() > rewriteCap {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if isText(data) {
			rel, _ := filepath.Rel(dir, path)
			fn(rel, data)
		}
		return nil
	})
}

// scanInside looks for references in the text files of an entry a copy
// would take: file (relative to root, "." for root itself) -> entry ->
// references. What cannot be read is passed over; the copy reports it.
func scanInside(root string, maxSize int64, h *homeRefs, skip func(rel string) bool) map[string]map[string]int {
	out := map[string]map[string]int{}
	_ = filepath.WalkDir(root, func(file string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(root, file)
		info, err := d.Info()
		if err != nil || info.Size() > rewriteCap || maxSize > 0 && info.Size() > maxSize || skip(filepath.ToSlash(rel)) {
			return nil
		}
		data, err := os.ReadFile(file)
		if err != nil || !isText(data) {
			return nil
		}
		if keys := h.count(string(data)); len(keys) > 0 {
			out[rel] = keys
		}
		return nil
	})
	return out
}

// base is the entry's own name, without the nested home it is in.
func (e *FileEntry) base() string { return e.key[strings.LastIndex(e.key, "/")+1:] }

// rewritten returns a copy of f whose text names the copies.
func (f *Found) rewritten(rw *rewriter) *Found {
	if rw == nil {
		return f
	}
	c := *f
	c.Agents = make([]*Agent, len(f.Agents))
	for i, a := range f.Agents {
		b := *a
		b.Workspace = rw.str(a.Workspace)
		b.Profile = rw.str(a.Profile)
		b.Files = make(map[string]string, len(a.Files))
		for name, text := range a.Files {
			b.Files[name] = rw.str(text)
		}
		b.Notes = slices.Clone(a.Notes)
		for j := range b.Notes {
			b.Notes[j].Body = rw.str(b.Notes[j].Body)
		}
		b.Skills = slices.Clone(a.Skills)
		c.Agents[i] = &b
	}
	c.Schedules = make([]*Schedule, len(f.Schedules))
	for i, sc := range f.Schedules {
		s := *sc
		s.Prompt, s.Command, s.Workdir = rw.str(sc.Prompt), rw.str(sc.Command), rw.str(sc.Workdir)
		c.Schedules[i] = &s
	}
	c.MCP = make([]*MCPServer, len(f.MCP))
	for i, m := range f.MCP {
		s := *m
		s.Command = rw.str(m.Command)
		s.Args = slices.Clone(m.Args)
		for j := range s.Args {
			s.Args[j] = rw.str(s.Args[j])
		}
		c.MCP[i] = &s
	}
	return &c
}

// freeSpace is the room at dir, or at the nearest folder above it that
// exists.
func freeSpace(dir string) (uint64, error) {
	for {
		if _, err := os.Stat(dir); err == nil {
			free, _, err := platform.DiskUsage(dir)
			return free, err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return 0, fmt.Errorf("no folder above %s exists", dir)
		}
		dir = parent
	}
}

// The manifest: source home -> entry -> where its copy is.
type manifest struct {
	Sources map[string]map[string]string `json:"sources"`
}

func readManifest(home, source string) (map[string]string, error) {
	m, err := loadManifest(home)
	if err != nil {
		return nil, err
	}
	return m.Sources[source], nil
}

func loadManifest(home string) (*manifest, error) {
	m := &manifest{Sources: map[string]map[string]string{}}
	path := filepath.Join(home, filesManifest)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, m); err != nil {
		return nil, fmt.Errorf("%s is damaged (%w); fix or remove it, then run the import again", path, err)
	}
	if m.Sources == nil {
		m.Sources = map[string]map[string]string{}
	}
	return m, nil
}

// recordAdopted adds the copied entries to the manifest.
func (p *Plan) recordAdopted() error {
	m, err := loadManifest(p.Home)
	if err != nil {
		return err
	}
	src := p.Found.Dir
	changed := false
	for _, e := range p.Files.Moves {
		if m.Sources[src] == nil {
			m.Sources[src] = map[string]string{}
		}
		if m.Sources[src][e.key] != e.Dest {
			m.Sources[src][e.key], changed = e.Dest, true
		}
	}
	if !changed {
		return nil
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return store.WriteFileAtomic(filepath.Join(p.Home, filesManifest), append(data, '\n'), 0o600)
}

// HumanSize writes a byte count for people.
func HumanSize(n int64) string { return humanSize(n) }

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", (n+1023)>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}

// freeSize is humanSize for the room on a disk.
func freeSize(n uint64) string {
	return humanSize(int64(min(n, 1<<62))) //nolint:gosec // capped
}

// ParseSize reads a size such as "500M", "2G", "1.5GB" or a plain byte
// count. "0" and "none" mean no limit and return -1.
func ParseSize(s string) (int64, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	if t == "0" || t == "NONE" {
		return -1, nil
	}
	t = strings.TrimSuffix(strings.TrimSuffix(t, "B"), "I")
	mult := float64(1)
	switch {
	case strings.HasSuffix(t, "K"):
		mult, t = 1<<10, strings.TrimSuffix(t, "K")
	case strings.HasSuffix(t, "M"):
		mult, t = 1<<20, strings.TrimSuffix(t, "M")
	case strings.HasSuffix(t, "G"):
		mult, t = 1<<30, strings.TrimSuffix(t, "G")
	case strings.HasSuffix(t, "T"):
		mult, t = 1<<40, strings.TrimSuffix(t, "T")
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
	if err != nil || v <= 0 || v*mult > 1<<60 || strings.ContainsAny(t, "EXPN_") {
		return 0, fmt.Errorf("%q is not a size; write it like 500M or 2G (0 for no limit)", s)
	}
	return int64(v * mult), nil
}

// Times is "once" or "3 times".
func Times(n int) string {
	if n == 1 {
		return "once"
	}
	return fmt.Sprintf("%d times", n)
}

// someOf joins the first few of a list, saying how many more there are.
func someOf(list []string, n int) string {
	if len(list) <= n {
		return strings.Join(list, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(list[:n], ", "), len(list)-n)
}

// writeFiles prints the plan's Files section.
func (p *Plan) writeFiles(w io.Writer) {
	fp := p.Files
	if fp == nil {
		return
	}
	src := p.Found.Dir
	fmt.Fprintf(w, "\nFiles in %s that the imported text names\n", src)
	for _, e := range fp.Moves {
		p.writeEntry(w, e)
	}
	files, refs := 0, 0
	for _, e := range fp.Moves {
		files, refs = files+e.InsideFiles, refs+e.InsideRefs
	}
	if fp.Pending() && refs > 0 {
		fmt.Fprintf(w, "  Inside the copied files, %s in %s will name the copies too.\n", plural(refs, "reference"), plural(files, "file"))
	}
	if len(fp.Stays) > 0 {
		fmt.Fprintf(w, "  Not moved (%s is still needed for these):\n", src)
		for _, e := range fp.Stays {
			fmt.Fprintf(w, "  - %s: named %s (%s): %s\n", e.Name, Times(e.Refs), someOf(e.Where, 3), e.Why)
		}
	}
	waiting := fp.Waiting()
	switch {
	case len(fp.Moves) == 0:
	case fp.Err != nil:
	case fp.Ask && fp.Pending():
		fmt.Fprintf(w, "  You will be asked whether to copy them (%s) into %s. --files copy or --files skip answers ahead.\n", humanSize(fp.Need), fp.Dir)
	case fp.Copies() && fp.Need > 0:
		fmt.Fprintf(w, "  They will be copied (%s; %s free) and the imported text will name the copies. %s is only read.\n",
			humanSize(fp.Need), freeSize(fp.Free), src)
	case fp.Copies() && fp.Pending():
		fmt.Fprintf(w, "  The imported text will name the new places. %s is only read.\n", src)
	case fp.Copies():
	case len(waiting) == 0:
	case fp.Chosen:
		fmt.Fprintf(w, "  They will not be copied (--files skip): the imported text keeps naming %s.\n", src)
	default:
		fmt.Fprintf(w, "  They are not copied unless you say so: hk migrate --files copy brings them over.\n")
	}
	for _, n := range fp.Notes {
		fmt.Fprintf(w, "  Note: %s\n", n)
	}
	if fp.Err != nil && fp.Copies() {
		fmt.Fprintf(w, "  Problem: %v\n", fp.Err)
	}
}

func (p *Plan) writeEntry(w io.Writer, e *FileEntry) {
	fp, st := p.Files, e.Stats
	what := ""
	switch e.Kind {
	case "folder":
		what = fmt.Sprintf("%s in %s", humanSize(st.Bytes), plural(st.Files, "file"))
	case "file":
		what = humanSize(st.Bytes)
	case "link":
		what = "a link to " + e.LinkTo
	case "missing":
		what = "not there yet, so only the references move"
	}
	what += ", named " + Times(e.Refs)
	changed := plural(st.Copy+st.MakeDirs+st.MakeLinks, "path")
	switch {
	case !fp.Copies() && !e.Adopted:
		fmt.Fprintf(w, "  - %s: %s (it would go to %s)\n", e.Name, what, e.Dest)
	case !fp.Copies() && st.todo():
		fmt.Fprintf(w, "  ~ %s: %s: copied earlier to %s; %s changed in the old folder since (--files copy brings them over)\n",
			e.Name, what, e.Dest, changed)
	case e.Adopted && !st.todo():
		fmt.Fprintf(w, "  = %s: %s: already in %s\n", e.Name, what, e.Dest)
	case e.Adopted:
		fmt.Fprintf(w, "  ~ %s: %s: %s to copy (%s) -> %s\n", e.Name, what, changed, humanSize(st.CopyBytes), e.Dest)
	default:
		fmt.Fprintf(w, "  + %s: %s -> %s\n", e.Name, what, e.Dest)
	}
	if n := len(st.Unreadable); n > 0 {
		fmt.Fprintf(w, "      cannot read %s, so they are not copied: %s\n", plural(n, "path"), someOf(st.Unreadable, 3))
	}
	if n := len(st.TooBig); n > 0 {
		fmt.Fprintf(w, "      leaves out %s over %s (--files-max raises the limit): %s\n", plural(n, "file"), humanSize(fp.Max), someOf(st.TooBig, 3))
		fmt.Fprintf(w, "      Text that names a file left out will not find it in the copy.\n")
	}
	if st.Newer > 0 {
		fmt.Fprintf(w, "      keeps %s that changed in the copy since\n", plural(st.Newer, "file"))
	}
	if n := len(st.Blocked); n > 0 {
		fmt.Fprintf(w, "      leaves %s alone where something else is in the way: %s\n", plural(n, "path"), someOf(st.Blocked, 3))
	}
	if st.Special > 0 {
		fmt.Fprintf(w, "      leaves out %s\n", plural(st.Special, "socket or device file"))
	}
	if st.Internal > 0 {
		fmt.Fprintf(w, "      leaves out %s that %s keeps there for itself\n", plural(st.Internal, "file"), Label(p.Found.From))
	}
}
