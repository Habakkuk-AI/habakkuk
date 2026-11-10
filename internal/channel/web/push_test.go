package web

import (
	"context"
	"crypto/ecdh"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/channel/webpush"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// pushService is a fake browser push service that counts what it gets.
type pushService struct {
	mu      sync.Mutex
	urgency []string
	srv     *httptest.Server
}

func (p *pushService) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.urgency)
}

func newPushRig(t *testing.T) (*dashRig, *pushService, string) {
	t.Helper()
	r := newDashRig(t)
	ps := &pushService{}
	ps.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ps.mu.Lock()
		ps.urgency = append(ps.urgency, req.Header.Get("Urgency"))
		ps.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(ps.srv.Close)
	dir := filepath.Join(r.home, "push")
	r.s.Push = &webpush.Service{Dir: dir, Client: dialOnly(ps.srv), Log: quiet, Hosts: []string{"push.example.com"}}

	info := r.get(t, "/api/push")
	if info["public_key"] == "" || len(list(info, "devices")) != 0 || len(list(info, "kinds")) != 3 {
		t.Fatalf("push info: %v", info)
	}
	priv, _ := ecdh.P256().GenerateKey(nil)
	enc := base64.RawURLEncoding.EncodeToString
	body, _ := json.Marshal(map[string]any{
		"subscription": map[string]any{"endpoint": "https://push.example.com/send/abc", "expirationTime": nil,
			"keys": map[string]string{"p256dh": enc(priv.PublicKey().Bytes()), "auth": enc([]byte("0123456789abcdef"))}},
		"label": "Phone",
	})
	out := r.send(t, "POST", "/api/push", string(body), http.StatusOK)
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("no id: %v", out)
	}
	return r, ps, id
}

func TestPushAPI(t *testing.T) {
	r, ps, id := newPushRig(t)
	devs := list(r.get(t, "/api/push"), "devices")
	if len(devs) != 1 || devs[0].(map[string]any)["label"] != "Phone" {
		t.Fatalf("devices %v", devs)
	}
	r.send(t, "POST", "/api/push/"+id+"/test", ``, http.StatusOK)
	waitFor(t, "test push", func() bool { return ps.count() == 1 })

	dev := r.send(t, "PUT", "/api/push/"+id, `{"schedules": false}`, http.StatusOK)
	if k := dev["kinds"].(map[string]any); k["schedules"] != false || k["approvals"] != true {
		t.Fatalf("kinds %v", k)
	}
	r.send(t, "PUT", "/api/push/"+id, `{"weather": true}`, http.StatusBadRequest)
	r.send(t, "PUT", "/api/push/dev_nope", `{"replies": true}`, http.StatusNotFound)

	// A schedule failure is off for this device: nothing is sent.
	r.s.ScheduleFailed("sch_1", "morning brief", "failed: model unavailable")
	time.Sleep(100 * time.Millisecond)
	if ps.count() != 1 {
		t.Fatal("sent a kind the device turned off")
	}

	bad, _ := json.Marshal(map[string]any{"subscription": map[string]any{"endpoint": "https://10.1.2.3/x",
		"keys": map[string]string{"p256dh": "x", "auth": "y"}}})
	r.send(t, "POST", "/api/push", string(bad), http.StatusBadRequest)

	if code, _ := r.do(t, "DELETE", "/api/push/"+id, token, "", ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if devs := list(r.get(t, "/api/push"), "devices"); len(devs) != 0 {
		t.Fatalf("still there: %v", devs)
	}
}

func TestPushRepliesOnlyWhenNobodyWatches(t *testing.T) {
	r, ps, _ := newPushRig(t)
	ctx := context.Background()
	to := channel.Address{Account: "web", Conversation: "c1"}
	if err := r.s.Send(ctx, to, "**Done.** The report is in your workspace.", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "reply push", func() bool { return ps.count() == 1 })

	r.open(t, "/api/chat/c1/events")
	waitFor(t, "stream open", func() bool { return r.s.hub.watched("c1") })
	if err := r.s.Send(ctx, to, "And one more thing.", ""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if ps.count() != 1 {
		t.Fatal("pushed a reply the open dashboard already shows")
	}
}

func TestPushApprovals(t *testing.T) {
	r, ps, _ := newPushRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.s.watchApprovals(ctx)
	g := r.s.Gateway
	sess, err := g.Runner.NewSession(ctx, "habakkuk")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Store.Bind(ctx, channel.Address{Account: "web", Conversation: "ap1"}, "habakkuk", sess.ID); err != nil {
		t.Fatal(err)
	}
	// No dashboard shows ap1, yet the prompt itself is not pushed as a
	// reply: only the approval notice goes out.
	go g.ApproverFor(sess.ID).Approve(ctx, tools.ApprovalRequest{Tool: "exec", Summary: "make deploy"})
	waitFor(t, "approval push", func() bool {
		ps.mu.Lock()
		defer ps.mu.Unlock()
		for _, u := range ps.urgency {
			if u == "high" {
				return true
			}
		}
		return false
	})
	n := ps.count()
	time.Sleep(3 * approvalPoll)
	if ps.count() != n || n != 1 {
		t.Fatalf("%d pushes, want the one approval notice (no reply push, no repeat)", ps.count())
	}
	// The lock screen gets the agent and the kind of action, not the command.
	a := g.Approvals()[0]
	m := r.s.approvalNotice(a)
	if strings.Contains(m.Body, "make deploy") || m.Body != "habakkuk is waiting for your yes or no to run a command." {
		t.Fatalf("approval notice %+v", m)
	}
}

func TestPlainPreview(t *testing.T) {
	if got := plainPreview("# Title\n\n**Bold** and `code`", 100); got != "Title Bold and code" {
		t.Fatalf("%q", got)
	}
	if got := plainPreview(strings.Repeat("é", 10), 4); got != "éééé..." {
		t.Fatalf("%q", got)
	}
}
