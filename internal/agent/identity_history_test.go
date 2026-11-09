package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory/history"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

func TestSetIdentityIsRecorded(t *testing.T) {
	h := newHarness(t, &fakeModel{})
	hist, err := history.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer hist.Close()
	reg := tools.NewRegistry()
	h.runner.RegisterIdentityWithHistory(reg, hist)
	if !hasSpec(reg.Specs([]string{"core"}), identityToolName) {
		t.Fatal("set_identity is not registered")
	}
	tool := recordedIdentityTool{identityTool{h.runner}, hist}
	env := &tools.Env{Agent: "habakkuk", Session: "s1"}
	for _, in := range []string{`{"name":"Ada"}`, `{"name":"Ada","emoji":"🌿","style":"brief"}`} {
		if _, err := tool.Run(context.Background(), env, json.RawMessage(in)); err != nil {
			t.Fatal(err)
		}
	}
	cs, err := hist.List(context.Background(), history.Filter{Agent: "habakkuk"})
	if err != nil || len(cs) != 2 {
		t.Fatal(cs, err)
	}
	c := cs[0]
	if c.Kind != history.KindIdentity || c.Op() != "updated" || c.Tool != identityToolName || c.Session != "s1" || c.By != history.ByAgent {
		t.Fatalf("%+v", c)
	}
	d, _ := hist.Diff(context.Background(), c)
	if !strings.Contains(d, "+style: brief") {
		t.Fatalf("diff:\n%s", d)
	}
	// Undo puts the first identity back.
	if _, _, err := hist.Restore(context.Background(), c.ID, history.Before); err != nil {
		t.Fatal(err)
	}
	if id, _ := ReadIdentity(h.runner.Agents["habakkuk"].Dir); id.Name != "Ada" || id.Style != "" {
		t.Fatalf("restored %+v", id)
	}
}
