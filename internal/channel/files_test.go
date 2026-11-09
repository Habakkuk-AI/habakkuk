package channel

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

func TestSendFileReachesTheChat(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7"}})
	ws := r.g.Runner.Agents["habakkuk"].Workspace
	if err := os.WriteFile(filepath.Join(ws, "report.txt"), []byte("quarterly numbers"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.g.Handle(context.Background(), dm("m1", "send file report.txt"))
	waitFor(t, "reply", hasText(r.a, "tool said: Queued report.txt"))
	waitFor(t, "file", func() bool { return len(r.a.sentFiles()) == 1 })
	f := r.a.sentFiles()[0]
	if f.Name != "report.txt" || f.MediaType != "text/plain" || f.Data != "quarterly numbers" || f.Caption != "here it is" {
		t.Fatalf("file: %+v", f)
	}
	if f.To.Conversation != "100" || f.To.ReplyTo != "" {
		t.Fatalf("file addressed wrong: %+v", f.To)
	}
}

type approveAll bool

func (a approveAll) Approve(context.Context, tools.ApprovalRequest) (bool, error) {
	return bool(a), nil
}

func runSendFile(t *testing.T, r *rig, ctx context.Context, env *tools.Env, path string) (string, error) {
	t.Helper()
	input, _ := json.Marshal(map[string]string{"path": path})
	return sendFileTool{r.g}.Run(ctx, env, input)
}

func TestSendFilePolicy(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7"}})
	ws := r.g.Runner.Agents["habakkuk"].Workspace
	outside := filepath.Join(filepath.Dir(ws), "secret.txt")
	os.WriteFile(outside, []byte("secret"), 0o644)
	os.WriteFile(filepath.Join(ws, "ok.txt"), []byte("fine"), 0o644)
	os.WriteFile(filepath.Join(ws, "empty.txt"), nil, 0o644)
	// Windows without symlink rights cannot make the link; that case is
	// then left out.
	linked := os.Symlink(outside, filepath.Join(ws, "link.txt")) == nil
	notFound := "no such file"
	if runtime.GOOS == "windows" {
		notFound = "cannot find"
	}
	os.Mkdir(filepath.Join(ws, "dir"), 0o755)
	ctx := context.WithValue(context.Background(), originKey{}, Address{Account: "tg", Conversation: "100", ReplyTo: "m1"})
	env := func(mode tools.Mode, ap tools.Approver) *tools.Env {
		return &tools.Env{Session: "s1", Workspace: ws, Mode: mode, Approver: ap}
	}
	cases := []struct {
		name, path string
		env        *tools.Env
		want       string // error substring; "" means it is queued
	}{
		{"inside", "ok.txt", env(tools.ModeWorkspace, nil), ""},
		{"outside unattended", outside, env(tools.ModeWorkspace, nil), "outside the workspace"},
		{"symlink out", "link.txt", env(tools.ModeWorkspace, nil), "outside the workspace"},
		{"outside approved", outside, env(tools.ModeWorkspace, approveAll(true)), ""},
		{"outside declined", outside, env(tools.ModeWorkspace, approveAll(false)), "declined"},
		{"outside full", outside, env(tools.ModeFull, nil), ""},
		{"missing", "nope.txt", env(tools.ModeWorkspace, nil), notFound},
		{"directory", "dir", env(tools.ModeWorkspace, nil), "is a directory"},
		{"empty", "empty.txt", env(tools.ModeWorkspace, nil), "is empty"},
	}
	for i, c := range cases {
		if c.name == "symlink out" && !linked {
			continue
		}
		out, err := runSendFile(t, r, tools.WithCallID(ctx, "call-"+c.name), c.env, c.path)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%d %s: %v", i, c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%d %s: got %q, %v; want error with %q", i, c.name, out, err, c.want)
		}
	}
	if _, err := runSendFile(t, r, ctx, env(tools.ModeWorkspace, nil), outside); !errors.Is(err, tools.ErrDenied) {
		t.Fatalf("outside the workspace should be a policy denial: %v", err)
	}

	r.a.maxFile = 3
	if _, err := runSendFile(t, r, ctx, env(tools.ModeWorkspace, nil), "ok.txt"); err == nil || !strings.Contains(err.Error(), "takes files up to 3 bytes") {
		t.Fatalf("size limit: %v", err)
	}
}

func TestSendFileNeedsAChat(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7"}})
	ws := r.g.Runner.Agents["habakkuk"].Workspace
	os.WriteFile(filepath.Join(ws, "ok.txt"), []byte("fine"), 0o644)
	_, err := runSendFile(t, r, context.Background(), &tools.Env{Session: "cli-session", Workspace: ws, Mode: tools.ModeFull}, "ok.txt")
	if !errors.Is(err, errNoChat) {
		t.Fatalf("err = %v", err)
	}
}

func TestSendFileOncePerCall(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7"}})
	ws := r.g.Runner.Agents["habakkuk"].Workspace
	os.WriteFile(filepath.Join(ws, "ok.txt"), []byte("fine"), 0o644)
	ctx := context.WithValue(context.Background(), originKey{}, Address{Account: "tg", Conversation: "100"})
	env := &tools.Env{Session: "s1", Workspace: ws, Mode: tools.ModeWorkspace}
	for range 2 { // the same call run twice, as after a worker restart
		if _, err := runSendFile(t, r, tools.WithCallID(ctx, "call-1"), env, "ok.txt"); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "file", func() bool { return len(r.a.sentFiles()) == 1 })
	if _, err := runSendFile(t, r, tools.WithCallID(ctx, "call-2"), env, "ok.txt"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "second file", func() bool { return len(r.a.sentFiles()) == 2 })
	time.Sleep(100 * time.Millisecond)
	if n := len(r.a.sentFiles()); n != 2 {
		t.Fatalf("sent %d files, want 2", n)
	}
}

func TestStillWorkingNoticeWithoutTyping(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7"}})
	r.g.StillWorking = 30 * time.Millisecond
	r.model.delay = 200 * time.Millisecond
	r.g.Handle(context.Background(), dm("m1", "slow one"))
	waitFor(t, "reply", hasText(r.a, "echo: slow one"))
	texts := r.a.texts()
	if len(texts) != 2 || texts[0] != stillWorkingText {
		t.Fatalf("want one progress notice before the reply, got %q", texts)
	}
}

type typingRecorder struct {
	*recorder
	typed atomic.Int32
}

func (t *typingRecorder) Typing(context.Context, Address) error { t.typed.Add(1); return nil }

func TestTypingCoversTheTurn(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7"}})
	tr := &typingRecorder{recorder: r.a}
	r.g.Adapters["tg"] = tr
	r.g.StillWorking = 10 * time.Millisecond
	r.model.delay = 150 * time.Millisecond
	r.g.Handle(context.Background(), dm("m1", "slow one"))
	waitFor(t, "reply", hasText(r.a, "echo: slow one"))
	if tr.typed.Load() == 0 {
		t.Fatal("no typing indicator during the turn")
	}
	if hasText(r.a, stillWorkingText)() {
		t.Fatal("a platform with a typing indicator got a progress notice")
	}
}
