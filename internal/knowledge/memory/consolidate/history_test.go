package consolidate

import (
	"context"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory/history"
)

func (r *rig) useHistory(t *testing.T) *history.Log {
	t.Helper()
	h, err := history.Open(context.Background(), r.home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	r.mem.UseHistory(h)
	if err := h.Scan(context.Background(), "habakkuk"); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestReviewChangesAreRecordedAndPinsHonoured(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	h := r.useHistory(t)
	if _, _, err := r.mem.Save(ctx, "habakkuk", "Coffee", "Tuned by hand: black, no sugar.", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.mem.UpdateProfile(ctx, "habakkuk", "Basics", "- Name: Ann", memory.ProfileAppend); err != nil {
		t.Fatal(err)
	}
	if err := h.Pin(ctx, "habakkuk", history.KindNote, "coffee"); err != nil {
		t.Fatal(err)
	}
	if err := h.Pin(ctx, "habakkuk", history.KindProfile, history.ProfileFile); err != nil {
		t.Fatal(err)
	}
	id := r.conversation(t, user("I switched to oat milk lattes, and I live in Lagos now."), assistant("Noted."))
	r.model.review = `{"ops":[
		{"op":"save","title":"Coffee","content":"Oat milk lattes.","from":"user"},
		{"op":"save","title":"Home city","content":"Lagos.","from":"user"},
		{"op":"profile","section":"Basics","mode":"append","content":"- Lives in Lagos","from":"user"}
	]}`
	r.c.Tick(ctx)
	j := r.claim(t)
	if j == nil {
		t.Fatal("no review")
	}
	if _, err := r.run(t, j); err != nil {
		t.Fatal(err)
	}
	coffee, _ := r.mem.Read(ctx, "habakkuk", "coffee")
	if !strings.Contains(coffee.Body, "Tuned by hand") {
		t.Fatalf("pinned note rewritten: %q", coffee.Body)
	}
	if p, _ := r.mem.Profile("habakkuk"); strings.Contains(p, "Lagos") {
		t.Fatalf("pinned profile changed: %q", p)
	}
	cs, err := h.List(ctx, history.Filter{Agent: "habakkuk", Limit: 1})
	if err != nil || len(cs) != 1 {
		t.Fatal(cs, err)
	}
	c := cs[0]
	if c.Name != "home-city" || c.By != history.ByReview || c.Tool != KindReview || c.Session != id || c.Tainted {
		t.Fatalf("review change %+v", c)
	}
	// Unpinned, the same change goes through.
	if _, err := h.Unpin(ctx, "habakkuk", history.KindNote, "coffee"); err != nil {
		t.Fatal(err)
	}
	if err := r.c.apply(history.WithActor(ctx, history.Actor{By: history.ByReview, Background: true}), "habakkuk",
		Op{Op: "save", Title: "Coffee", Content: "Oat milk lattes.", Untrusted: true}); err != nil {
		t.Fatal(err)
	}
	if coffee, _ := r.mem.Read(ctx, "habakkuk", "coffee"); coffee.Body != "Oat milk lattes." {
		t.Fatalf("unpinned note not updated: %q", coffee.Body)
	}
	if c := mustLatest(t, h); c.Name != "coffee" || !c.Tainted {
		t.Fatalf("untrusted change not marked tainted: %+v", c)
	}
}

func mustLatest(t *testing.T, h *history.Log) history.Change {
	t.Helper()
	cs, err := h.List(context.Background(), history.Filter{Limit: 1})
	if err != nil || len(cs) == 0 {
		t.Fatal(cs, err)
	}
	return cs[0]
}

func TestNightlyLeavesPinnedNotesAloneAndRecordsMerges(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	h := r.useHistory(t)
	r.mem.UseEmbedder(vecEmbedder{}, nil)
	for _, n := range [][2]string{
		{"Coffee preference", "Flat white."},
		{"Coffee preferences", "Oat milk, no sugar."},
		{"Tea preference", "Green tea."},
		{"Tea preferences", "No milk."},
	} {
		if _, _, err := r.mem.Save(ctx, "habakkuk", n[0], n[1], nil); err != nil {
			t.Fatal(err)
		}
	}
	r.mem.EmbedPending(ctx, "habakkuk")
	if err := h.Pin(ctx, "habakkuk", history.KindNote, "tea-preferences"); err != nil {
		t.Fatal(err)
	}
	r.c.Nightly = true
	r.model.merge = `{"merge": true, "title": "Coffee preference", "content": "Flat white with oat milk, no sugar.", "tags": []}`
	r.c.Tick(ctx)
	j := r.claim(t)
	if j == nil || j.Kind != KindNightly {
		t.Fatalf("job %+v", j)
	}
	if _, err := r.run(t, j); err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"tea-preference", "tea-preferences", "coffee-preference"} {
		if _, err := r.mem.Read(ctx, "habakkuk", slug); err != nil {
			t.Errorf("%s gone: %v", slug, err)
		}
	}
	if _, err := r.mem.Read(ctx, "habakkuk", "coffee-preferences"); err == nil {
		t.Error("coffee duplicate not merged")
	}
	cs, err := h.List(ctx, history.Filter{Agent: "habakkuk"})
	if err != nil {
		t.Fatal(err)
	}
	var merge []string
	for _, c := range cs {
		if c.By == history.ByReview {
			merge = append(merge, c.Name+":"+c.Op())
			if c.Tool != KindNightly {
				t.Fatalf("%+v", c)
			}
		}
	}
	if strings.Join(merge, " ") != "coffee-preferences:deleted coffee-preference:updated" {
		t.Fatalf("recorded %v", merge)
	}
}
