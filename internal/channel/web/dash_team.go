package web

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/config"
)

// The control panel: the dashboard pages that change how agents are set up,
// not only what they are doing. Each change edits config.yaml in place
// (config.SetAgent, config.SetChannelRouting) and most take effect when the
// server restarts, which the dashboard can ask for (Dash.Restart).

// ModelList is what one provider offers, for the dashboard's model picker.
type ModelList struct {
	Provider string   `json:"provider"`
	Type     string   `json:"type,omitempty"`
	Models   []string `json:"models"`
	Error    string   `json:"error,omitempty"`
}

// Extension is a page an enabled plugin adds to the dashboard.
type Extension struct {
	Plugin string `json:"plugin"`
	Title  string `json:"title"`
	URL    string `json:"url"`
}

// BuiltinToolsets are the toolsets an agent can be given besides MCP
// servers ("mcp:<name>"), in the order the dashboard lists them.
var BuiltinToolsets = []string{"core", "memory", "skills", "schedule", "delegate", "board", "browser", "computer", "meeting", "homeassistant", "a2a"}

// defaultToolsets is what an agent gets when its config lists none.
var defaultToolsets = []string{"core", "memory", "skills", "schedule", "delegate", "board"}

type teamAgent struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Model       string   `json:"model"`
	Fallbacks   []string `json:"fallbacks"`
	Toolsets    []string `json:"toolsets"`
	// DefaultToolsets: the config lists none, so the agent has the defaults.
	DefaultToolsets bool          `json:"default_toolsets"`
	Home            string        `json:"home,omitempty"`
	ReportHome      string        `json:"report_home,omitempty"`
	Permission      string        `json:"permission,omitempty"`
	Approvals       string        `json:"approvals,omitempty"`
	Daily           float64       `json:"daily,omitempty"`
	Monthly         float64       `json:"monthly,omitempty"`
	Answers         []answerPlace `json:"answers"`
}

// answerPlace is somewhere an agent answers: a whole channel (as its
// default agent) or one routed conversation.
type answerPlace struct {
	Channel      string `json:"channel"`
	Type         string `json:"type"`
	Conversation string `json:"conversation,omitempty"`
	Label        string `json:"label,omitempty"`
}

type routingView struct {
	Account string         `json:"account"`
	Type    string         `json:"type"`
	Agent   string         `json:"agent"`
	Routes  []config.Route `json:"routes"`
}

// liveConfig is config.yaml as it is on disk now, so the control panel
// shows saved changes before a restart applies them. It falls back to the
// config the server started with.
func (s *Server) liveConfig() *config.Config {
	d := s.Dash
	if d == nil {
		return nil
	}
	if d.Home != "" {
		if cfg, err := config.Load(d.Home); err == nil {
			return cfg
		}
	}
	return d.Config
}

func (s *Server) controlRoutes(api func(string, http.HandlerFunc)) {
	api("GET /api/team", s.team)
	api("PUT /api/agents/{name}", s.agentSave)
	api("GET /api/routing", s.routingList)
	api("PUT /api/routing/{account}", s.routingSave)
	api("GET /api/models", s.models)
	api("POST /api/skills-off/{name}", s.skillOff)
	api("GET /api/restart", s.restartState)
	api("POST /api/restart", s.restart)
}

func (s *Server) team(w http.ResponseWriter, _ *http.Request) {
	cfg := s.liveConfig()
	if cfg == nil {
		httpError(w, http.StatusNotImplemented, "the control panel needs hk serve")
		return
	}
	routes := map[string][]config.Route{}
	for name := range cfg.Channels {
		if s.Dash.Home != "" {
			if r, err := config.Routes(s.Dash.Home, name); err == nil {
				routes[name] = r
			}
		}
	}
	var out []teamAgent
	for _, name := range cfg.AgentNames() {
		ac := cfg.Agents[name]
		if ac == nil {
			continue
		}
		t := teamAgent{Name: name, Description: ac.Description, Model: ac.Model, Fallbacks: ac.Fallbacks,
			Toolsets: ac.Toolsets, Home: ac.Home, ReportHome: ac.ReportHome, Permission: ac.Permission, Approvals: ac.Approvals,
			Answers: []answerPlace{}}
		if t.Description == "" {
			t.Description = soulTitle(cfg.AgentDir(name))
		}
		// Loading fills in the defaults for an agent that lists none.
		if len(t.Toolsets) == 0 || slices.Equal(t.Toolsets, defaultToolsets) {
			t.DefaultToolsets, t.Toolsets = true, defaultToolsets
		}
		if t.Fallbacks == nil {
			t.Fallbacks = []string{}
		}
		if b := ac.Budget; b != nil {
			t.Daily, t.Monthly = b.Daily.Dollars, b.Monthly.Dollars
		}
		for _, ch := range slices.Sorted(mapsKeys(cfg.Channels)) {
			c := cfg.Channels[ch]
			if c == nil || !c.MessagingChannel() && c.Type != "web" {
				continue
			}
			if c.Agent == name {
				t.Answers = append(t.Answers, answerPlace{Channel: ch, Type: c.Type})
			}
			for _, r := range routes[ch] {
				if r.Agent == name {
					t.Answers = append(t.Answers, answerPlace{Channel: ch, Type: c.Type, Conversation: r.Conversation, Label: r.Label})
				}
			}
		}
		out = append(out, t)
	}
	toolsets := slices.Clone(BuiltinToolsets)
	for _, m := range slices.Sorted(mapsKeys(cfg.MCP)) {
		toolsets = append(toolsets, "mcp:"+m)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"agents":   out,
		"toolsets": toolsets,
		"writable": s.Dash.Home != "",
	})
}

// soulTitle is the first heading of an agent's SOUL.md, a fair one-line
// description of an agent that has none in the config.
func soulTitle(dir string) string {
	f, err := os.Open(filepath.Join(dir, "SOUL.md"))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for i := 0; sc.Scan() && i < 20; i++ {
		if line := strings.TrimSpace(sc.Text()); strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "# "))
		}
	}
	return ""
}

func (s *Server) agentSave(w http.ResponseWriter, r *http.Request) {
	if s.Dash == nil || s.Dash.Home == "" {
		httpError(w, http.StatusNotImplemented, "these settings cannot be saved from this process")
		return
	}
	var in config.AgentChange
	if !decodeJSON(w, r, &in) {
		return
	}
	for _, v := range []*float64{in.Daily, in.Monthly} {
		if v != nil && *v < 0 {
			httpError(w, http.StatusBadRequest, "a spending limit cannot be negative")
			return
		}
	}
	name := r.PathValue("name")
	if _, err := config.SetAgent(s.Dash.Home, name, in); err != nil {
		configError(w, err)
		return
	}
	s.Log.Info("agent settings saved from the dashboard", "agent", name)
	writeJSON(w, http.StatusOK, s.restartInfo())
}

func (s *Server) routingList(w http.ResponseWriter, _ *http.Request) {
	cfg := s.liveConfig()
	if cfg == nil {
		httpError(w, http.StatusNotImplemented, "the control panel needs hk serve")
		return
	}
	out := []routingView{}
	for _, name := range slices.Sorted(mapsKeys(cfg.Channels)) {
		c := cfg.Channels[name]
		if c == nil || !c.MessagingChannel() {
			continue
		}
		v := routingView{Account: name, Type: c.Type, Agent: c.Agent, Routes: []config.Route{}}
		if s.Dash.Home != "" {
			if r, err := config.Routes(s.Dash.Home, name); err == nil && r != nil {
				v.Routes = r
			}
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": out, "agents": cfg.AgentNames(), "writable": s.Dash.Home != ""})
}

func (s *Server) routingSave(w http.ResponseWriter, r *http.Request) {
	if s.Dash == nil || s.Dash.Home == "" {
		httpError(w, http.StatusNotImplemented, "these settings cannot be saved from this process")
		return
	}
	var in struct {
		Agent  string         `json:"agent"`
		Routes []config.Route `json:"routes"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	account := r.PathValue("account")
	if _, err := config.SetChannelRouting(s.Dash.Home, account, in.Agent, in.Routes); err != nil {
		configError(w, err)
		return
	}
	s.Log.Info("channel routing saved from the dashboard", "channel", account, "routes", len(in.Routes))
	writeJSON(w, http.StatusOK, s.restartInfo())
}

// configError answers a failed config edit: an unknown agent or channel is
// 404, anything else (it would not load, a bad value) 400.
func configError(w http.ResponseWriter, err error) {
	msg := err.Error()
	if strings.Contains(msg, "config.yaml has no ") {
		httpError(w, http.StatusNotFound, msg)
		return
	}
	httpError(w, http.StatusBadRequest, msg)
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	if s.Dash == nil || s.Dash.Models == nil {
		writeJSON(w, http.StatusOK, map[string]any{"providers": []ModelList{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": s.Dash.Models(r.Context())})
}

func (s *Server) skillOff(w http.ResponseWriter, r *http.Request) {
	if s.Dash == nil || s.Dash.Skills == nil {
		httpError(w, http.StatusNotImplemented, "skills are not available")
		return
	}
	var in struct {
		Off bool `json:"off"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	name := r.PathValue("name")
	if err := s.Dash.Skills.SetOff(name, in.Off); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "off": in.Off})
}

// restartInfo says whether saved changes wait for a restart, and whether
// this process can restart itself.
func (s *Server) restartInfo() map[string]any {
	pending := []string{}
	if s.Dash != nil && s.Dash.Pending != nil {
		if p := s.Dash.Pending(); p != nil {
			pending = p
		}
	}
	return map[string]any{"pending": pending, "can_restart": s.Dash != nil && s.Dash.Restart != nil}
}

func (s *Server) restartState(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.restartInfo())
}

func (s *Server) restart(w http.ResponseWriter, _ *http.Request) {
	if s.Dash == nil || s.Dash.Restart == nil {
		httpError(w, http.StatusNotImplemented, "this process cannot restart itself; restart hk serve")
		return
	}
	if err := s.Dash.Restart(); err != nil {
		if errors.Is(err, context.Canceled) {
			httpError(w, http.StatusConflict, "already restarting")
			return
		}
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.Log.Info("restart asked for from the dashboard")
	writeJSON(w, http.StatusAccepted, map[string]any{"restarting": true})
}

// extensions are the dashboard pages enabled plugins add.
func (s *Server) extensions() []Extension {
	out := []Extension{}
	if s.Dash == nil || s.Dash.Config == nil {
		return out
	}
	for _, p := range s.Dash.Config.Plugins {
		if p.DashboardDir() == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(p.DashboardDir(), "index.html")); err != nil {
			continue
		}
		out = append(out, Extension{Plugin: p.Name, Title: p.DashboardTitle(), URL: "ext/" + p.Name + "/"})
	}
	return out
}

// extFile serves a plugin's dashboard page and its files. Like the
// dashboard's own files they hold no data; the page asks the API, with the
// dashboard's token, for anything it shows.
func (s *Server) extFile(w http.ResponseWriter, r *http.Request) {
	if s.Dash == nil || s.Dash.Config == nil {
		http.NotFound(w, r)
		return
	}
	name := r.PathValue("plugin")
	var dir string
	for _, p := range s.Dash.Config.Plugins {
		if p.Name == name {
			dir = p.DashboardDir()
		}
	}
	file := r.PathValue("file")
	if file == "" {
		file = "index.html"
	}
	clean := path.Clean("/" + file)
	if dir == "" || strings.Contains(file, "\\") || clean == "/" {
		http.NotFound(w, r)
		return
	}
	full := filepath.Join(dir, filepath.FromSlash(clean))
	if rel, err := filepath.Rel(dir, full); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		http.NotFound(w, r)
		return
	}
	if fi, err := os.Lstat(full); err != nil || !fi.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	data, err := os.ReadFile(full)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	if t, ok := uiTypes[path.Ext(clean)]; ok {
		h.Set("Content-Type", t)
	} else {
		h.Set("Content-Type", "application/octet-stream")
	}
	h.Set("Cache-Control", "no-cache")
	h.Set("Content-Security-Policy", uiCSP)
	_, _ = w.Write(data)
}

func mapsKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}
