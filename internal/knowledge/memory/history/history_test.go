package history

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func open(t *testing.T, home string, clk *clock) *Log {
	t.Helper()
	l, err := Open(context.Background(), home)
	if err != nil {
		t.Fatal(err)
	}
	l.now = clk.now
	t.Cleanup(l.Close)
	return l
}

func newLog(t *testing.T) (*Log, *clock) {
	clk := &clock{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	return open(t, t.TempDir(), clk), clk
}

func writeFile(path string, data []byte) func() error {
	return func() error {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		return store.WriteFileAtomic(path, data, 0o600)
	}
}

func put(t *testing.T, l *Log, ctx context.Context, tg Target, text string) *Change {
	t.Helper()
	c, err := l.Write(ctx, tg, []byte(text), writeFile(tg.Path, []byte(text)))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func list(t *testing.T, l *Log, f Filter) []Change {
	t.Helper()
	cs, err := l.List(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func content(t *testing.T, l *Log, hash string) string {
	t.Helper()
	b, err := l.Content(context.Background(), hash)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWriteRecordsBeforeAfterAndActor(t *testing.T) {
	l, _ := newLog(t)
	tg := NoteTarget(l.home, "ann", "coffee")
	ctx := WithActor(context.Background(), Actor{By: ByAgent, Tool: "memory_save", Session: "s1", Tainted: true})
	c1 := put(t, l, ctx, tg, "black\n")
	c2 := put(t, l, ctx, tg, "black, no sugar\n")
	if c1.Op() != "created" || c2.Op() != "updated" {
		t.Fatalf("ops %s %s", c1.Op(), c2.Op())
	}
	cs := list(t, l, Filter{Agent: "ann"})
	if len(cs) != 2 || cs[0].ID != c2.ID {
		t.Fatalf("list %+v", cs)
	}
	got := cs[0]
	if got.By != ByAgent || got.Tool != "memory_save" || got.Session != "s1" || !got.Tainted || got.Path != "agents/ann/memory/coffee.md" {
		t.Fatalf("change %+v", got)
	}
	if content(t, l, got.Old) != "black\n" || content(t, l, got.New) != "black, no sugar\n" {
		t.Fatal("content not kept")
	}
	// The same content again records nothing.
	if c := put(t, l, ctx, tg, "black, no sugar\n"); c != nil {
		t.Fatalf("no-op recorded %+v", c)
	}
	// Removing is a change with no "after".
	c3, err := l.Remove(ctx, tg, func() error { return os.Remove(tg.Path) })
	if err != nil || c3.Op() != "deleted" {
		t.Fatalf("remove %+v %v", c3, err)
	}
	// Filters.
	if n := len(list(t, l, Filter{Agent: "ann", File: "coffee"})); n != 3 {
		t.Fatalf("by name: %d", n)
	}
	if n := len(list(t, l, Filter{Agent: "ann", File: "coffee.md"})); n != 3 {
		t.Fatalf("by file name: %d", n)
	}
	if n := len(list(t, l, Filter{Agent: "bob"})); n != 0 {
		t.Fatalf("other agent: %d", n)
	}
	if g, err := l.Get(context.Background(), c2.ID[:5]); err != nil || g.ID != c2.ID {
		t.Fatalf("get by prefix: %v %v", g.ID, err)
	}
}

func TestDefaultActorIsSystem(t *testing.T) {
	l, _ := newLog(t)
	c := put(t, l, context.Background(), ProfileTarget(l.home, "ann"), "## Work\n\nnurse\n")
	if c.By != BySystem || c.Tainted {
		t.Fatalf("%+v", c)
	}
}

func TestRestoreRoundTrip(t *testing.T) {
	l, _ := newLog(t)
	ctx := WithActor(context.Background(), Actor{By: ByAgent, Tool: "memory_save"})
	tg := NoteTarget(l.home, "ann", "coffee")
	put(t, l, ctx, tg, "v1\n")
	c2 := put(t, l, ctx, tg, "v2\n")

	uctx := WithActor(context.Background(), Actor{By: ByUser, Tool: "cli"})
	orig, r, err := l.Restore(uctx, c2.ID, Before)
	if err != nil || orig.ID != c2.ID || r == nil {
		t.Fatalf("restore: %v %v", r, err)
	}
	if data, _ := os.ReadFile(tg.Path); string(data) != "v1\n" {
		t.Fatalf("file %q", data)
	}
	if r.Restores != c2.ID || r.By != ByUser || !strings.Contains(r.Note, "before") {
		t.Fatalf("restore change %+v", r)
	}
	// Redo with the after side.
	if _, r2, err := l.Restore(uctx, c2.ID, After); err != nil || r2 == nil {
		t.Fatalf("redo: %v %v", r2, err)
	}
	if data, _ := os.ReadFile(tg.Path); string(data) != "v2\n" {
		t.Fatalf("file %q", data)
	}
	// Undoing a creation moves the file to the trash.
	cs := list(t, l, Filter{Agent: "ann"})
	created := cs[len(cs)-1]
	if _, _, err := l.Restore(uctx, created.ID, Before); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tg.Path); !os.IsNotExist(err) {
		t.Fatal("file still there")
	}
	trash, _ := os.ReadDir(filepath.Join(filepath.Dir(tg.Path), ".trash"))
	if len(trash) != 1 {
		t.Fatalf("trash %v", trash)
	}
	// And undoing that brings it back.
	last := list(t, l, Filter{Agent: "ann", Limit: 1})[0]
	if last.Op() != "deleted" {
		t.Fatalf("last %+v", last)
	}
	if _, _, err := l.Restore(uctx, last.ID, Before); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(tg.Path); string(data) != "v2\n" {
		t.Fatalf("file %q", data)
	}
	if n := len(list(t, l, Filter{Agent: "ann"})); n != 6 {
		t.Fatalf("%d changes", n)
	}
}

func TestDiff(t *testing.T) {
	l, _ := newLog(t)
	tg := NoteTarget(l.home, "ann", "list")
	put(t, l, context.Background(), tg, "a\nb\nc\nd\ne\nf\ng\nh\n")
	c := put(t, l, context.Background(), tg, "a\nb\nC\nd\ne\nf\ng\nh\ni\n")
	d, err := l.Diff(context.Background(), *c)
	if err != nil {
		t.Fatal(err)
	}
	want := "--- a/agents/ann/memory/list.md\n+++ b/agents/ann/memory/list.md\n" +
		"@@ -1,8 +1,9 @@\n a\n b\n-c\n+C\n d\n e\n f\n g\n h\n+i\n"
	if d != want {
		t.Fatalf("diff:\n%s\nwant:\n%s", d, want)
	}
	// Hunks far apart are separate.
	long := ""
	for i := range 30 {
		long += string(rune('a'+i%26)) + "\n"
	}
	edited := "X\n" + long[2:len(long)-2] + "Y\n"
	u := Unified("a", "b", long, edited)
	if strings.Count(u, "@@ -") != 2 || !strings.Contains(u, "@@ -1,4 +1,4 @@") {
		t.Fatalf("hunks:\n%s", u)
	}
	if got := Unified("a", "b", "x", "y"); !strings.Contains(got, "\\ No newline at end of file") {
		t.Fatalf("no newline marker:\n%s", got)
	}
	if Unified("a", "b", "same", "same") != "" {
		t.Fatal("equal texts have a diff")
	}
}

func TestCrashAfterPendingDropsTheChange(t *testing.T) {
	l, clk := newLog(t)
	tg := NoteTarget(l.home, "ann", "coffee")
	put(t, l, context.Background(), tg, "v1\n")
	l.crash = func(step string) bool { return step == "pending" }
	if _, err := l.Write(context.Background(), tg, []byte("v2\n"), writeFile(tg.Path, []byte("v2\n"))); !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	// The file is untouched and the pending row is invisible.
	if data, _ := os.ReadFile(tg.Path); string(data) != "v1\n" {
		t.Fatalf("file %q", data)
	}
	if n := len(list(t, l, Filter{})); n != 1 {
		t.Fatalf("%d changes listed", n)
	}
	// A restarted process, a while later, drops it.
	clk.add(2 * staleAfter)
	l2 := open(t, l.home, clk)
	var pending int
	l2.db.QueryRow(`SELECT count(*) FROM changes WHERE state = 0`).Scan(&pending)
	if pending != 0 {
		t.Fatalf("%d pending after recovery", pending)
	}
	if err := l2.Scan(context.Background(), "ann"); err != nil {
		t.Fatal(err)
	}
	if n := len(list(t, l2, Filter{})); n != 1 {
		t.Fatalf("%d changes after scan", n)
	}
}

func TestCrashAfterWriteCommitsTheChange(t *testing.T) {
	l, clk := newLog(t)
	tg := NoteTarget(l.home, "ann", "coffee")
	put(t, l, context.Background(), tg, "v1\n")
	l.crash = func(step string) bool { return step == "written" }
	ctx := WithActor(context.Background(), Actor{By: ByReview, Tool: "memory_review"})
	if _, err := l.Write(ctx, tg, []byte("v2\n"), writeFile(tg.Path, []byte("v2\n"))); !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(tg.Path); string(data) != "v2\n" {
		t.Fatalf("file %q", data)
	}
	// Before it is stale, a scan neither commits it nor calls it a hand edit.
	if err := l.Scan(context.Background(), "ann"); err != nil {
		t.Fatal(err)
	}
	if cs := list(t, l, Filter{}); len(cs) != 1 {
		t.Fatalf("listed %+v", cs)
	}
	clk.add(2 * staleAfter)
	l2 := open(t, l.home, clk)
	cs := list(t, l2, Filter{})
	if len(cs) != 2 || cs[0].By != ByReview || content(t, l2, cs[0].New) != "v2\n" {
		t.Fatalf("after recovery %+v", cs)
	}
	if err := l2.Scan(context.Background(), "ann"); err != nil {
		t.Fatal(err)
	}
	if n := len(list(t, l2, Filter{})); n != 2 {
		t.Fatalf("scan recorded a hand edit: %d changes", n)
	}
}

func TestSameSizeEditInOneMtimeTick(t *testing.T) {
	l, _ := newLog(t)
	tg := NoteTarget(l.home, "ann", "coffee")
	put(t, l, context.Background(), tg, "v1\n")
	info, err := os.Stat(tg.Path)
	if err != nil {
		t.Fatal(err)
	}
	// An edit of the same size that the file system stamps with the same
	// mtime, as Linux does for writes within one clock tick.
	if err := os.WriteFile(tg.Path, []byte("v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tg.Path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := l.Scan(context.Background(), "ann"); err != nil {
		t.Fatal(err)
	}
	cs := list(t, l, Filter{})
	if len(cs) != 2 || cs[0].By != ByHand || content(t, l, cs[0].New) != "v2\n" {
		t.Fatalf("edit missed: %+v", cs)
	}
}

func TestFailedWriteRecordsNothing(t *testing.T) {
	l, _ := newLog(t)
	tg := NoteTarget(l.home, "ann", "coffee")
	boom := errors.New("disk full")
	if _, err := l.Write(context.Background(), tg, []byte("x"), func() error { return boom }); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	var n int
	l.db.QueryRow(`SELECT count(*) FROM changes`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d rows", n)
	}
}

func TestHandEdits(t *testing.T) {
	l, clk := newLog(t)
	ctx := context.Background()
	note := NoteTarget(l.home, "ann", "old")
	prof := ProfileTarget(l.home, "ann")
	// Files that predate the history are a baseline, not changes.
	writeFile(note.Path, []byte("from before\n"))()
	writeFile(prof.Path, []byte("## Me\n\nAnn\n"))()
	if err := l.Scan(ctx, "ann"); err != nil {
		t.Fatal(err)
	}
	if n := len(list(t, l, Filter{})); n != 0 {
		t.Fatalf("baseline recorded %d changes", n)
	}
	// Edits, a new file and a removal made by hand are recorded.
	clk.add(time.Second)
	writeFile(note.Path, []byte("edited by hand\n"))()
	skill := SkillTarget(l.home, "ann", "brew")
	writeFile(skill.Path, []byte("---\nname: brew\n---\nsteps\n"))()
	os.Remove(prof.Path)
	if err := l.Scan(ctx, "ann"); err != nil {
		t.Fatal(err)
	}
	cs := list(t, l, Filter{})
	ops := map[string]string{}
	for _, c := range cs {
		if c.By != ByHand {
			t.Fatalf("%+v", c)
		}
		ops[c.Name] = c.Op()
	}
	if ops["old"] != "updated" || ops["brew"] != "created" || ops[ProfileFile] != "deleted" || len(cs) != 3 {
		t.Fatalf("ops %v", ops)
	}
	for _, c := range cs {
		if c.Name == "old" && content(t, l, c.Old) != "from before\n" {
			t.Fatal("hand edit lost the earlier text")
		}
	}
	// A second scan finds nothing new.
	if err := l.Scan(ctx, "ann"); err != nil {
		t.Fatal(err)
	}
	if n := len(list(t, l, Filter{})); n != 3 {
		t.Fatalf("%d changes", n)
	}
	// Observe on one file, and a write after an unseen hand edit records
	// the hand edit first.
	writeFile(note.Path, []byte("again by hand\n"))()
	c := put(t, l, WithActor(ctx, Actor{By: ByAgent}), note, "by the agent\n")
	cs = list(t, l, Filter{File: "old", Limit: 2})
	if cs[0].ID != c.ID || cs[1].By != ByHand || content(t, l, cs[1].New) != "again by hand\n" || content(t, l, c.Old) != "again by hand\n" {
		t.Fatalf("%+v", cs)
	}
	writeFile(note.Path, []byte("observed\n"))()
	if err := l.Observe(ctx, note); err != nil {
		t.Fatal(err)
	}
	if got := list(t, l, Filter{File: "old", Limit: 1})[0]; got.By != ByHand || got.Note != "edited on disk" {
		t.Fatalf("%+v", got)
	}
}

func TestPinnedRefusesBackgroundWrites(t *testing.T) {
	l, _ := newLog(t)
	ctx := context.Background()
	tg := NoteTarget(l.home, "ann", "coffee")
	put(t, l, ctx, tg, "mine\n")
	if err := l.Pin(ctx, "ann", KindNote, "coffee"); err != nil {
		t.Fatal(err)
	}
	bg := WithActor(ctx, Actor{By: ByReview, Background: true})
	if _, err := l.Write(bg, tg, []byte("theirs\n"), writeFile(tg.Path, []byte("theirs\n"))); !errors.Is(err, ErrPinned) {
		t.Fatalf("background write: %v", err)
	}
	if _, err := l.Remove(bg, tg, func() error { return os.Remove(tg.Path) }); !errors.Is(err, ErrPinned) {
		t.Fatalf("background remove: %v", err)
	}
	if data, _ := os.ReadFile(tg.Path); string(data) != "mine\n" {
		t.Fatalf("file %q", data)
	}
	// The user still can.
	put(t, l, WithActor(ctx, Actor{By: ByUser}), tg, "edited\n")
	pins, _ := l.Pins(ctx, "ann")
	if len(pins) != 1 || pins[0].Name != "coffee" {
		t.Fatalf("pins %v", pins)
	}
	if ok, _ := l.Unpin(ctx, "ann", KindNote, "coffee"); !ok {
		t.Fatal("unpin")
	}
	if _, err := l.Write(bg, tg, []byte("theirs\n"), writeFile(tg.Path, []byte("theirs\n"))); err != nil {
		t.Fatal(err)
	}
}

func TestPruneKeepsTheLatestVersion(t *testing.T) {
	l, clk := newLog(t)
	ctx := context.Background()
	live := NoteTarget(l.home, "ann", "live")
	gone := NoteTarget(l.home, "ann", "gone")
	put(t, l, ctx, live, "v1\n")
	put(t, l, ctx, live, "v2\n")
	put(t, l, ctx, live, "v3\n")
	put(t, l, ctx, gone, "bye\n")
	if _, err := l.Remove(ctx, gone, func() error { return os.Remove(gone.Path) }); err != nil {
		t.Fatal(err)
	}
	clk.add(100 * 24 * time.Hour)
	recent := put(t, l, ctx, NoteTarget(l.home, "ann", "recent"), "new\n")
	n, err := l.Prune(ctx, clk.now().Add(-90*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	cs := list(t, l, Filter{})
	if n != 4 || len(cs) != 2 {
		t.Fatalf("pruned %d, left %+v", n, cs)
	}
	names := map[string]Change{}
	for _, c := range cs {
		names[c.Name] = c
	}
	if names["recent"].ID != recent.ID || content(t, l, names["live"].New) != "v3\n" {
		t.Fatalf("left %+v", cs)
	}
	var blobs int
	l.db.QueryRow(`SELECT count(*) FROM blobs`).Scan(&blobs)
	if blobs != 3 { // v2 (old side of the kept change), v3, new
		t.Fatalf("%d blobs left", blobs)
	}
	// Restoring the kept change still works.
	if _, _, err := l.Restore(ctx, names["live"].ID, Before); err != nil {
		t.Fatal(err)
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"7d": now.AddDate(0, 0, -7), "12h": now.Add(-12 * time.Hour), "": {},
	} {
		got, err := ParseSince(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	if _, err := ParseSince("soon", now); err == nil {
		t.Error("bad span accepted")
	}
}

func TestConcurrentWritesAreAllRecorded(t *testing.T) {
	l, _ := newLog(t)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tg := NoteTarget(l.home, "ann", "n"+string(rune('a'+i)))
			for j := range 5 {
				text := []byte(strings.Repeat("x", j+1))
				if _, err := l.Write(context.Background(), tg, text, writeFile(tg.Path, text)); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if n := len(list(t, l, Filter{Limit: 1000})); n != 40 {
		t.Fatalf("%d changes", n)
	}
}
