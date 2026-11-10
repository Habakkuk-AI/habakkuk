package web

import (
	"errors"
	"net/http"
	"slices"
	"strconv"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/config"
)

// Channel display settings: the two things a person can flip from the
// dashboard without editing YAML or restarting. What a chat shows while the
// agent works (progress) and how much an approval request shows
// (approvals_detail). Saving edits config.yaml in place and the gateway
// uses the new values from the next turn.

type channelSettings struct {
	Account         string `json:"account"`
	Type            string `json:"type"`
	Progress        string `json:"progress"`
	ApprovalsDetail string `json:"approvals_detail"`
	// LiveProgress: the platform can edit a message, so progress shows as
	// a line that changes. Without it a long turn shows a typing indicator
	// or one "still working" notice, whatever progress says (off aside).
	LiveProgress bool `json:"live_progress"`
}

func (s *Server) settingsOf(account string) channelSettings {
	d := s.Gateway.Display(account)
	_, edits := s.Gateway.Adapters[account].(channel.Editor)
	return channelSettings{Account: account, Type: s.Gateway.Channels[account].Type,
		Progress: d.Progress, ApprovalsDetail: d.ApprovalsDetail, LiveProgress: edits}
}

// channelSettingsList lists the messaging channels with their display
// settings as they are now, and the values each setting takes.
func (s *Server) channelSettingsList(w http.ResponseWriter, _ *http.Request) {
	out := []channelSettings{}
	for name, ch := range s.Gateway.Channels {
		if ch.MessagingChannel() {
			out = append(out, s.settingsOf(name))
		}
	}
	slices.SortFunc(out, func(a, b channelSettings) int {
		if a.Account < b.Account {
			return -1
		}
		return 1
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"channels":         out,
		"progress":         config.ProgressModes,
		"approvals_detail": config.ApprovalsDetails,
		"writable":         s.Gateway.SaveDisplay != nil,
	})
}

// channelSettingsSave changes one channel's display settings. Either field
// may be left out to keep it.
func (s *Server) channelSettingsSave(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Progress        *string `json:"progress"`
		ApprovalsDetail *string `json:"approvals_detail"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	account := r.PathValue("account")
	if ch := s.Gateway.Channels[account]; ch == nil || !ch.MessagingChannel() {
		httpError(w, http.StatusNotFound, "no channel "+strconv.Quote(account))
		return
	}
	progress, detail := "", ""
	if in.Progress != nil {
		if _, ok := config.NormalizeProgress(*in.Progress); !ok || *in.Progress == "" {
			httpError(w, http.StatusBadRequest, "progress must be plain, detailed or off")
			return
		}
		progress = *in.Progress
	}
	if in.ApprovalsDetail != nil {
		if _, ok := config.NormalizeApprovalsDetail(*in.ApprovalsDetail); !ok || *in.ApprovalsDetail == "" {
			httpError(w, http.StatusBadRequest, "approvals_detail must be summary or full")
			return
		}
		detail = *in.ApprovalsDetail
	}
	if progress == "" && detail == "" {
		httpError(w, http.StatusBadRequest, `give "progress", "approvals_detail" or both`)
		return
	}
	if _, err := s.Gateway.ChangeDisplay(account, progress, detail); err != nil {
		if errors.Is(err, channel.ErrDisplayReadOnly) {
			notAvailable(w, "saving channel settings")
			return
		}
		s.fail(w, "save the channel settings", err)
		return
	}
	writeJSON(w, http.StatusOK, s.settingsOf(account))
}

// liveDisplay puts the display settings in effect now into a rendered
// config tree, which otherwise shows the file as it was when serve started.
func (s *Server) liveDisplay(tree any) {
	root, _ := tree.(map[string]any)
	chans, _ := root["channels"].(map[string]any)
	for name, v := range chans {
		ch, ok := v.(map[string]any)
		if !ok || s.Gateway.Channels[name] == nil {
			continue
		}
		d := s.Gateway.Display(name)
		ch["progress"], ch["approvals_detail"] = d.Progress, d.ApprovalsDetail
	}
}
