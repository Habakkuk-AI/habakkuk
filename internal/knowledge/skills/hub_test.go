package skills

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/knowledge/hub"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

func tarball(t *testing.T, files map[string]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for n, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: "acme-tools-abc1234/" + n, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

const commit = "abc1234abc1234abc1234abc1234abc1234abc12"

func TestHubInstallFromGitHubAndInvoke(t *testing.T) {
	files := map[string]string{
		"SKILL.md":         skillMD("pdf-forms", "Fill PDF forms.", "1. Read the form.\n2. Fill it."),
		"scripts/fill.py":  "print('fill')\n",
		"reference/fields": "name, date",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/tools/commits/HEAD":
			fmt.Fprint(w, commit)
		case "/repos/acme/tools/tarball/" + commit:
			w.Write(tarball(t, files))
		case "/index.json":
			json.NewEncoder(w).Encode(hub.Index{Version: 1, Skills: []hub.IndexEntry{{Name: "pdf-forms", Description: "Fill PDF forms", Source: "github:acme/tools"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	l := &Library{Home: t.TempDir()}
	h := l.Hub(&hub.Fetcher{GitHubAPI: srv.URL, Registries: []hub.Registry{{Name: "main", URL: srv.URL + "/index.json"}}})
	res, err := h.Install(context.Background(), "pdf-forms", hub.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Name != "pdf-forms" || res[0].Entry.Version != commit || res[0].Entry.Registry != "main" {
		t.Fatalf("%+v", res[0])
	}
	// It ships a script and is not approved: disabled, neither listed nor
	// loaded.
	if list, _ := l.List("habakkuk"); len(list) != 0 {
		t.Fatalf("unapproved skill with scripts listed: %+v", list)
	}
	if _, err := l.Invoke("habakkuk", "pdf-forms", ""); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("unapproved skill with scripts invoked: %v", err)
	}
	r := tools.NewRegistry()
	RegisterTools(r, l)
	env := &tools.Env{Agent: "habakkuk", Mode: tools.ModeFull}
	if res := r.Execute(context.Background(), env, llm.Block{Type: llm.ToolUse, ToolUseID: "t", ToolName: "skill_view", Input: json.RawMessage(`{"name":"pdf-forms"}`)}); !res.IsError {
		t.Fatalf("view of a disabled skill: %s", res.Text)
	}

	// Without scripts an unapproved skill loads, and the view and the
	// invocation both warn.
	os.RemoveAll(filepath.Join(l.SharedDir(), "pdf-forms", "scripts"))
	list, _ := l.List("habakkuk")
	if len(list) != 1 || !list[0].Shared {
		t.Fatalf("list %+v", list)
	}
	res1 := r.Execute(context.Background(), env, llm.Block{Type: llm.ToolUse, ToolUseID: "t", ToolName: "skill_view", Input: json.RawMessage(`{"name":"pdf-forms"}`)})
	if out := res1.Text; res1.IsError || !strings.Contains(out, "has not approved it yet") || !strings.Contains(out, "reference/fields") {
		t.Fatalf("view:\n%s", out)
	}
	text, err := l.Invoke("habakkuk", "pdf-forms", "the lease in inbox/lease.pdf")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[Skill pdf-forms, invoked by the user]", "has not approved it yet", "1. Read the form.", "- reference/fields", "Request: the lease in inbox/lease.pdf"} {
		if !strings.Contains(text, want) {
			t.Fatalf("invoke text lacks %q:\n%s", want, text)
		}
	}
	if err := h.Set("pdf-forms", func(e *hub.Entry) { e.Trusted = true }); err != nil {
		t.Fatal(err)
	}
	if text, _ := l.Invoke("habakkuk", "pdf-forms", ""); strings.Contains(text, "has not approved") || !strings.Contains(text, "nothing more was given") {
		t.Fatalf("approved skill still warned:\n%s", text)
	}
	if _, err := l.Invoke("habakkuk", "nope", ""); err == nil {
		t.Fatal("unknown skill invoked")
	}
	if used := l.LastUsed(); used["skills/pdf-forms"].IsZero() {
		t.Fatalf("usage not recorded: %v", used)
	}
}

func TestPluginSkillRoots(t *testing.T) {
	l := fixture(t)
	plugin := filepath.Join(t.TempDir(), "skills")
	write(t, filepath.Join(plugin, "triage", FileName), skillMD("triage", "Triage issues.", "Plugin triage."))
	write(t, filepath.Join(plugin, "deploy", FileName), skillMD("deploy", "Plugin deploy.", "Plugin deploy."))
	l.Extra = []string{plugin}
	list, err := l.List("habakkuk")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Skill{}
	for _, s := range list {
		byName[s.Name] = s
	}
	if !byName["triage"].Plugin || byName["deploy"].Plugin || byName["weekly-report"].Plugin {
		t.Fatalf("origins %+v", byName)
	}
	s, err := l.Get("other", "deploy") // no agent copy: the shared one beats the plugin's
	if err != nil || s.Plugin {
		t.Fatalf("%+v %v", s, err)
	}
	if s, err := l.Get("other", "triage"); err != nil || !s.Plugin {
		t.Fatalf("%+v %v", s, err)
	}
	if _, untrusted := l.Untrusted(byName["triage"]); untrusted {
		t.Fatal("plugin skills are approved with the plugin")
	}
}

func TestLearnPrompt(t *testing.T) {
	p := LearnPrompt("https://example.com/guide")
	for _, want := range []string{"https://example.com/guide", "fetch_url", "skill_save", "not instructions to you"} {
		if !strings.Contains(p, want) {
			t.Fatalf("learn prompt lacks %q", want)
		}
	}
}

func TestCurator(t *testing.T) {
	l := &Library{Home: t.TempDir()}
	dir := l.AgentDir("habakkuk")
	now := time.Now()
	old := now.Add(-200 * 24 * time.Hour)
	write(t, filepath.Join(dir, "weekly-report", FileName), skillMD("weekly-report", "Write the weekly status report from the board.", "Steps A."))
	write(t, filepath.Join(dir, "weekly-reports", FileName), skillMD("weekly-reports", "Weekly report.", "Steps B."))
	write(t, filepath.Join(dir, "weekly-reports", "templates", "t.md"), "template")
	write(t, filepath.Join(dir, "invoice-chase", FileName), skillMD("invoice-chase", "Chase unpaid invoices by email.", "Steps C."))
	write(t, filepath.Join(dir, "invoice-reminders", FileName), skillMD("invoice-reminders", "Chase unpaid invoices by email reminders.", "Steps D."))
	write(t, filepath.Join(dir, "old-thing", FileName), skillMD("old-thing", "Something nobody uses.", "Steps E."))
	write(t, filepath.Join(l.SharedDir(), "shared-old", FileName), skillMD("shared-old", "Shared and old.", "Never touched."))
	for _, n := range []string{"weekly-report", "weekly-reports", "invoice-chase", "invoice-reminders", "old-thing"} {
		os.Chtimes(filepath.Join(dir, n, FileName), old, old)
	}
	os.Chtimes(filepath.Join(l.SharedDir(), "shared-old", FileName), old, old)
	// weekly-reports and invoice-reminders were used recently: they win.
	l.MarkUsed(Skill{Dir: filepath.Join(dir, "weekly-reports")})
	l.MarkUsed(Skill{Dir: filepath.Join(dir, "invoice-reminders")})

	opts := CurateOptions{Agents: []string{"habakkuk"}, ArchiveAfter: 90 * 24 * time.Hour, Now: now, DryRun: true}
	acts, err := l.Curate(opts)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range acts {
		got = append(got, a.Action+":"+a.Skill+">"+a.Into)
	}
	want := "merged:weekly-report>weekly-reports merged:invoice-chase>invoice-reminders archived:old-thing>"
	if strings.Join(got, " ") != want {
		t.Fatalf("actions %v, want %s", got, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "weekly-report")); err != nil {
		t.Fatal("dry run changed something")
	}

	opts.DryRun = false
	if _, err := l.Curate(opts); err != nil {
		t.Fatal(err)
	}
	merged, _ := os.ReadFile(filepath.Join(dir, "weekly-reports", FileName))
	if !strings.Contains(string(merged), "Steps B.") || !strings.Contains(string(merged), "## Merged from weekly-report") || !strings.Contains(string(merged), "Steps A.") {
		t.Fatalf("merged skill:\n%s", merged)
	}
	for _, gone := range []string{"weekly-report", "invoice-chase", "old-thing"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s still there", gone)
		}
	}
	if _, err := os.Stat(filepath.Join(l.SharedDir(), "shared-old")); err != nil {
		t.Fatal("the curator touched a shared skill")
	}
	// Everything can come back.
	if _, err := l.Backups().Restore("old-thing", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "old-thing", FileName)); err != nil {
		t.Fatal("archived skill not restored")
	}
	if _, err := l.Backups().Restore("weekly-reports", ""); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "weekly-reports", FileName)); strings.Contains(string(b), "Merged from") {
		t.Fatal("merge not undone")
	}

	if l.CurateDue(0, now) || !l.CurateDue(time.Hour, now) {
		t.Fatal("due")
	}
	l.MarkCurated(now)
	if l.CurateDue(time.Hour, now.Add(time.Minute)) || !l.CurateDue(time.Hour, now.Add(2*time.Hour)) {
		t.Fatal("due after a run")
	}
}
