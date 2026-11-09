package core

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/connections"
	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/habakkukapi"
	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/supervisor"
)

// API serves the app's own JSON API (mounted at /desktop/api/ by the
// loopback server, which checks the session and content type first).
func (c *Controller) API() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /state", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, c.Snapshot()) })
	mux.HandleFunc("POST /connections", c.saveConnection)
	mux.HandleFunc("DELETE /connections/{id}", c.deleteConnection)
	mux.HandleFunc("POST /connections/{id}/activate", func(w http.ResponseWriter, r *http.Request) {
		if err := c.Activate(r.PathValue("id")); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, c.Snapshot())
	})
	mux.HandleFunc("POST /test", c.test)
	mux.HandleFunc("POST /local/{action}", func(w http.ResponseWriter, r *http.Request) {
		var err error
		switch r.PathValue("action") {
		case "start":
			err = c.StartLocal()
		case "stop":
			err = c.StopLocal()
		default:
			http.NotFound(w, r)
			return
		}
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, c.Snapshot())
	})
	mux.HandleFunc("GET /logs", func(w http.ResponseWriter, r *http.Request) {
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		lines, next := c.Logs(after)
		if lines == nil {
			lines = []supervisor.Line{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"lines": lines, "next": next})
	})
	mux.HandleFunc("PUT /settings", c.settings)
	mux.HandleFunc("POST /open", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Path string `json:"path"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		if !strings.HasPrefix(body.Path, "/app/") && !strings.HasPrefix(body.Path, "/desktop/") {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path must be under /app/ or /desktop/"})
			return
		}
		c.UI.ShowMain(body.Path)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /ask", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Conversation string `json:"conversation"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		c.SetAskConversation(body.Conversation)
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

// connInput is a connection from the page; Token, when present, replaces
// the stored one.
type connInput struct {
	connections.Connection
	Token *string `json:"token"`
}

func (c *Controller) saveConnection(w http.ResponseWriter, r *http.Request) {
	var in connInput
	if !readJSON(w, r, &in) {
		return
	}
	if in.ID != "" {
		if _, err := c.Conns.Get(in.ID); err != nil {
			fail(w, err)
			return
		}
	}
	saved, err := c.Conns.Save(in.Connection, in.Token)
	if err != nil {
		fail(w, err)
		return
	}
	if act, ok := c.Conns.Active(); ok && act.ID == saved.ID {
		c.Reconnect()
	}
	writeJSON(w, http.StatusOK, ConnView{Connection: saved, HasToken: c.Conns.HasToken(saved.ID)})
}

func (c *Controller) deleteConnection(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	act, _ := c.Conns.Active()
	c.stopLocal(id)
	if err := c.Conns.Delete(id); err != nil {
		fail(w, err)
		return
	}
	if act.ID == id {
		c.Reconnect()
	}
	w.WriteHeader(http.StatusNoContent)
}

// stopLocal stops and drops a connection's supervisor, if any.
func (c *Controller) stopLocal(id string) {
	c.mu.Lock()
	sup := c.sups[id]
	delete(c.sups, id)
	c.mu.Unlock()
	if sup != nil {
		ctx, cancel := context.WithTimeout(context.Background(), c.grace()+5*time.Second)
		defer cancel()
		if err := sup.Stop(ctx); err != nil {
			c.log().Warn("stop habakkuk serve", "err", err)
		}
	}
}

func (c *Controller) test(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID    string `json:"id"`
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	u, err := connections.ParseURL(in.URL)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	tok := in.Token
	if tok == "" && in.ID != "" {
		tok, _ = c.Conns.Token(in.ID)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	client := &habakkukapi.Client{BaseURL: u.String(), Token: tok}
	info, err := client.App(ctx)
	if err != nil {
		msg := err.Error()
		if habakkukapi.IsUnauthorized(err) {
			msg = "Habakkuk answered, but the token is missing or wrong"
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": msg})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "agents": info.Agents, "default": info.Default})
}

func (c *Controller) settings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Hotkey       string `json:"hotkey"`
		StartAtLogin bool   `json:"start_at_login"`
		Quiet        bool   `json:"quiet"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	prev := c.Conns.Settings()
	var warnings []string
	hotkey := strings.TrimSpace(in.Hotkey)
	if hotkey == "" {
		hotkey = connections.DefaultHotkey
	}
	if hotkey != prev.Hotkey {
		if err := c.UI.SetHotkey(hotkey); err != nil {
			warnings = append(warnings, "hotkey not changed: "+err.Error())
			hotkey = prev.Hotkey
		}
	}
	login := in.StartAtLogin
	if login != prev.StartAtLogin {
		if err := c.UI.SetStartAtLogin(login); err != nil {
			warnings = append(warnings, "start at login: "+err.Error())
			login = prev.StartAtLogin
		}
	}
	s, err := c.Conns.UpdateSettings(func(s *connections.Settings) {
		s.Hotkey, s.StartAtLogin, s.Quiet = hotkey, login, in.Quiet
	})
	if err != nil {
		fail(w, err)
		return
	}
	c.UI.Changed()
	out := map[string]any{"settings": s}
	if len(warnings) > 0 {
		out["warning"] = strings.Join(warnings, "; ")
	}
	writeJSON(w, http.StatusOK, out)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad JSON: " + err.Error()})
		return false
	}
	return true
}

func fail(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	if errors.Is(err, connections.ErrNotFound) {
		code = http.StatusNotFound
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
