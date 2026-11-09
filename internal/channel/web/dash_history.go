package web

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory/history"
)

// Memory history: every change to an agent's notes, USER.md, IDENTITY.md
// and own skills, with diffs and restore.

type changeView struct {
	history.Change
	Op   string `json:"op"`
	Diff string `json:"diff,omitempty"`
}

func viewChange(c history.Change) changeView { return changeView{Change: c, Op: c.Op()} }

// dashActor marks the memory changes a dashboard request makes as the
// user's.
func dashActor(r *http.Request) context.Context {
	return history.WithActor(r.Context(), history.Actor{By: history.ByUser, Tool: "dashboard"})
}

// memoryHistory returns the store's history for an agent's endpoints, or
// writes the error.
func (s *Server) memoryHistory(w http.ResponseWriter, agentName string) *history.Log {
	st := s.memoryStore(w, agentName)
	if st == nil {
		return nil
	}
	h := st.History()
	if h == nil {
		notAvailable(w, "memory history")
	}
	return h
}

// historyList lists an agent's changes, newest first, with ?file=, ?kind=,
// ?since= (7d, 12h or a date) and ?limit=.
func (s *Server) historyList(w http.ResponseWriter, r *http.Request) {
	agentName := r.PathValue("agent")
	h := s.memoryHistory(w, agentName)
	if h == nil {
		return
	}
	q := r.URL.Query()
	since, err := history.ParseSince(q.Get("since"), time.Now())
	if err != nil {
		userError(w, err)
		return
	}
	cs, err := h.List(r.Context(), history.Filter{Agent: agentName, File: q.Get("file"), Kind: q.Get("kind"),
		Since: since, Limit: intParam(r, "limit", 50, 1, 500)})
	if err != nil {
		s.fail(w, "list memory changes", err)
		return
	}
	out := make([]changeView, 0, len(cs))
	for _, c := range cs {
		out = append(out, viewChange(c))
	}
	pins, err := h.Pins(r.Context(), agentName)
	if err != nil {
		s.fail(w, "list pins", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"changes": out, "pins": nonNil(pins)})
}

// historyChange finds one of the agent's changes, writing 404 when there
// is none.
func (s *Server) historyChange(w http.ResponseWriter, r *http.Request, h *history.Log, agentName string) (history.Change, bool) {
	c, err := h.Get(r.Context(), r.PathValue("id"))
	if err != nil || c.Agent != agentName {
		httpError(w, http.StatusNotFound, "no change "+r.PathValue("id")+" for agent "+agentName)
		return c, false
	}
	return c, true
}

// historyGet returns one change with its unified diff.
func (s *Server) historyGet(w http.ResponseWriter, r *http.Request) {
	agentName := r.PathValue("agent")
	h := s.memoryHistory(w, agentName)
	if h == nil {
		return
	}
	c, ok := s.historyChange(w, r, h, agentName)
	if !ok {
		return
	}
	v := viewChange(c)
	d, err := h.Diff(r.Context(), c)
	if err != nil {
		s.fail(w, "diff a memory change", err)
		return
	}
	v.Diff = d
	writeJSON(w, http.StatusOK, v)
}

// historyRestore puts back the "before" (default) or "after" side of a
// change, recorded as a change by the user through the dashboard.
func (s *Server) historyRestore(w http.ResponseWriter, r *http.Request) {
	agentName := r.PathValue("agent")
	h := s.memoryHistory(w, agentName)
	if h == nil {
		return
	}
	var in struct {
		Version string `json:"version"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	version := strings.ToLower(strings.TrimSpace(in.Version))
	if version != "" && version != history.Before && version != history.After {
		httpError(w, http.StatusBadRequest, `version must be "before" or "after"`)
		return
	}
	c, ok := s.historyChange(w, r, h, agentName)
	if !ok {
		return
	}
	_, restored, err := s.Dash.Memory.Restore(dashActor(r), agentName, c.ID, version)
	if err != nil {
		userError(w, err)
		return
	}
	if restored == nil {
		writeJSON(w, http.StatusOK, map[string]any{"restored": nil, "unchanged": true})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"restored": viewChange(*restored), "unchanged": false})
}
