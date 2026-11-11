package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/retro"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/skills"
	"github.com/habakkuk-ai/habakkuk/internal/llm/budget"
	"github.com/habakkuk-ai/habakkuk/internal/llm/usage"
	"github.com/habakkuk-ai/habakkuk/internal/work/board"
	"github.com/habakkuk-ai/habakkuk/internal/work/schedule"
)

//go:embed ui
var ui embed.FS

// Dash is what the control dashboard at /app reads and changes beyond the
// gateway: the stores of the other subsystems, the effective config and the
// doctor. A nil field turns its pages' API off (501).
type Dash struct {
	Schedules *schedule.Store
	Scheduler *schedule.Scheduler
	Board     *board.Store
	BoardWake func() // look at the board now (the dispatcher)
	Memory    *memory.Store
	Skills    *skills.Library
	Retros    *retro.Runner
	Usage     *usage.Store
	Pricer    *usage.Pricer
	// Budget reads spend against the spending limits now.
	Budget func() budget.Report
	Config *config.Config
	// Secret resolves a variable the config names, so the config view can
	// scrub its value wherever it appears.
	Secret func(name string) string
	// Doctor checks config, credentials and storage, like hk doctor.
	Doctor func(ctx context.Context) []Check
}

// Check is one line of the doctor's report.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// dashRoutes mounts the dashboard page, its static files and its JSON API.
func (s *Server) dashRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /app", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/app/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /app/{file...}", s.uiFile)

	api := func(pattern string, h http.HandlerFunc) { mux.HandleFunc(pattern, s.auth(dashGuard(h))) }
	api("GET /api/app", s.appInfo)
	api("GET /api/overview", s.overview)
	api("GET /api/chat/conversations", s.conversations)
	api("GET /api/chat/{conversation}/live", s.live)
	api("GET /api/chat/{conversation}/messages/{seq}/speech", s.speech)
	api("GET /api/sessions", s.sessionList)
	api("GET /api/sessions/{id}", s.sessionGet)
	api("GET /api/sessions/{id}/export", s.sessionExport)
	api("GET /api/board", s.boardList)
	api("POST /api/board", s.boardCreate)
	api("GET /api/board/{id}", s.boardGet)
	api("POST /api/board/{id}/{action}", s.boardAction)
	api("GET /api/schedules", s.scheduleList)
	api("POST /api/schedules", s.scheduleCreate)
	api("POST /api/schedules/{id}/{action}", s.scheduleAction)
	api("DELETE /api/schedules/{id}", s.scheduleDelete)
	api("GET /api/memory/{agent}", s.memoryList)
	api("POST /api/memory/{agent}", s.memorySave)
	api("GET /api/memory/{agent}/{slug}", s.memoryGet)
	api("PUT /api/memory/{agent}/{slug}", s.memorySave)
	api("DELETE /api/memory/{agent}/{slug}", s.memoryForget)
	api("GET /api/memory-history/{agent}", s.historyList)
	api("GET /api/memory-history/{agent}/{id}", s.historyGet)
	api("POST /api/memory-history/{agent}/{id}/restore", s.historyRestore)
	api("GET /api/retros/{agent}", s.retroList)
	api("GET /api/retros/{agent}/{id}", s.retroGet)
	api("GET /api/skills/{agent}", s.skillList)
	api("GET /api/skills/{agent}/{name}", s.skillGet)
	api("GET /api/usage", s.usageReport)
	api("GET /api/budget", s.budgetReport)
	api("GET /api/approvals", s.approvals)
	api("POST /api/approvals/{id}", s.approve)
	api("GET /api/approval-rules", s.approvalRules)
	api("DELETE /api/approval-rules/{agent}/{id}", s.approvalRuleDelete)
	api("GET /api/config", s.configView)
	api("GET /api/channels", s.channelSettingsList)
	api("PUT /api/channels/{account}", s.channelSettingsSave)
	api("POST /api/realtime/session", s.realtimeSession)
	api("POST /api/realtime/tool", s.realtimeToolCall)
	api("POST /api/realtime/transcript", s.realtimeTranscript)
	api("POST /api/realtime/end", s.realtimeEnd)
	s.pushRoutes(api)
}

// dashGuard refuses requests a browser marks as coming from another site,
// and changes whose body is not JSON. Without a token the API answers any
// page on this machine, so a cross-site form post (which needs no
// preflight) must not be able to change anything.
func dashGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if crossSite(r) {
			httpError(w, http.StatusForbidden, "cross-site request refused")
			return
		}
		if r.Method == http.MethodPost || r.Method == http.MethodPut {
			if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
				httpError(w, http.StatusUnsupportedMediaType, "send application/json")
				return
			}
		}
		next(w, r)
	}
}

func crossSite(r *http.Request) bool {
	site := r.Header.Get("Sec-Fetch-Site")
	return site == "cross-site" || site == "same-site"
}

var uiTypes = map[string]string{
	".html":        "text/html; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".webmanifest": "application/manifest+json",
}

const uiCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; " +
	"img-src 'self' data: blob:; media-src 'self' blob:; manifest-src 'self'; worker-src 'self'; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// uiFile serves the dashboard's static files. They hold no data; the page
// asks for the token once and keeps it in localStorage.
func (s *Server) uiFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if name == "" {
		name = "index.html"
	}
	if strings.Contains(name, "/") {
		http.NotFound(w, r)
		return
	}
	data, err := ui.ReadFile("ui/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", uiTypes[path.Ext(name)])
	if path.Ext(name) == ".png" {
		// The home screen and notification icons; they rarely change.
		h.Set("Cache-Control", "public, max-age=86400")
	} else {
		// Revalidate every time: an update ships new files in the binary.
		h.Set("Cache-Control", "no-cache")
	}
	h.Set("Content-Security-Policy", s.uiPolicy())
	if name == "sw.js" {
		h.Set("Service-Worker-Allowed", "/app/")
	}
	w.Write(data)
}

func (s *Server) agentNames() []string {
	names := make([]string, 0, len(s.Gateway.Runner.Agents))
	for name := range s.Gateway.Runner.Agents {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// knownAgent writes a 404 and returns false when name is not configured.
func (s *Server) knownAgent(w http.ResponseWriter, name string) bool {
	if _, ok := s.Gateway.Runner.Agents[name]; !ok {
		httpError(w, http.StatusNotFound, "no agent "+strconv.Quote(name))
		return false
	}
	return true
}

func (s *Server) location() *time.Location {
	if loc := s.Gateway.Runner.Location; loc != nil {
		return loc
	}
	return time.Local
}

// dash returns the dashboard's stores, writing 501 when the app gave none.
func (s *Server) dash(w http.ResponseWriter) *Dash {
	if s.Dash == nil {
		httpError(w, http.StatusNotImplemented, "the dashboard API is not available in this process")
	}
	return s.Dash
}

func notAvailable(w http.ResponseWriter, what string) {
	httpError(w, http.StatusNotImplemented, what+" is not available in this process")
}

func (s *Server) appInfo(w http.ResponseWriter, _ *http.Request) {
	def := ""
	if ch := s.Gateway.Channels[s.Account]; ch != nil {
		def = ch.Agent
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account":       s.Account,
		"agents":        s.agentNames(),
		"default":       def,
		"transcription": s.Gateway.Transcribe != nil,
		"speech":        s.Gateway.Speech != nil,
		"realtime":      s.realtimeConfig() != nil,
		"timezone":      s.location().String(),
	})
}

type channelHealth struct {
	Account   string    `json:"account"`
	Type      string    `json:"type"`
	State     string    `json:"state"`
	Detail    string    `json:"detail,omitempty"`
	LastError string    `json:"last_error,omitempty"`
	Restarts  int       `json:"restarts"`
	Since     time.Time `json:"since"`
	LastBeat  time.Time `json:"last_beat"`
}

type jobView struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Agent     string    `json:"agent"`
	Session   string    `json:"session,omitempty"`
	Status    string    `json:"status"`
	Attempts  int       `json:"attempts"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// overview is the dashboard's front page: channel health, the queue, what
// is running, recent errors and what waits on the user.
func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := map[string]any{}
	sts, err := s.Gateway.Store.Statuses(ctx)
	if err != nil {
		s.fail(w, "read channel status", err)
		return
	}
	byName := map[string]channelHealth{}
	for _, st := range sts {
		byName[st.Account] = channelHealth{Account: st.Account, State: st.State, Detail: st.Detail, LastError: st.LastError,
			Restarts: st.Restarts, Since: st.Since.UTC(), LastBeat: st.LastBeat.UTC()}
	}
	names := make([]string, 0, len(s.Gateway.Channels))
	for name := range s.Gateway.Channels {
		names = append(names, name)
	}
	slices.Sort(names)
	chans := []channelHealth{}
	for _, name := range names {
		h, ok := byName[name]
		if !ok {
			h = channelHealth{Account: name, State: "never started"}
		}
		h.Type = s.Gateway.Channels[name].Type
		chans = append(chans, h)
	}
	out["channels"] = chans
	pending, running, err := s.Gateway.Queue.Depth(ctx)
	if err != nil {
		s.fail(w, "read queue depth", err)
		return
	}
	out["queue"] = map[string]int{"pending": pending, "running": running}
	run, err := s.Gateway.Queue.Running(ctx, 50)
	if err != nil {
		s.fail(w, "list running jobs", err)
		return
	}
	out["running"] = jobViews(run)
	failed, err := s.Gateway.Queue.Failures(ctx, time.Now().Add(-7*24*time.Hour), 20)
	if err != nil {
		s.fail(w, "list failed jobs", err)
		return
	}
	out["errors"] = jobViews(failed)
	out["approvals"] = len(s.Gateway.Approvals())
	// An install that is already in use does not need the first-run
	// pointers: the agent has answered someone, or has a schedule, or more
	// than one messaging channel is set up (setup leaves at most one).
	messaging := 0
	for _, ch := range s.Gateway.Channels {
		if ch.MessagingChannel() {
			messaging++
		}
	}
	established := messaging > 1
	if !established {
		if had, err := s.Gateway.Runner.Sessions.HasTurns(ctx); err == nil && had {
			established = true
		}
	}
	if d := s.Dash; d != nil && d.Board != nil {
		if cards, err := d.Board.List(ctx, board.Filter{Limit: 500}); err == nil {
			out["open_cards"] = len(cards)
		}
	}
	if d := s.Dash; d != nil && d.Schedules != nil {
		if list, err := d.Schedules.List(ctx, ""); err == nil {
			on := 0
			for _, sc := range list {
				if sc.Enabled {
					on++
				}
			}
			out["schedules"] = map[string]int{"total": len(list), "enabled": on}
			established = established || len(list) > 0
		}
	}
	out["established"] = established
	writeJSON(w, http.StatusOK, out)
}

func jobViews(list []jobs.Listed) []jobView {
	out := make([]jobView, 0, len(list))
	for _, j := range list {
		var p struct {
			Session string `json:"session"`
		}
		_ = json.Unmarshal(j.Payload, &p)
		out = append(out, jobView{ID: j.ID, Kind: j.Kind, Agent: j.Agent, Session: p.Session, Status: string(j.Status),
			Attempts: j.Attempts, Error: shortText(j.LastError, 500), CreatedAt: j.CreatedAt.UTC(), UpdatedAt: j.UpdatedAt.UTC()})
	}
	return out
}

// shortText cuts s to n runes, marking the cut.
func shortText(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// fail logs an internal error and answers 500 without its text.
func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	s.Log.Error("dashboard: "+what+" failed", "err", err)
	httpError(w, http.StatusInternalServerError, "could not "+what)
}

// userError answers 400 with the error's text: for validation errors the
// stores return, which are written for the person using them.
func userError(w http.ResponseWriter, err error) {
	httpError(w, http.StatusBadRequest, err.Error())
}

func intParam(r *http.Request, name string, def, lo, hi int) int {
	n, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil {
		return def
	}
	return min(max(n, lo), hi)
}

// decodeJSON reads a JSON body into v, rejecting unknown fields so a typo
// in a field name is an error rather than a silent no-op.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	body, ok := readBody(w, r)
	if !ok {
		return false
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		body = []byte("{}")
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var syn *json.SyntaxError
		if errors.As(err, &syn) {
			httpError(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		} else {
			httpError(w, http.StatusBadRequest, err.Error())
		}
		return false
	}
	return true
}
