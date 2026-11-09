package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSaveReadSearchForget(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	m, created, err := s.Save(ctx, "habakkuk", "Coffee preference", "Flat white, oat milk, no sugar.", []string{"Food", "preferences"})
	if err != nil || !created || m.Slug != "coffee-preference" {
		t.Fatalf("save: %+v created=%v err=%v", m, created, err)
	}
	data, err := os.ReadFile(filepath.Join(s.Dir("habakkuk"), "coffee-preference.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"---\ntitle: Coffee preference\n", "tags: [food, preferences]", "Flat white"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("file missing %q:\n%s", want, data)
		}
	}

	got, err := s.Read(ctx, "habakkuk", "coffee-preference")
	if err != nil || got.Title != "Coffee preference" || got.Body != "Flat white, oat milk, no sugar." || !slices.Equal(got.Tags, []string{"food", "preferences"}) {
		t.Fatalf("read: %+v %v", got, err)
	}

	hits, err := s.Search(ctx, "habakkuk", "what milk does the user like?", nil, 5)
	if err != nil || len(hits) != 1 || hits[0].Slug != "coffee-preference" || !strings.Contains(hits[0].Snippet, "[milk]") {
		t.Fatalf("search: %+v %v", hits, err)
	}
	// Other agents' memories are separate.
	if hits, _ := s.Search(ctx, "ops", "milk", nil, 5); len(hits) != 0 {
		t.Fatalf("ops sees habakkuk's memory: %+v", hits)
	}

	if _, err := s.Forget(ctx, "habakkuk", "coffee-preference"); err != nil {
		t.Fatal(err)
	}
	if hits, _ := s.Search(ctx, "habakkuk", "milk", nil, 5); len(hits) != 0 {
		t.Fatalf("forgotten memory still found: %+v", hits)
	}
	trash, _ := filepath.Glob(filepath.Join(s.Dir("habakkuk"), ".trash", "coffee-preference.*.md"))
	if len(trash) != 1 {
		t.Fatalf("trash = %v", trash)
	}
	if _, err := s.Read(ctx, "habakkuk", "coffee-preference"); err == nil {
		t.Fatal("read after forget succeeded")
	}
}

func TestUpdateKeepsCreatedAndTags(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s.now = func() time.Time { return t0 }
	if _, _, err := s.Save(ctx, "habakkuk", "Home city", "Lagos", []string{"personal"}); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return t0.Add(48 * time.Hour) }
	m, created, err := s.Save(ctx, "habakkuk", "home  CITY", "Toronto since 2024", nil)
	if err != nil || created {
		t.Fatalf("update: created=%v err=%v", created, err)
	}
	got, _ := s.Read(ctx, "habakkuk", m.Slug)
	if !got.Created.Equal(t0) || !got.Updated.Equal(t0.Add(48*time.Hour)) || !slices.Equal(got.Tags, []string{"personal"}) || got.Body != "Toronto since 2024" {
		t.Fatalf("after update: %+v", got)
	}
	// An explicit empty list clears tags.
	s.Save(ctx, "habakkuk", "Home city", "Toronto", []string{})
	if got, _ := s.Read(ctx, "habakkuk", m.Slug); len(got.Tags) != 0 {
		t.Fatalf("tags not cleared: %v", got.Tags)
	}
}

func TestHandEditedFilesArePickedUp(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if _, _, err := s.Save(ctx, "habakkuk", "Editor", "Uses vim", nil); err != nil {
		t.Fatal(err)
	}
	dir := s.Dir("habakkuk")
	path := filepath.Join(dir, "editor.md")
	data, _ := os.ReadFile(path)
	edited := strings.Replace(string(data), "Uses vim", "Switched to helix in October", 1)
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	// Make sure the mtime moves even on coarse-grained filesystems.
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(path, future, future)
	// A brand-new file written by hand, without frontmatter.
	os.WriteFile(filepath.Join(dir, "standup-time.md"), []byte("Daily standup is at 09:30 Toronto time.\n"), 0o600)
	// Files that are not memories are ignored.
	os.WriteFile(filepath.Join(dir, "Not A Slug.md"), []byte("helix"), 0o600)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("helix"), 0o600)

	hits, err := s.Search(ctx, "habakkuk", "helix", nil, 10)
	if err != nil || len(hits) != 1 || hits[0].Slug != "editor" {
		t.Fatalf("edited content not indexed: %+v %v", hits, err)
	}
	if hits, _ := s.Search(ctx, "habakkuk", "vim", nil, 10); len(hits) != 0 {
		t.Fatalf("old content still indexed: %+v", hits)
	}
	hits, _ = s.Search(ctx, "habakkuk", "standup", nil, 10)
	if len(hits) != 1 || hits[0].Title != "standup-time" {
		t.Fatalf("hand-written file: %+v", hits)
	}
	// A file deleted by hand leaves the index.
	os.Remove(filepath.Join(dir, "standup-time.md"))
	if hits, _ := s.Search(ctx, "habakkuk", "standup", nil, 10); len(hits) != 0 {
		t.Fatalf("deleted file still indexed: %+v", hits)
	}
	// A fresh store over the same home (a restart) sees the same index.
	s2, err := Open(ctx, s.home)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, total, _ := s2.Recent(ctx, "habakkuk", 10); total != 1 {
		t.Fatalf("after restart total = %d", total)
	}
}

func TestRankingPrefersTitleAndMoreMatches(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	save := func(title, body string, tags ...string) {
		t.Helper()
		if _, _, err := s.Save(ctx, "habakkuk", title, body, tags); err != nil {
			t.Fatal(err)
		}
	}
	save("Deploy process", "Run the tests, tag a release, then ship it to the server with the deploy script.")
	save("Lunch spots", "The team mentioned deploy once while eating ramen near the office.")
	save("Kubernetes cluster", "Production runs on a three-node cluster in Toronto.", "infra")
	save("Database backups", "Backups of the production database run nightly at 02:00.", "infra")

	hits, err := s.Search(ctx, "habakkuk", "deploy", nil, 10)
	if err != nil || len(hits) != 2 || hits[0].Slug != "deploy-process" {
		t.Fatalf("title match should rank first: %+v %v", hits, err)
	}
	hits, _ = s.Search(ctx, "habakkuk", "production database nightly", nil, 10)
	if len(hits) < 2 || hits[0].Slug != "database-backups" {
		t.Fatalf("more matching terms should rank first: %+v", hits)
	}
	// Stemming: "backup" finds "Backups".
	if hits, _ := s.Search(ctx, "habakkuk", "backup", nil, 10); len(hits) != 1 {
		t.Fatalf("stemmed search: %+v", hits)
	}
	// Tag filter, with and without a query.
	hits, _ = s.Search(ctx, "habakkuk", "production", []string{"INFRA"}, 10)
	if len(hits) != 2 {
		t.Fatalf("tag filter with query: %+v", hits)
	}
	hits, _ = s.Search(ctx, "habakkuk", "", []string{"infra"}, 10)
	if len(hits) != 2 {
		t.Fatalf("tag filter alone: %+v", hits)
	}
	// FTS syntax in the query is neutralised, not an error.
	if _, err := s.Search(ctx, "habakkuk", `deploy" OR (NEAR* AND`, nil, 10); err != nil {
		t.Fatalf("hostile query: %v", err)
	}
	if _, err := s.Search(ctx, "habakkuk", "  ", nil, 10); err == nil {
		t.Fatal("empty query accepted")
	}
}

func TestRecentOrderAndValidation(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for i, title := range []string{"Alpha", "Bravo", "Charlie"} {
		s.now = func() time.Time { return base.Add(time.Duration(i) * time.Hour) }
		s.Save(ctx, "habakkuk", title, "x", nil)
	}
	list, total, err := s.Recent(ctx, "habakkuk", 2)
	if err != nil || total != 3 || len(list) != 2 || list[0].Slug != "charlie" || list[1].Slug != "bravo" {
		t.Fatalf("recent: %+v total=%d err=%v", list, total, err)
	}

	for _, slug := range []string{"../config", "a/b", "", ".trash", "UPPER"} {
		if _, err := s.Read(ctx, "habakkuk", slug); err == nil {
			t.Errorf("read %q succeeded", slug)
		}
		if _, err := s.Forget(ctx, "habakkuk", slug); err == nil {
			t.Errorf("forget %q succeeded", slug)
		}
	}
	if _, _, err := s.Save(ctx, "../x", "t", "c", nil); err == nil {
		t.Error("bad agent name accepted")
	}
	if _, _, err := s.Save(ctx, "habakkuk", "!!!", "c", nil); err == nil {
		t.Error("title without letters accepted")
	}
	if _, _, err := s.Save(ctx, "habakkuk", "t", strings.Repeat("x", MaxContent+1), nil); err == nil {
		t.Error("oversized content accepted")
	}
	// A symlink in the memory dir is neither read nor indexed.
	outside := filepath.Join(t.TempDir(), "secret.md")
	os.WriteFile(outside, []byte("password hunter2"), 0o600)
	os.Symlink(outside, filepath.Join(s.Dir("habakkuk"), "leak.md"))
	if _, err := s.Read(ctx, "habakkuk", "leak"); err == nil {
		t.Error("read through symlink")
	}
	if hits, _ := s.Search(ctx, "habakkuk", "hunter2", nil, 5); len(hits) != 0 {
		t.Errorf("symlink indexed: %+v", hits)
	}
}

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"Coffee preference":       "coffee-preference",
		"  Ünïcode -- & Co. ":     "n-code-co",
		"API keys (rotation)":     "api-keys-rotation",
		strings.Repeat("ab ", 40): strings.TrimRight(strings.Repeat("ab-", 22)[:64], "-"),
	} {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

type answer bool

func (a answer) Approve(context.Context, tools.ApprovalRequest) (bool, error) { return bool(a), nil }

func TestToolsAndPolicy(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	r := tools.NewRegistry()
	RegisterTools(r, s)
	if specs := r.Specs([]string{Toolset}); len(specs) != 7 {
		t.Fatalf("specs: %+v", specs)
	}
	call := func(env *tools.Env, name string, in any) llm.Block {
		b, _ := json.Marshal(in)
		return r.Execute(ctx, env, llm.Block{Type: llm.ToolUse, ToolUseID: "t", ToolName: name, Input: b})
	}
	// Workspace mode with nobody attending: memory writes still run, since
	// they only touch the agent's own memory directory.
	ws := &tools.Env{Agent: "habakkuk", Workspace: t.TempDir(), Mode: tools.ModeWorkspace}
	res := call(ws, "memory_save", map[string]any{"title": "Timezone", "content": "America/Toronto", "tags": []string{"personal"}})
	if res.IsError || !strings.Contains(res.Text, "created memory") {
		t.Fatalf("save: %q", res.Text)
	}
	res = call(ws, "memory_search", map[string]any{"query": "toronto"})
	if res.IsError || !strings.Contains(res.Text, "slug timezone") || !strings.Contains(res.Text, "[personal]") {
		t.Fatalf("search: %q", res.Text)
	}
	res = call(ws, "memory_read", map[string]any{"slug": "timezone"})
	if res.IsError || !strings.Contains(res.Text, "# Timezone") || !strings.Contains(res.Text, "America/Toronto") {
		t.Fatalf("read: %q", res.Text)
	}
	// Read-only denies writes but allows reads; ask mode asks.
	ro := &tools.Env{Agent: "habakkuk", Workspace: ws.Workspace, Mode: tools.ModeReadOnly}
	if res := call(ro, "memory_forget", map[string]any{"slug": "timezone"}); !strings.Contains(res.Text, "denied") {
		t.Fatalf("read-only forget: %q", res.Text)
	}
	if res := call(ro, "memory_search", map[string]any{"query": "toronto"}); res.IsError {
		t.Fatalf("read-only search: %q", res.Text)
	}
	ask := &tools.Env{Agent: "habakkuk", Workspace: ws.Workspace, Mode: tools.ModeAsk, Approver: answer(false)}
	if res := call(ask, "memory_save", map[string]any{"title": "x", "content": "y"}); !strings.Contains(res.Text, "declined") {
		t.Fatalf("ask mode save: %q", res.Text)
	}
	res = call(ws, "memory_forget", map[string]any{"slug": "timezone"})
	if res.IsError || !strings.Contains(res.Text, "forgot timezone") {
		t.Fatalf("forget: %q", res.Text)
	}
	if res := call(ws, "memory_search", map[string]any{"query": "toronto"}); res.Text != "no memories match" {
		t.Fatalf("after forget: %q", res.Text)
	}
}
