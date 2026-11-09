package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// Live mode: the dashboard talks to a realtime speech model directly over
// WebRTC. Habakkuk never carries the audio. It mints a short-lived client
// secret (POST /api/realtime/session) whose session config gives the voice
// model one function tool, ask_habakkuk. The page forwards each call to
// POST /api/realtime/tool, which runs a normal turn of the conversation's
// agent, with all its tools, approvals and memory, and returns the reply for
// the voice model to speak. What both sides say is written down by the
// provider and posted to POST /api/realtime/transcript: each line goes to
// the chat's log at once, and the lines the agent has not seen yet go into
// its session, inside the next ask_habakkuk turn or, when the call ends
// (POST /api/realtime/end), as a note of their own.

const (
	realtimeTool     = "ask_habakkuk"
	maxRequest       = 8000             // characters of one ask_habakkuk request
	maxLiveLine      = 4000             // characters of one transcript line
	toolWait         = 4 * time.Minute  // longest an ask_habakkuk call waits for its turn
	noteWait         = 10 * time.Minute // longest the end of a call waits to write its note
	approvalPollTick = 300 * time.Millisecond
)

// realtimeState is what Live mode keeps between requests, per conversation:
// transcript lines the agent has not seen and the turn an ask_habakkuk call
// left running (to pick up after an approval or a timeout).
type realtimeState struct {
	mu      sync.Mutex
	pending map[string][]liveLine
	turns   map[string]string
}

type liveLine struct {
	Role string `json:"role"` // user | assistant
	Text string `json:"text"`
}

func (rs *realtimeState) add(conv string, l liveLine) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.pending == nil {
		rs.pending = map[string][]liveLine{}
	}
	rs.pending[conv] = append(rs.pending[conv], l)
	if n := len(rs.pending[conv]); n > 200 { // a very long call: keep the latest
		rs.pending[conv] = slices.Clone(rs.pending[conv][n-200:])
	}
}

func (rs *realtimeState) take(conv string) []liveLine {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	l := rs.pending[conv]
	delete(rs.pending, conv)
	return l
}

// restore puts lines back in front of any that arrived since they were
// taken, when the agent did not get them after all.
func (rs *realtimeState) restore(conv string, lines []liveLine) {
	if len(lines) == 0 {
		return
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.pending == nil {
		rs.pending = map[string][]liveLine{}
	}
	rs.pending[conv] = append(slices.Clone(lines), rs.pending[conv]...)
}

func (rs *realtimeState) setTurn(conv, job string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.turns == nil {
		rs.turns = map[string]string{}
	}
	if job == "" {
		delete(rs.turns, conv)
	} else {
		rs.turns[conv] = job
	}
}

func (rs *realtimeState) turn(conv string) string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.turns[conv]
}

// realtimeConfig returns Live mode's config, or nil when it is off.
func (s *Server) realtimeConfig() *config.Realtime {
	if s.Dash == nil || s.Dash.Config == nil {
		return nil
	}
	return s.Dash.Config.Realtime
}

// uiPolicy is the dashboard's Content-Security-Policy. With Live mode on,
// the page may also post its WebRTC offer to the provider.
func (s *Server) uiPolicy() string {
	rt := s.realtimeConfig()
	if rt == nil {
		return uiCSP
	}
	u, err := url.Parse(rt.BaseURL)
	if err != nil || u.Host == "" {
		return uiCSP
	}
	return strings.Replace(uiCSP, "connect-src 'self'", "connect-src 'self' "+u.Scheme+"://"+u.Host, 1)
}

// voiceInstructions are the voice model's standing instructions.
const voiceInstructions = `You are the voice of Habakkuk, the user's personal AI agent. You speak with the user in real time; Habakkuk does the work.
- Keep turns short and natural, like a phone call. No lists, Markdown or URLs read out in full.
- For anything that needs facts you are not sure of, the user's files, memory, calendar, email, the web, code, schedules, messages or any action, call ask_habakkuk with one complete, self-contained request in plain words (include names, dates and details the user gave). Say a few words such as "One moment" before calling it.
- ask_habakkuk returns Habakkuk's answer as text. Tell the user the gist in one to three spoken sentences; offer more detail if it is long.
- If ask_habakkuk says Habakkuk needs approval, explain what Habakkuk wants to do and ask the user. When they answer, call ask_habakkuk with exactly "yes" or "no".
- If ask_habakkuk says Habakkuk is still working, tell the user the answer will appear in the chat, and carry on.
- Never invent results Habakkuk did not return.`

// realtimeToolSpec is the one function the voice model can call.
func realtimeToolSpec() map[string]any {
	return map[string]any{
		"type": "function",
		"name": realtimeTool,
		"description": "Hand a request to Habakkuk, the user's agent, which has their memory, files and tools, and return its answer. " +
			"Use it for anything beyond small talk.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"request": map[string]any{"type": "string", "description": "What the user wants, complete and self-contained."},
			},
			"required":             []string{"request"},
			"additionalProperties": false,
		},
	}
}

type realtimeSessionRequest struct {
	Conversation string `json:"conversation"`
	Agent        string `json:"agent"`
}

// realtimeSession mints a client secret for one Live call. The secret is
// short-lived and only good for starting a session with this config, so the
// page never sees the API key.
func (s *Server) realtimeSession(w http.ResponseWriter, r *http.Request) {
	rt := s.realtimeConfig()
	if rt == nil {
		notAvailable(w, "Live mode (realtime in config.yaml)")
		return
	}
	var req realtimeSessionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !conversationID.MatchString(req.Conversation) {
		httpError(w, http.StatusBadRequest, "conversation must be 1-128 letters, digits, '.', '_', ':' or '-'")
		return
	}
	if _, ok := s.Gateway.Runner.Agents[req.Agent]; req.Agent != "" && !ok {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("unknown agent %q", req.Agent))
		return
	}
	key := ""
	if s.Dash.Secret != nil {
		key = s.Dash.Secret(rt.KeyEnv)
	}
	if key == "" {
		httpError(w, http.StatusServiceUnavailable, "realtime: "+rt.KeyEnv+" is not set")
		return
	}
	instructions := voiceInstructions
	if rt.Instructions != "" {
		instructions += "\n\n" + rt.Instructions
	}
	body := map[string]any{
		"expires_after": map[string]any{"anchor": "created_at", "seconds": int(rt.TTL / time.Second)},
		"session": map[string]any{
			"type":         "realtime",
			"model":        rt.Model,
			"instructions": instructions,
			"audio": map[string]any{
				"input":  map[string]any{"transcription": map[string]any{"model": rt.Transcription}},
				"output": map[string]any{"voice": rt.Voice},
			},
			"tools":       []any{realtimeToolSpec()},
			"tool_choice": "auto",
		},
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	secret, err := mintClientSecret(ctx, rt.BaseURL, key, body)
	if err != nil {
		s.Log.Warn("realtime session failed", "err", err)
		httpError(w, http.StatusBadGateway, "realtime: "+shortText(err.Error(), 300))
		return
	}
	base := strings.TrimRight(rt.BaseURL, "/")
	writeJSON(w, http.StatusOK, map[string]any{
		"client_secret": secret.Value,
		"expires_at":    secret.ExpiresAt,
		"calls_url":     base + "/realtime/calls",
		"model":         rt.Model,
		"voice":         rt.Voice,
		"tool":          realtimeTool,
	})
}

type clientSecret struct {
	Value     string `json:"value"`
	ExpiresAt int64  `json:"expires_at"`
}

// mintClientSecret calls POST {base}/realtime/client_secrets.
func mintClientSecret(ctx context.Context, base, key string, body any) (clientSecret, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return clientSecret{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/realtime/client_secrets", bytes.NewReader(data))
	if err != nil {
		return clientSecret{}, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return clientSecret{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return clientSecret{}, err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		msg := strings.TrimSpace(string(raw[:min(len(raw), 300)]))
		if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
			msg = e.Error.Message
		}
		return clientSecret{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
	}
	var out clientSecret
	if err := json.Unmarshal(raw, &out); err != nil {
		return clientSecret{}, fmt.Errorf("unreadable response: %w", err)
	}
	if out.Value == "" {
		return clientSecret{}, errors.New("the response has no client secret")
	}
	return out, nil
}

type realtimeToolRequest struct {
	Conversation string `json:"conversation"`
	Agent        string `json:"agent"`
	CallID       string `json:"call_id"`
	Request      string `json:"request"`
}

// realtimeToolCall runs an ask_habakkuk call as a turn of the conversation
// and answers {"output": text} for the voice model. A call that hits an
// approval returns at once, asking the voice model to ask the user; the
// answer ("yes" or "no") comes back as another call and decides it.
func (s *Server) realtimeToolCall(w http.ResponseWriter, r *http.Request) {
	var req realtimeToolRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Request = strings.TrimSpace(req.Request)
	switch {
	case !conversationID.MatchString(req.Conversation):
		httpError(w, http.StatusBadRequest, "conversation must be 1-128 letters, digits, '.', '_', ':' or '-'")
		return
	case req.Request == "":
		httpError(w, http.StatusBadRequest, "request is empty")
		return
	case utf8.RuneCountInString(req.Request) > maxRequest:
		httpError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("request is longer than %d characters", maxRequest))
		return
	case len(req.CallID) > 100:
		httpError(w, http.StatusBadRequest, "call_id is longer than 100 characters")
		return
	}
	ctx := r.Context()
	conv := req.Conversation
	if a, ok := s.approvalFor(conv); ok {
		decision, answered := approvalWord(req.Request)
		if !answered {
			writeJSON(w, http.StatusOK, map[string]string{"output": approvalOutput(a)})
			return
		}
		s.Gateway.Decide(a.ID, decision)
		job := s.rt.turn(conv)
		if job == "" {
			writeJSON(w, http.StatusOK, map[string]string{"output": map[bool]string{true: "Approved.", false: "Denied."}[decision]})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"output": s.awaitTool(ctx, conv, job)})
		return
	}
	text := "[Request from the voice conversation]\n" + req.Request
	lines := s.rt.take(conv)
	if len(lines) > 0 {
		text = renderLive(lines) + "\n\n" + text
	}
	id := ""
	if req.CallID != "" {
		id = "rt-" + req.CallID // a resent call attaches to the first one's turn
	}
	out, status, msg := s.deliverChat(ctx, chatRequest{Conversation: conv, Text: text, Agent: req.Agent, ID: id},
		"(voice) "+req.Request, nil)
	if msg != "" {
		s.rt.restore(conv, lines)
		httpError(w, status, msg)
		return
	}
	job := out["turn"]
	if job == "" {
		writeJSON(w, http.StatusOK, map[string]string{"output": "Done."})
		return
	}
	s.rt.setTurn(conv, job)
	writeJSON(w, http.StatusOK, map[string]string{"output": s.awaitTool(ctx, conv, job)})
}

// awaitTool waits for a turn's reply, returning early when the turn asks
// for an approval or runs past toolWait; the turn keeps running either way.
func (s *Server) awaitTool(ctx context.Context, conv, job string) string {
	type result struct {
		text string
		err  error
	}
	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan result, 1)
	go func() {
		text, err := s.Gateway.Runner.Await(actx, job, nil, nil)
		done <- result{text, err}
	}()
	tick := time.NewTicker(approvalPollTick)
	defer tick.Stop()
	limit := time.NewTimer(toolWait)
	defer limit.Stop()
	for {
		select {
		case res := <-done:
			if res.err != nil {
				if ctx.Err() != nil {
					return ""
				}
				s.rt.setTurn(conv, "")
				return "Habakkuk could not finish that: " + shortText(res.err.Error(), 300)
			}
			s.rt.setTurn(conv, "")
			if strings.TrimSpace(res.text) == "" || strings.TrimSpace(res.text) == channel.Silent {
				return "Done."
			}
			return res.text
		case <-tick.C:
			if a, ok := s.approvalFor(conv); ok {
				return approvalOutput(a)
			}
		case <-limit.C:
			return "Habakkuk is still working on this; the answer will appear in the chat when it is ready."
		case <-ctx.Done():
			return ""
		}
	}
}

func (s *Server) approvalFor(conv string) (channel.Approval, bool) {
	for _, a := range s.Gateway.Approvals() {
		if a.To.Account == s.Account && a.To.Conversation == conv {
			return a, true
		}
	}
	return channel.Approval{}, false
}

func approvalOutput(a channel.Approval) string {
	out := fmt.Sprintf("Habakkuk needs approval before it continues: it wants to use %s: %s.", a.Tool, shortText(a.Summary, 300))
	if a.Review != "" {
		out += " Why it asks: " + shortText(a.Review, 200) + "."
	}
	return out + ` Ask the user, then call ask_habakkuk with exactly "yes" or "no".`
}

// approvalWord reads a spoken answer to an approval.
func approvalWord(text string) (ok, answered bool) {
	w := strings.ToLower(strings.Trim(strings.TrimSpace(text), ".!?\"'"))
	switch w {
	case "yes", "y", "yeah", "yep", "ok", "okay", "approve", "approved", "allow", "go ahead", "do it":
		return true, true
	case "no", "n", "nope", "deny", "denied", "don't", "dont", "cancel", "stop":
		return false, true
	}
	return false, false
}

// renderLive turns transcript lines into a note for the agent.
func renderLive(lines []liveLine) string {
	var b strings.Builder
	b.WriteString("[Live voice conversation, transcribed]")
	for _, l := range lines {
		who := "User"
		if l.Role == "assistant" {
			who = "Voice assistant"
		}
		b.WriteString("\n" + who + ": " + l.Text)
	}
	return b.String()
}

type realtimeTranscriptRequest struct {
	Conversation string     `json:"conversation"`
	Lines        []liveLine `json:"lines"`
}

// realtimeTranscript records finished transcript lines in the chat's log
// and keeps them for the agent's session.
func (s *Server) realtimeTranscript(w http.ResponseWriter, r *http.Request) {
	var req realtimeTranscriptRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !conversationID.MatchString(req.Conversation) {
		httpError(w, http.StatusBadRequest, "bad conversation id")
		return
	}
	if len(req.Lines) > 50 {
		httpError(w, http.StatusBadRequest, "at most 50 lines at a time")
		return
	}
	ctx := context.WithoutCancel(r.Context())
	n := 0
	for _, l := range req.Lines {
		l.Text = strings.TrimSpace(l.Text)
		if l.Role != "user" && l.Role != "assistant" {
			httpError(w, http.StatusBadRequest, "role must be user or assistant")
			return
		}
		if l.Text == "" {
			continue
		}
		l.Text = shortText(l.Text, maxLiveLine)
		shown := "(voice) " + l.Text
		if _, err := s.Store.Append(ctx, req.Conversation, l.Role, "", shown); err != nil {
			s.fail(w, "record the transcript", err)
			return
		}
		s.rt.add(req.Conversation, l)
		n++
	}
	if n > 0 {
		s.hub.notify(req.Conversation, false)
	}
	writeJSON(w, http.StatusOK, map[string]int{"recorded": n})
}

type realtimeEndRequest struct {
	Conversation string `json:"conversation"`
	Agent        string `json:"agent"`
}

// realtimeEnd writes the transcript lines the agent has not seen into the
// conversation's session, so the next turn (typed or spoken) knows what was
// said. While a turn runs it waits in the background for the session to
// be idle; the transcript is append-only and a turn owns the next slot.
func (s *Server) realtimeEnd(w http.ResponseWriter, r *http.Request) {
	var req realtimeEndRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !conversationID.MatchString(req.Conversation) {
		httpError(w, http.StatusBadRequest, "bad conversation id")
		return
	}
	if _, ok := s.Gateway.Runner.Agents[req.Agent]; req.Agent != "" && !ok {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("unknown agent %q", req.Agent))
		return
	}
	lines := s.rt.take(req.Conversation)
	if len(lines) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"noted": 0})
		return
	}
	ctx := context.WithoutCancel(r.Context())
	sessionID, err := s.liveSession(ctx, req.Conversation, req.Agent)
	if err != nil {
		s.rt.restore(req.Conversation, lines)
		s.fail(w, "open the conversation's session", err)
		return
	}
	if err := s.noteLive(ctx, sessionID, lines); err != nil {
		if !errors.Is(err, errSessionBusy) {
			s.rt.restore(req.Conversation, lines)
			s.fail(w, "record the transcript", err)
			return
		}
		go func() {
			deadline := time.Now().Add(noteWait)
			for time.Now().Before(deadline) {
				time.Sleep(time.Second)
				if err := s.noteLive(ctx, sessionID, lines); !errors.Is(err, errSessionBusy) {
					if err != nil {
						s.Log.Warn("record live transcript failed", "err", err)
					}
					return
				}
			}
			s.Log.Warn("live transcript dropped: the session stayed busy", "session", sessionID)
		}()
		writeJSON(w, http.StatusAccepted, map[string]any{"noted": len(lines), "session": sessionID})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"noted": len(lines), "session": sessionID})
}

// liveSession returns the conversation's session, starting one bound to it
// when the conversation has none yet (a call that never asked Habakkuk).
func (s *Server) liveSession(ctx context.Context, conv, agentName string) (string, error) {
	g := s.Gateway
	to := channel.Address{Account: s.Account, Conversation: conv}
	c, err := g.Store.Conversation(ctx, to)
	if err != nil {
		return "", err
	}
	if c != nil && c.SessionID != "" {
		return c.SessionID, nil
	}
	if agentName == "" {
		if ch := g.Channels[s.Account]; ch != nil {
			agentName = ch.Agent
		}
	}
	sess, err := g.Runner.NewSession(ctx, agentName)
	if err != nil {
		return "", err
	}
	if err := g.Store.Bind(ctx, to, agentName, sess.ID); err != nil {
		return "", err
	}
	return sess.ID, nil
}

var errSessionBusy = errors.New("the session has a turn in progress")

// noteLive appends the transcript to an idle session as a user note and a
// short acknowledgement, keeping the transcript's turns alternating.
func (s *Server) noteLive(ctx context.Context, sessionID string, lines []liveLine) error {
	runner := s.Gateway.Runner
	busy, err := runner.Queue.Busy(ctx, sessionID, "")
	if err != nil {
		return err
	}
	if busy {
		return errSessionBusy
	}
	recs, err := runner.Sessions.Messages(ctx, sessionID)
	if err != nil {
		return err
	}
	seq := len(recs)
	var msgs []llm.Message
	if seq > 0 && recs[seq-1].Message.Role == llm.User { // an earlier turn ended without a reply
		msgs = append(msgs, llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: "(no reply)"}}})
	}
	msgs = append(msgs, llm.UserText(renderLive(lines)),
		llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: "(noted)"}}})
	for i, m := range msgs {
		if err := runner.Sessions.Append(ctx, sessionID, "", seq+i, m, "", nil); err != nil {
			return err
		}
	}
	return nil
}
