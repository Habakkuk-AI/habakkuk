package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/channel/webpush"
)

// Web Push from the dashboard. A device that turned notifications on gets
// one when a tool call waits for approval, when a dashboard chat gets a
// reply while no open dashboard shows that chat, and when a scheduled task
// fails. Each device turns each kind on or off. Messages go to the
// browser's own push service, encrypted for that browser.

// approvalPoll is how often the approval list is checked for new entries.
const approvalPoll = 2 * time.Second

// pushRoutes mounts the push API on the dashboard's mux.
func (s *Server) pushRoutes(api func(string, http.HandlerFunc)) {
	api("GET /api/push", s.pushInfo)
	api("POST /api/push", s.pushSubscribe)
	api("PUT /api/push/{id}", s.pushSettings)
	api("DELETE /api/push/{id}", s.pushRemove)
	api("POST /api/push/{id}/test", s.pushTest)
}

// pushInfo gives the dashboard the server's public key, the kinds and the
// devices registered.
func (s *Server) pushInfo(w http.ResponseWriter, _ *http.Request) {
	if s.Push == nil {
		notAvailable(w, "web push")
		return
	}
	keys, err := s.Push.Keys()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "push keys: "+err.Error())
		return
	}
	devs, err := s.Push.Devices()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"public_key": keys.Public, "kinds": webpush.Kinds, "devices": devs})
}

// pushSubscribe registers the subscription a browser's PushManager made:
// {"subscription": <PushSubscription JSON>, "label": "...", "off": [kinds]}.
func (s *Server) pushSubscribe(w http.ResponseWriter, r *http.Request) {
	if s.Push == nil {
		notAvailable(w, "web push")
		return
	}
	var in struct {
		Subscription struct {
			Endpoint       string          `json:"endpoint"`
			ExpirationTime json.RawMessage `json:"expirationTime"`
			Keys           struct {
				P256dh string `json:"p256dh"`
				Auth   string `json:"auth"`
			} `json:"keys"`
		} `json:"subscription"`
		Label string   `json:"label"`
		Off   []string `json:"off"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	sub := in.Subscription
	id, err := s.Push.Subscribe(sub.Endpoint, sub.Keys.P256dh, sub.Keys.Auth, in.Label, in.Off)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id})
}

// pushSettings turns kinds on or off for one device: {"replies": false}.
func (s *Server) pushSettings(w http.ResponseWriter, r *http.Request) {
	if s.Push == nil {
		notAvailable(w, "web push")
		return
	}
	var in map[string]bool
	if !decodeJSON(w, r, &in) {
		return
	}
	dev, err := s.Push.SetKinds(r.PathValue("id"), in)
	switch {
	case errors.Is(err, os.ErrNotExist):
		httpError(w, http.StatusNotFound, "no such device; turn notifications on again")
	case err != nil:
		httpError(w, http.StatusBadRequest, err.Error())
	default:
		writeJSON(w, http.StatusOK, dev)
	}
}

func (s *Server) pushRemove(w http.ResponseWriter, r *http.Request) {
	if s.Push == nil {
		notAvailable(w, "web push")
		return
	}
	if err := s.Push.Remove(r.PathValue("id")); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// pushTest sends one notification to one device now.
func (s *Server) pushTest(w http.ResponseWriter, r *http.Request) {
	if s.Push == nil {
		notAvailable(w, "web push")
		return
	}
	err := s.Push.Test(r.Context(), r.PathValue("id"), webpush.Message{Title: "Habakkuk",
		Body: "Notifications work on this device.", URL: "#/config", Tag: "test"})
	switch {
	case errors.Is(err, os.ErrNotExist):
		httpError(w, http.StatusNotFound, "no such device; turn notifications on again")
	case errors.Is(err, webpush.ErrGone):
		httpError(w, http.StatusGone, "the browser no longer accepts notifications for this device; turn them on again")
	case err != nil:
		httpError(w, http.StatusBadGateway, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]bool{"sent": true})
	}
}

// notify sends in the background: a slow push service never holds up a
// reply or the scheduler.
func (s *Server) notify(kind string, m webpush.Message) {
	if s.Push == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		s.Push.Notify(ctx, kind, m)
	}()
}

// replyPushed notifies about a reply in a web conversation that no open
// dashboard is showing.
func (s *Server) replyPushed(ctx context.Context, to channel.Address, text string) {
	if s.Push == nil || s.hub.watched(to.Conversation) {
		return
	}
	title := "New reply"
	if s.Gateway != nil && s.Gateway.Store != nil {
		if c, err := s.Gateway.Store.Conversation(ctx, to); err == nil && c != nil && c.Agent != "" {
			title = c.Agent + " replied"
		}
	}
	s.notify(webpush.KindReplies, webpush.Message{Title: title, Body: plainPreview(text, 180),
		URL: "#/chat/" + to.Conversation, Tag: "reply-" + to.Conversation})
}

// ScheduleFailed notifies that a scheduled task failed. The scheduler
// calls it once per failed run (once per failure streak for scripts).
func (s *Server) ScheduleFailed(id, name, status string) {
	if name == "" {
		name = id
	}
	s.notify(webpush.KindSchedules, webpush.Message{Title: "Scheduled task failed",
		Body: plainPreview(fmt.Sprintf("%s: %s", name, status), 180), URL: "#/schedules", Tag: "schedule-" + id})
}

// watchApprovals notifies once for each approval that starts waiting.
func (s *Server) watchApprovals(ctx context.Context) {
	if s.Push == nil || s.Gateway == nil {
		return
	}
	seen := map[string]bool{}
	t := time.NewTicker(approvalPoll)
	defer t.Stop()
	for {
		now := map[string]bool{}
		for _, a := range s.Gateway.Approvals() {
			now[a.ID] = true
			if seen[a.ID] {
				continue
			}
			body := a.Summary
			if body == "" {
				body = a.Tool
			}
			s.notify(webpush.KindApprovals, webpush.Message{Title: "Approval needed", Body: plainPreview(body, 180),
				URL: "#/approvals", Tag: "approval-" + a.ID})
		}
		seen = now
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// plainPreview is a short one-line version of Markdown text for a
// notification.
func plainPreview(text string, n int) string {
	text = strings.NewReplacer("**", "", "__", "", "`", "", "#", "", "> ", "").Replace(text)
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) <= n {
		return text
	}
	r := []rune(text)
	return strings.TrimSpace(string(r[:n])) + "..."
}

// watched reports whether any event stream is open on a conversation.
func (h *hub) watched(conv string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[conv]) > 0
}
