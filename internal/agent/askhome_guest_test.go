package agent

import (
	"context"
	"encoding/json"
	"testing"
)

// A guest's chat message asks in the agent's own home; the owner's, or a
// schedule's, does not.
func TestAskHomeForGuest(t *testing.T) {
	r := &Runner{}
	spec := &Spec{Name: "main", Home: "tg:1"}
	for _, c := range []struct {
		name string
		p    TurnPayload
		want string
	}{
		{"guest", TurnPayload{Origin: json.RawMessage(`{"account":"s","conversation":"c","trust":"guest"}`)}, "tg:1"},
		{"owner", TurnPayload{Origin: json.RawMessage(`{"account":"s","conversation":"c","trust":"owner"}`)}, ""},
		{"schedule on an open account", TurnPayload{Source: SourceSchedule, Origin: json.RawMessage(`{"account":"s","conversation":"c"}`)}, ""},
		{"no origin", TurnPayload{}, ""},
	} {
		if got, _ := r.askHome(context.Background(), c.p, spec); got != c.want {
			t.Errorf("%s: home %q, want %q", c.name, got, c.want)
		}
	}
}
