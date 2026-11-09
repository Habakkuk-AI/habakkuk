package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/skills"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/llm/usage"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
	"github.com/habakkuk-ai/habakkuk/internal/tools/mediatools"
	"github.com/habakkuk-ai/habakkuk/internal/work/board"
	"github.com/habakkuk-ai/habakkuk/internal/work/schedule"
)

type fakeSpeaker struct{ said []string }

func (f *fakeSpeaker) Synthesize(_ context.Context, text, _ string) (mediatools.Audio, error) {
	f.said = append(f.said, text)
	return mediatools.Audio{Data: []byte("OggS fake audio"), MediaType: "audio/ogg", Ext: ".ogg"}, nil
}

type dashRig struct {
	*rig
	home  string
	usage *usage.Store
	heard chan string // what the fake transcriber was asked to transcribe (file names)
}

// newDashRig is the web rig plus every store behind the dashboard, a fake
// transcriber and a fake speaker.
func newDashRig(t *testing.T) *dashRig {
	t.Helper()
	r := newRig(t, token)
	ctx := context.Background()
	home := t.TempDir()
	migs := slices.Concat(jobs.Migrations, channel.Migrations, schedule.Migrations, board.Migrations)
	slices.SortStableFunc(migs, func(a, b store.Migration) int { return a.Version - b.Version })
	db, err := store.Open(ctx, filepath.Join(home, "control.db"), migs)
	if err != nil {
		t.Fatal(err)
	}
	us, err := usage.Open(ctx, filepath.Join(home, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	mem, err := memory.Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(); us.Close(); mem.Close() })
	q := jobs.NewQueue(db)
	sch := schedule.NewStore(db, time.UTC)
	cfg := &config.Config{Version: 1, Home: home, Agents: map[string]*config.Agent{"habakkuk": {Model: "fake/m"}}}
	r.s.Dash = &Dash{
		Schedules: sch, Scheduler: &schedule.Scheduler{Store: sch, Queue: q, Log: quiet},
		Board: board.NewStore(db, q), Memory: mem, Skills: &skills.Library{Home: home},
		Usage: us, Pricer: usage.NewPricer(cfg), Config: cfg, Secret: func(string) string { return "" },
		Doctor: func(context.Context) []Check { return []Check{{Name: "control.db", OK: true, Detail: "ok"}} },
	}
	heard := make(chan string, 10)
	r.s.Gateway.Transcribe = func(_ context.Context, name, mediaType string, data []byte) (string, error) {
		heard <- name + " " + mediaType
		return "turn on the lights", nil
	}
	r.s.Gateway.Speech = &fakeSpeaker{}
	return &dashRig{rig: r, home: home, usage: us, heard: heard}
}

func (r *dashRig) get(t *testing.T, path string) map[string]any {
	t.Helper()
	code, out := r.do(t, "GET", path, token, "", "")
	if code != http.StatusOK {
		t.Fatalf("GET %s: %d %v", path, code, out)
	}
	return out
}

func (r *dashRig) send(t *testing.T, method, path, body string, want int) map[string]any {
	t.Helper()
	code, out := r.do(t, method, path, token, "application/json", body)
	if code != want {
		t.Fatalf("%s %s: %d %v, want %d", method, path, code, out, want)
	}
	return out
}

func list(m map[string]any, key string) []any {
	v, _ := m[key].([]any)
	return v
}

func TestDashNeedsTokenAndRefusesCrossSite(t *testing.T) {
	r := newDashRig(t)
	paths := []string{"/api/app", "/api/overview", "/api/sessions", "/api/board", "/api/schedules", "/api/memory/habakkuk",
		"/api/skills/habakkuk", "/api/usage", "/api/approvals", "/api/config", "/api/chat/conversations"}
	for _, p := range paths {
		if code, _ := r.do(t, "GET", p, "", "", ""); code != http.StatusUnauthorized {
			t.Errorf("%s without token: %d", p, code)
		}
		if code, _ := r.do(t, "GET", p, "wrong", "", ""); code != http.StatusUnauthorized {
			t.Errorf("%s with a wrong token: %d", p, code)
		}
		if code, out := r.do(t, "GET", p, token, "", ""); code != http.StatusOK {
			t.Errorf("%s: %d %v", p, code, out)
		}
		if code, _ := r.do(t, "GET", p, token, "", "", "Sec-Fetch-Site", "cross-site"); code != http.StatusForbidden {
			t.Errorf("%s cross-site: %d", p, code)
		}
	}
	// A change must be JSON, which a cross-site form cannot send.
	if code, _ := r.do(t, "POST", "/api/board", token, "text/plain", `{"title":"x"}`); code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain post: %d", code)
	}
	if code, _ := r.do(t, "POST", "/api/schedules/x/run", token, "", ""); code != http.StatusUnsupportedMediaType {
		t.Errorf("bodiless post: %d", code)
	}
}

func TestDashServesTheAppWithoutData(t *testing.T) {
	r := newDashRig(t)
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := noRedirect.Get(r.ts.URL + "/app")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMovedPermanently || res.Header.Get("Location") != "/app/" {
		t.Fatalf("/app: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	for path, ctype := range map[string]string{
		"/app/":                      "text/html",
		"/app/app.js":                "text/javascript",
		"/app/app.css":               "text/css",
		"/app/sw.js":                 "text/javascript",
		"/app/manifest.webmanifest":  "application/manifest+json",
		"/app/icon.svg":              "image/svg+xml",
		"/app/icon-180.png":          "image/png",
		"/app/icon-192.png":          "image/png",
		"/app/icon-512.png":          "image/png",
		"/app/icon-maskable-512.png": "image/png",
		"/app/pair.html":             "text/html",
	} {
		res, err := http.Get(r.ts.URL + path) // no token: the shell holds no data
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), ctype) {
			t.Errorf("%s: %d %s", path, res.StatusCode, res.Header.Get("Content-Type"))
		}
		if strings.HasSuffix(path, ".png") {
			// icon-180.png is 180 pixels square, icon-maskable-512.png 512.
			name := strings.TrimSuffix(path[strings.LastIndex(path, "-")+1:], ".png")
			img, err := png.Decode(bytes.NewReader(body))
			if err != nil || strconv.Itoa(img.Bounds().Dx()) != name || img.Bounds().Dy() != img.Bounds().Dx() {
				t.Errorf("%s: %v", path, err)
			}
		} else if !strings.Contains(res.Header.Get("Content-Security-Policy"), "script-src 'self'") {
			t.Errorf("%s: CSP %q", path, res.Header.Get("Content-Security-Policy"))
		}
	}
	for _, path := range []string{"/app/nope.js", "/app/icon-77.png", "/app/../web.go"} {
		res, err := http.Get(r.ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode == http.StatusOK {
			t.Errorf("%s served", path)
		}
	}
}

func TestDashOverviewAndApp(t *testing.T) {
	r := newDashRig(t)
	app := r.get(t, "/api/app")
	if app["default"] != "habakkuk" || app["speech"] != true || app["transcription"] != true || len(list(app, "agents")) != 2 {
		t.Fatalf("app: %v", app)
	}
	o := r.get(t, "/api/overview")
	chans := list(o, "channels")
	if len(chans) != 1 || chans[0].(map[string]any)["account"] != "web" || chans[0].(map[string]any)["type"] != "web" {
		t.Fatalf("channels: %v", chans)
	}
	if _, ok := o["queue"].(map[string]any)["pending"]; !ok {
		t.Fatalf("queue: %v", o["queue"])
	}
	if o["approvals"] != float64(0) || o["open_cards"] != float64(0) {
		t.Fatalf("overview: %v", o)
	}
}

func TestDashSessionsTranscriptSearchExport(t *testing.T) {
	r := newDashRig(t)
	st := r.open(t, "/api/chat/s1/events")
	r.chat(t, "s1", "hello marmalade")
	st.next(t)
	if e := st.next(t); e.Text != "echo: hello marmalade" {
		t.Fatalf("reply: %+v", e)
	}
	sessions := list(r.get(t, "/api/sessions"), "sessions")
	if len(sessions) != 1 {
		t.Fatalf("sessions: %v", sessions)
	}
	id := sessions[0].(map[string]any)["id"].(string)
	got := r.get(t, "/api/sessions/"+id)
	msgs := list(got, "messages")
	if len(msgs) != 2 {
		t.Fatalf("messages: %v", msgs)
	}
	blocks := list(msgs[1].(map[string]any), "blocks")
	if blocks[0].(map[string]any)["text"] != "echo: hello marmalade" {
		t.Fatalf("assistant blocks: %v", blocks)
	}
	hits := list(r.get(t, "/api/sessions?q=marmalade"), "hits")
	if len(hits) == 0 || hits[0].(map[string]any)["session"] != id {
		t.Fatalf("hits: %v", hits)
	}
	if code, _ := r.do(t, "GET", "/api/sessions?agent=ghost", token, "", ""); code != http.StatusNotFound {
		t.Errorf("unknown agent: %d", code)
	}
	req, _ := http.NewRequest("GET", r.ts.URL+"/api/sessions/"+id+"/export", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(res.Header.Get("Content-Disposition"), id+".md") || !strings.Contains(string(body), "echo: hello marmalade") {
		t.Fatalf("export: %s %q", res.Header.Get("Content-Disposition"), body)
	}
	if code, _ := r.do(t, "GET", "/api/sessions/ses_nope", token, "", ""); code != http.StatusNotFound {
		t.Errorf("missing session: %d", code)
	}
	convs := list(r.get(t, "/api/chat/conversations"), "conversations")
	if len(convs) != 1 || convs[0].(map[string]any)["conversation"] != "s1" || convs[0].(map[string]any)["session"] != id {
		t.Fatalf("conversations: %v", convs)
	}
}

func TestDashBoard(t *testing.T) {
	r := newDashRig(t)
	r.send(t, "POST", "/api/board", `{"title":"x","assignee":"ghost"}`, http.StatusNotFound)
	r.send(t, "POST", "/api/board", `{"title":"x","owner":"me"}`, http.StatusBadRequest)
	r.send(t, "POST", "/api/board", `{"title":"  "}`, http.StatusBadRequest)
	c := r.send(t, "POST", "/api/board", `{"title":"Write the report","body":"weekly","assignee":"habakkuk","priority":2}`, http.StatusCreated)
	id := c["id"].(string)
	if c["status"] != "todo" || c["created_by"] != board.User {
		t.Fatalf("card: %v", c)
	}
	if cards := list(r.get(t, "/api/board"), "cards"); len(cards) != 1 {
		t.Fatalf("cards: %v", cards)
	}
	c = r.send(t, "POST", "/api/board/"+id+"/comment", `{"text":"use last week's numbers"}`, http.StatusOK)
	if cm := list(c, "comments"); len(cm) != 1 || cm[0].(map[string]any)["text"] != "use last week's numbers" {
		t.Fatalf("comments: %v", c["comments"])
	}
	c = r.send(t, "POST", "/api/board/"+id+"/assign", `{"assignee":"ops"}`, http.StatusOK)
	if c["assignee"] != "ops" {
		t.Fatalf("assign: %v", c)
	}
	r.send(t, "POST", "/api/board/"+id+"/assign", `{"assignee":"ghost"}`, http.StatusNotFound)
	c = r.send(t, "POST", "/api/board/"+id+"/cancel", `{}`, http.StatusOK)
	if c["status"] != "cancelled" {
		t.Fatalf("cancel: %v", c)
	}
	r.send(t, "POST", "/api/board/"+id+"/cancel", `{}`, http.StatusBadRequest)
	if cards := list(r.get(t, "/api/board"), "cards"); len(cards) != 0 {
		t.Fatalf("open cards after cancel: %v", cards)
	}
	if cards := list(r.get(t, "/api/board?status=all"), "cards"); len(cards) != 1 {
		t.Fatalf("all cards: %v", cards)
	}
	got := r.get(t, "/api/board/"+id)
	if len(list(got, "events")) == 0 {
		t.Fatalf("history: %v", got)
	}
	r.send(t, "POST", "/api/board/"+id+"/explode", `{}`, http.StatusNotFound)
}

func TestDashSchedules(t *testing.T) {
	r := newDashRig(t)
	r.send(t, "POST", "/api/schedules", `{"agent":"habakkuk","name":"x","when":"@every 1h","prompt":"p","command":"rm -rf /"}`, http.StatusBadRequest)
	r.send(t, "POST", "/api/schedules", `{"agent":"ghost","name":"x","when":"@every 1h","prompt":"p"}`, http.StatusNotFound)
	r.send(t, "POST", "/api/schedules", `{"agent":"habakkuk","name":"x","when":"whenever","prompt":"p"}`, http.StatusBadRequest)
	r.send(t, "POST", "/api/schedules", `{"agent":"habakkuk","name":"x","when":"@every 1h","prompt":"p","deliver_to":"nowhere:c1"}`, http.StatusBadRequest)
	sc := r.send(t, "POST", "/api/schedules",
		`{"agent":"habakkuk","name":"brief","when":"0 8 * * *","prompt":"Morning brief","deliver_to":"web:c-1"}`, http.StatusCreated)
	id := sc["id"].(string)
	if sc["enabled"] != true || sc["next_run"] == nil || sc["deliver_to"].(map[string]any)["conversation"] != "c-1" {
		t.Fatalf("schedule: %v", sc)
	}
	out := r.send(t, "POST", "/api/schedules/"+id+"/pause", `{}`, http.StatusOK)
	if out["schedule"].(map[string]any)["enabled"] != false {
		t.Fatalf("pause: %v", out)
	}
	out = r.send(t, "POST", "/api/schedules/"+id+"/resume", `{}`, http.StatusOK)
	if out["schedule"].(map[string]any)["enabled"] != true {
		t.Fatalf("resume: %v", out)
	}
	out = r.send(t, "POST", "/api/schedules/"+id+"/run", `{}`, http.StatusOK)
	if job, _ := out["job"].(string); !strings.HasPrefix(job, "sch_") {
		t.Fatalf("run: %v", out)
	}
	if l := list(r.get(t, "/api/schedules"), "schedules"); len(l) != 1 {
		t.Fatalf("list: %v", l)
	}
	if code, _ := r.do(t, "DELETE", "/api/schedules/"+id, token, "", ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if code, _ := r.do(t, "DELETE", "/api/schedules/"+id, token, "", ""); code != http.StatusNotFound {
		t.Fatalf("delete again: %d", code)
	}
	r.send(t, "POST", "/api/schedules/"+id+"/run", `{}`, http.StatusNotFound)
}

func TestDashMemory(t *testing.T) {
	r := newDashRig(t)
	r.send(t, "POST", "/api/memory/ghost", `{"title":"x","content":"y"}`, http.StatusNotFound)
	r.send(t, "POST", "/api/memory/habakkuk", `{"title":"x","content":""}`, http.StatusBadRequest)
	m := r.send(t, "POST", "/api/memory/habakkuk", `{"title":"Coffee order","content":"Flat white, oat milk","tags":["food"]}`, http.StatusCreated)
	if m["slug"] != "coffee-order" {
		t.Fatalf("save: %v", m)
	}
	notes := r.get(t, "/api/memory/habakkuk")
	if notes["total"] != float64(1) {
		t.Fatalf("list: %v", notes)
	}
	hits := list(r.get(t, "/api/memory/habakkuk?q=oat"), "memories")
	if len(hits) != 1 || !strings.Contains(hits[0].(map[string]any)["snippet"].(string), "[oat]") {
		t.Fatalf("search: %v", hits)
	}
	if got := r.get(t, "/api/memory/habakkuk/coffee-order"); got["body"] != "Flat white, oat milk" {
		t.Fatalf("read: %v", got)
	}
	// Renaming saves under the new slug and trashes the old file.
	m = r.send(t, "PUT", "/api/memory/habakkuk/coffee-order", `{"title":"Coffee","content":"Cortado now"}`, http.StatusCreated)
	if m["slug"] != "coffee" || len(list(m, "tags")) != 0 {
		t.Fatalf("rename: %v", m)
	}
	if code, _ := r.do(t, "GET", "/api/memory/habakkuk/coffee-order", token, "", ""); code != http.StatusNotFound {
		t.Fatalf("old slug still there: %d", code)
	}
	r.send(t, "PUT", "/api/memory/habakkuk/coffee", `{"title":"Coffee","content":"Cortado, no sugar","tags":["food"]}`, http.StatusOK)
	r.send(t, "PUT", "/api/memory/habakkuk/nope", `{"title":"Nope","content":"x"}`, http.StatusNotFound)
	if code, _ := r.do(t, "DELETE", "/api/memory/habakkuk/coffee", token, "", ""); code != http.StatusNoContent {
		t.Fatalf("forget: %d", code)
	}
	if notes := r.get(t, "/api/memory/habakkuk"); notes["total"] != float64(0) {
		t.Fatalf("after forget: %v", notes)
	}
	trash, _ := os.ReadDir(filepath.Join(r.home, "agents", "habakkuk", "memory", ".trash"))
	if len(trash) != 2 {
		t.Fatalf("trash: %v", trash)
	}
}

func TestDashSkills(t *testing.T) {
	r := newDashRig(t)
	dir := filepath.Join(r.home, "skills", "weekly-report")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: weekly-report\ndescription: Write the weekly report.\n---\n\n1. Read the board.\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "template.md"), []byte("# Week"), 0o644)
	sk := list(r.get(t, "/api/skills/habakkuk"), "skills")
	if len(sk) != 1 || sk[0].(map[string]any)["description"] != "Write the weekly report." || sk[0].(map[string]any)["shared"] != true {
		t.Fatalf("skills: %v", sk)
	}
	got := r.get(t, "/api/skills/habakkuk/weekly-report")
	if !strings.Contains(got["body"].(string), "Read the board") || len(list(got, "files")) != 1 {
		t.Fatalf("skill: %v", got)
	}
	if code, _ := r.do(t, "GET", "/api/skills/habakkuk/nope", token, "", ""); code != http.StatusNotFound {
		t.Errorf("missing skill: %d", code)
	}
	if code, _ := r.do(t, "GET", "/api/skills/ghost", token, "", ""); code != http.StatusNotFound {
		t.Errorf("unknown agent: %d", code)
	}
}

func TestDashUsage(t *testing.T) {
	r := newDashRig(t)
	ctx := context.Background()
	now := time.Now()
	for _, c := range []usage.Call{
		{At: now, Provider: "anthropic", Model: "claude-sonnet-4-5", Agent: "habakkuk", Usage: llm.Usage{InputTokens: 1_000_000, OutputTokens: 100_000}},
		{At: now.Add(-48 * time.Hour), Provider: "local", Model: "mystery-model", Agent: "ops", Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}},
		{At: now.AddDate(0, 0, -40), Provider: "anthropic", Model: "claude-sonnet-4-5", Agent: "habakkuk", Usage: llm.Usage{InputTokens: 7}},
	} {
		if err := r.usage.Record(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	u := r.get(t, "/api/usage?days=7")
	total := u["total"].(map[string]any)
	if total["calls"] != float64(2) || total["input_tokens"] != float64(1_000_010) || total["unpriced"] != float64(1) {
		t.Fatalf("total: %v", total)
	}
	if cost := total["cost"].(float64); cost <= 0 {
		t.Fatalf("cost: %v", cost)
	}
	days := list(u, "days")
	if len(days) != 7 || days[6].(map[string]any)["calls"] != float64(1) {
		t.Fatalf("days: %v", days)
	}
	models := list(u, "by_model")
	if len(models) != 2 || models[0].(map[string]any)["key"] != "anthropic/claude-sonnet-4-5" {
		t.Fatalf("by model: %v", models)
	}
	if agents := list(r.get(t, "/api/usage?days=7&agent=ops"), "by_agent"); len(agents) != 1 || agents[0].(map[string]any)["key"] != "ops" {
		t.Fatalf("by agent: %v", agents)
	}
}

func TestDashApprovals(t *testing.T) {
	r := newDashRig(t)
	ctx := context.Background()
	g := r.s.Gateway
	sess, err := g.Runner.NewSession(ctx, "habakkuk")
	if err != nil {
		t.Fatal(err)
	}
	to := channel.Address{Account: "web", Conversation: "ap1"}
	if err := g.Store.Bind(ctx, to, "habakkuk", sess.ID); err != nil {
		t.Fatal(err)
	}
	st := r.open(t, "/api/chat/ap1/events")
	answer := make(chan bool, 1)
	go func() {
		ok, _ := g.ApproverFor(sess.ID).Approve(ctx, tools.ApprovalRequest{Tool: "exec", Summary: "rm -rf build/", Review: "deletes files"})
		answer <- ok
	}()
	if e := st.next(t); !strings.Contains(e.Text, "Approval needed") {
		t.Fatalf("prompt: %+v", e)
	}
	var id string
	waitFor(t, "approval listed", func() bool {
		l := list(r.get(t, "/api/approvals"), "approvals")
		if len(l) == 1 {
			a := l[0].(map[string]any)
			id, _ = a["id"].(string)
			return a["tool"] == "exec" && a["summary"] == "rm -rf build/" && a["session"] == sess.ID
		}
		return false
	})
	if o := r.get(t, "/api/overview"); o["approvals"] != float64(1) {
		t.Fatalf("overview approvals: %v", o["approvals"])
	}
	r.send(t, "POST", "/api/approvals/"+id, `{}`, http.StatusBadRequest)
	r.send(t, "POST", "/api/approvals/apr_nope", `{"approve":true}`, http.StatusNotFound)
	r.send(t, "POST", "/api/approvals/"+id, `{"approve":true}`, http.StatusOK)
	select {
	case ok := <-answer:
		if !ok {
			t.Fatal("approval denied")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("approver still waiting")
	}
	if e := st.next(t); e.Text != "Approved from the dashboard." {
		t.Fatalf("notice: %+v", e)
	}
	if l := list(r.get(t, "/api/approvals"), "approvals"); len(l) != 0 {
		t.Fatalf("still listed: %v", l)
	}
	r.send(t, "POST", "/api/approvals/"+id, `{"approve":false}`, http.StatusNotFound)
}

// upload posts a multipart chat message.
func (r *dashRig) upload(t *testing.T, fields map[string]string, files map[string][]byte, audio []byte, hdr bool) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	for name, data := range files {
		fw, _ := mw.CreateFormFile("file", name)
		fw.Write(data)
	}
	if audio != nil {
		fw, _ := mw.CreateFormFile("audio", "voice.webm")
		fw.Write(audio)
	}
	mw.Close()
	extra := []string{}
	if hdr {
		extra = append(extra, "X-Habakkuk-Client", "test")
	}
	return r.do(t, "POST", "/api/chat", token, mw.FormDataContentType(), buf.String(), extra...)
}

func TestChatUploadTranscribesAudio(t *testing.T) {
	r := newDashRig(t)
	st := r.open(t, "/api/chat/v1/events")
	// WebM magic, so the type is sniffed rather than trusted.
	webm := append([]byte{0x1a, 0x45, 0xdf, 0xa3}, make([]byte, 64)...)
	if code, _ := r.upload(t, map[string]string{"conversation": "v1"}, nil, webm, false); code != http.StatusForbidden {
		t.Fatalf("upload without the client header: %d", code)
	}
	code, out := r.upload(t, map[string]string{"conversation": "v1", "id": "rec-1"}, nil, webm, true)
	if code != http.StatusAccepted || out["transcript"] != "turn on the lights" || out["turn"] == "" {
		t.Fatalf("upload: %d %v", code, out)
	}
	select {
	case got := <-r.heard:
		if !strings.HasPrefix(got, "voice.webm audio/webm") {
			t.Fatalf("transcriber got %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("not transcribed")
	}
	if e := st.next(t); e.Role != "user" || e.Text != "turn on the lights" {
		t.Fatalf("logged: %+v", e)
	}
	if e := st.next(t); e.Text != "echo: turn on the lights" {
		t.Fatalf("reply: %+v", e)
	}
	// The same message id again is a no-op, as for JSON messages.
	r.upload(t, map[string]string{"conversation": "v1", "id": "rec-1"}, nil, webm, true)
	st.none(t)

	// Without a transcriber the recording is refused, not sent silently.
	r.s.Gateway.Transcribe = nil
	if code, _ := r.upload(t, map[string]string{"conversation": "v1"}, nil, webm, true); code != http.StatusNotImplemented {
		t.Fatalf("no transcriber: %d", code)
	}
	if code, _ := r.upload(t, map[string]string{"conversation": "v1"}, nil, nil, true); code != http.StatusBadRequest {
		t.Fatalf("empty upload: %d", code)
	}
}

func TestChatUploadFilesBecomeAttachments(t *testing.T) {
	r := newDashRig(t)
	st := r.open(t, "/api/chat/f1/events")
	pngData := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 32))
	code, out := r.upload(t, map[string]string{"conversation": "f1", "text": "what is this?"},
		map[string][]byte{"../../photo 1.png": pngData, "notes.txt": []byte("hello")}, nil, true)
	if code != http.StatusAccepted {
		t.Fatalf("upload: %d %v", code, out)
	}
	e := st.next(t)
	if !strings.HasPrefix(e.Text, "what is this?\n\nAttached: ") || strings.Contains(e.Text, "..") {
		t.Fatalf("logged: %q", e.Text)
	}
	j, err := r.q.Get(context.Background(), out["turn"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var p agent.TurnPayload
	if err := j.Decode(&p); err != nil {
		t.Fatal(err)
	}
	if len(p.Media) != 2 {
		t.Fatalf("media: %+v", p.Media)
	}
	types := map[string]string{}
	for _, m := range p.Media {
		types[m.Name] = m.MediaType
		if m.Path == "" {
			t.Errorf("%s not saved to the inbox", m.Name)
		}
	}
	if !strings.HasPrefix(types["photo_1.png"], "image/png") || !strings.HasPrefix(types["notes.txt"], "text/plain") {
		t.Fatalf("types: %v", types)
	}
	many := map[string][]byte{}
	for i := range maxUploadFiles + 1 {
		many[fmt.Sprintf("f%d.txt", i)] = []byte("x")
	}
	if code, _ := r.upload(t, map[string]string{"conversation": "f1"}, many, nil, true); code != http.StatusBadRequest {
		t.Fatalf("too many files: %d", code)
	}
	if code, _ := r.upload(t, map[string]string{"conversation": "bad id!"}, map[string][]byte{"a.txt": []byte("x")}, nil, true); code != http.StatusBadRequest {
		t.Fatalf("bad conversation: %d", code)
	}
}

func TestDashSpeech(t *testing.T) {
	r := newDashRig(t)
	st := r.open(t, "/api/chat/sp1/events")
	r.chat(t, "sp1", "say **hi**")
	user := st.next(t)
	reply := st.next(t)
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/chat/sp1/messages/%d/speech", r.ts.URL, reply.Seq), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "audio/ogg" || string(body) != "OggS fake audio" {
		t.Fatalf("speech: %d %s %q", res.StatusCode, res.Header.Get("Content-Type"), body)
	}
	if said := r.s.Gateway.Speech.(*fakeSpeaker).said; len(said) != 1 || strings.Contains(said[0], "**") {
		t.Fatalf("spoke %q (Markdown should be stripped)", said)
	}
	if code, _ := r.do(t, "GET", fmt.Sprintf("/api/chat/sp1/messages/%d/speech", user.Seq), token, "", ""); code != http.StatusNotFound {
		t.Fatalf("speaking a user message: %d", code)
	}
	if code, _ := r.do(t, "GET", "/api/chat/sp1/messages/99999/speech", token, "", ""); code != http.StatusNotFound {
		t.Fatalf("missing message: %d", code)
	}
	r.s.Gateway.Speech = nil
	if code, _ := r.do(t, "GET", fmt.Sprintf("/api/chat/sp1/messages/%d/speech", reply.Seq), token, "", ""); code != http.StatusNotImplemented {
		t.Fatalf("no tts: %d", code)
	}
}

func TestDashLiveStreamsActivity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("times out on the Windows CI runner; needs a Windows machine to debug (tracked in docs/contributing/roadmap.md)")
	}
	r := newDashRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", r.ts.URL+"/api/chat/lv1/live", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var mu sync.Mutex
	var kinds []string
	session := ""
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(res.Body)
		event := ""
		for sc.Scan() {
			line := sc.Text()
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				event = v
				continue
			}
			data, ok := strings.CutPrefix(line, "data: ")
			if !ok {
				continue
			}
			var m map[string]any
			json.Unmarshal([]byte(data), &m)
			mu.Lock()
			switch event {
			case "session":
				session, _ = m["session"].(string)
			case "activity":
				kinds = append(kinds, m["kind"].(string))
			}
			mu.Unlock()
		}
	}()
	r.chat(t, "lv1", "stream me")
	waitFor(t, "live activity", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return session != "" && slices.Contains(kinds, "text") && slices.Contains(kinds, "turn_done")
	})
	cancel()
	<-done
}

func TestRedactConfig(t *testing.T) {
	cfg, err := config.Parse([]byte(`
version: 1
providers:
  openai:
    type: openai
    keys_env: [OPENAI_KEY]
agents:
  habakkuk:
    model: openai/gpt-5
channels:
  tg:
    type: telegram
    token_env: TG_TOKEN
    api_url: https://bot:hunter22@api.example.com/x?token=abc123456789&page=2
mcp_servers:
  files:
    command: files-mcp
    args: ["--key", "sk-live-0123456789abcdefghij", "--root", "/srv"]
    env:
      FILES_TOKEN: MCP_FILES_SECRET
  remote:
    url: https://mcp.example.com/mcp
    headers:
      Authorization: "Bearer ${REMOTE_TOKEN}"
      X-Api-Key: literal-header-secret-value
hooks:
  - event: turn_end
    command: 'curl -H "x-key: tg-0987654321" https://hooks.example.com'
`))
	if err != nil {
		t.Fatal(err)
	}
	secrets := map[string]string{
		"OPENAI_KEY":       "opk-aaaabbbbccccdddd",
		"TG_TOKEN":         "123456:telegram-secret",
		"MCP_FILES_SECRET": "files-secret-value",
		"REMOTE_TOKEN":     "remote-secret-value",
		"HOOK_KEY":         "tg-0987654321", // named nowhere, so it is not known
	}
	// A secret value that also appears in a field it was not read from.
	cfg.Agents["habakkuk"].Workspace = "/home/me/opk-aaaabbbbccccdddd"
	tree, err := RedactConfig(cfg, func(name string) string { return secrets[name] })
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(tree)
	out := string(data)
	for _, leak := range []string{"opk-aaaabbbbccccdddd", "telegram-secret", "files-secret-value", "remote-secret-value",
		"hunter22", "abc123456789", "sk-live-0123456789abcdefghij", "literal-header-secret-value"} {
		if strings.Contains(out, leak) {
			t.Errorf("leaked %q in %s", leak, out)
		}
	}
	// Variable names, templates and ordinary values stay readable.
	for _, keep := range []string{"OPENAI_KEY", "TG_TOKEN", "MCP_FILES_SECRET", `Bearer ${REMOTE_TOKEN}`, "openai/gpt-5", "page=2", "/srv", "files-mcp"} {
		if !strings.Contains(out, keep) {
			t.Errorf("lost %q in %s", keep, out)
		}
	}
	// And through the API, the doctor's details are scrubbed too.
	r := newDashRig(t)
	r.s.Dash.Config = cfg
	r.s.Dash.Secret = func(name string) string { return secrets[name] }
	r.s.Dash.Doctor = func(context.Context) []Check {
		return []Check{{Name: "provider openai", OK: false, Detail: "rejected key opk-aaaabbbbccccdddd"}}
	}
	got := r.get(t, "/api/config")
	data, _ = json.Marshal(got)
	if strings.Contains(string(data), "opk-aaaabbbbccccdddd") || !strings.Contains(string(data), "provider openai") {
		t.Fatalf("config view: %s", data)
	}
}
