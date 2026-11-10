package web

import (
	"errors"
	"net/http"

	"github.com/habakkuk-ai/habakkuk/internal/knowledge/retro"
)

// Retros: an agent's retrospectives, read only. Proposals are approved
// with habakkuk retro apply or /retro apply in a chat.

// retroRunner returns the retro runner for an agent's endpoints, or
// writes the error.
func (s *Server) retroRunner(w http.ResponseWriter, agentName string) *retro.Runner {
	d := s.dash(w)
	if d == nil {
		return nil
	}
	if d.Retros == nil {
		notAvailable(w, "retros")
		return nil
	}
	if !s.knownAgent(w, agentName) {
		return nil
	}
	return d.Retros
}

// retroList lists an agent's retros, newest first, with ?limit=.
func (s *Server) retroList(w http.ResponseWriter, r *http.Request) {
	agentName := r.PathValue("agent")
	rr := s.retroRunner(w, agentName)
	if rr == nil {
		return
	}
	list, err := rr.List(r.Context(), agentName, intParam(r, "limit", 20, 1, 200))
	if err != nil {
		s.fail(w, "list retros", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"retros": nonNil(list)})
}

// retroGet is one retro with its evidence and actions; the id "latest"
// is the newest.
func (s *Server) retroGet(w http.ResponseWriter, r *http.Request) {
	agentName := r.PathValue("agent")
	rr := s.retroRunner(w, agentName)
	if rr == nil {
		return
	}
	ret, err := rr.Resolve(r.Context(), agentName, r.PathValue("id"))
	switch {
	case errors.Is(err, retro.ErrNotFound):
		httpError(w, http.StatusNotFound, err.Error())
		return
	case err != nil:
		s.fail(w, "read a retro", err)
		return
	case ret.Agent != agentName:
		httpError(w, http.StatusNotFound, "no retro "+r.PathValue("id")+" for agent "+agentName)
		return
	}
	writeJSON(w, http.StatusOK, ret)
}
