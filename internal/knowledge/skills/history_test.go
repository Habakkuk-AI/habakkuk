package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory/history"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

func historyLibrary(t *testing.T) (*Library, *history.Log) {
	t.Helper()
	l := &Library{Home: t.TempDir()}
	h, err := history.Open(context.Background(), l.Home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	l.History = h
	if err := h.Scan(context.Background(), "habakkuk"); err != nil {
		t.Fatal(err)
	}
	return l, h
}

func TestSkillSaveIsRecorded(t *testing.T) {
	l, h := historyLibrary(t)
	r := tools.NewRegistry()
	RegisterTools(r, l)
	// A clean turn: a tainted one holds the skill instead (held_test.go).
	env := &tools.Env{Agent: "habakkuk", Session: "s9", Workspace: t.TempDir(), Mode: tools.ModeWorkspace, Taint: tools.NewTaint(false)}
	for _, body := range []string{"Step one.", "Step one, then two."} {
		in, _ := json.Marshal(map[string]string{"name": "brew", "description": "Brew coffee.", "body": body})
		res := r.Execute(context.Background(), env, llm.Block{Type: llm.ToolUse, ToolUseID: "t", ToolName: "skill_save", Input: in})
		if res.IsError {
			t.Fatal(res.Text)
		}
	}
	cs, err := h.List(context.Background(), history.Filter{Agent: "habakkuk"})
	if err != nil || len(cs) != 2 {
		t.Fatal(cs, err)
	}
	c := cs[0]
	if c.Kind != history.KindSkill || c.Name != "brew" || c.Op() != "updated" || c.Tool != "skill_save" || c.Session != "s9" || c.Tainted {
		t.Fatalf("%+v", c)
	}
	d, _ := h.Diff(context.Background(), c)
	if !strings.Contains(d, "-Step one.") || !strings.Contains(d, "+Step one, then two.") {
		t.Fatalf("diff:\n%s", d)
	}
}

func TestCuratorChangesAreRecordedAndPinnedSkillsKept(t *testing.T) {
	l, h := historyLibrary(t)
	ctx := context.Background()
	dir := l.AgentDir("habakkuk")
	now := time.Now()
	old := now.Add(-200 * 24 * time.Hour)
	write(t, filepath.Join(dir, "weekly-report", FileName), skillMD("weekly-report", "Write the weekly status report from the board.", "Steps A."))
	write(t, filepath.Join(dir, "weekly-reports", FileName), skillMD("weekly-reports", "Weekly report.", "Steps B."))
	write(t, filepath.Join(dir, "tuned", FileName), skillMD("tuned", "Tuned by hand, rarely used.", "Steps T."))
	write(t, filepath.Join(dir, "tuned-copy", FileName), skillMD("tuned-copy", "Copy.", "Steps T."))
	write(t, filepath.Join(dir, "old-thing", FileName), skillMD("old-thing", "Something nobody uses.", "Steps E."))
	for _, n := range []string{"weekly-report", "weekly-reports", "tuned", "tuned-copy", "old-thing"} {
		os.Chtimes(filepath.Join(dir, n, FileName), old, old)
	}
	l.MarkUsed(Skill{Dir: filepath.Join(dir, "weekly-reports")})
	l.MarkUsed(Skill{Dir: filepath.Join(dir, "tuned-copy")})
	// Created by hand after the baseline: recorded as such by a scan.
	if err := h.Scan(ctx, "habakkuk"); err != nil {
		t.Fatal(err)
	}
	if err := h.Pin(ctx, "habakkuk", history.KindSkill, "tuned"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Curate(CurateOptions{Agents: []string{"habakkuk"}, ArchiveAfter: 90 * 24 * time.Hour, Now: now}); err != nil {
		t.Fatal(err)
	}
	// The pinned skill was neither merged away (its copy has the same
	// instructions) nor archived (unused for 200 days).
	if b, err := os.ReadFile(filepath.Join(dir, "tuned", FileName)); err != nil || strings.Contains(string(b), "Merged from") {
		t.Fatalf("pinned skill changed: %s %v", b, err)
	}
	cs, err := h.List(ctx, history.Filter{Agent: "habakkuk", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var curated []string
	for _, c := range cs {
		switch c.By {
		case history.ByCurator:
			curated = append(curated, c.Name+":"+c.Op())
			if c.Tool != "skills_curator" || c.Note == "" {
				t.Fatalf("%+v", c)
			}
		case history.ByHand:
			if c.Op() != "created" {
				t.Fatalf("%+v", c)
			}
		default:
			t.Fatalf("unexpected %+v", c)
		}
	}
	if strings.Join(curated, " ") != "old-thing:deleted weekly-report:deleted weekly-reports:updated" {
		t.Fatalf("curator changes %v", curated)
	}
	// The merge can be undone from the history.
	for _, c := range cs {
		if c.Name == "weekly-reports" && c.By == history.ByCurator {
			if _, _, err := h.Restore(ctx, c.ID, history.Before); err != nil {
				t.Fatal(err)
			}
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "weekly-reports", FileName)); strings.Contains(string(b), "Merged from") {
		t.Fatal("merge not undone")
	}
}
