package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/core/events"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/tools/media"
	"github.com/habakkuk-ai/habakkuk/internal/tools/mediatools"
)

const (
	maxUpload      = 25 << 20 // all files of one chat message together
	maxUploadFiles = 10
	liveInput      = 2000 // characters of a tool call's input sent to the page
	liveOutput     = 4000 // characters of a tool result sent to the page
)

type conversationView struct {
	Conversation string    `json:"conversation"`
	Agent        string    `json:"agent"`
	Session      string    `json:"session"`
	Title        string    `json:"title"`
	LastAt       time.Time `json:"last_at"`
}

// conversations lists the web chat's conversations, newest first, with
// their sessions' titles.
func (s *Server) conversations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	list, err := s.Gateway.Store.Conversations(ctx, s.Account, intParam(r, "limit", 50, 1, 200))
	if err != nil {
		s.fail(w, "list conversations", err)
		return
	}
	out := []conversationView{}
	for _, c := range list {
		v := conversationView{Conversation: c.Conversation, Agent: c.Agent, Session: c.SessionID, LastAt: c.LastAt.UTC()}
		if sess, err := s.Gateway.Runner.Sessions.Get(ctx, c.SessionID); err == nil {
			v.Title = sess.Title
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversations": out})
}

// liveEvent is one piece of a running turn's activity, for tool cards and
// the reply as it streams.
type liveEvent struct {
	Kind    string `json:"kind"`
	Tool    string `json:"tool,omitempty"`
	ID      string `json:"id,omitempty"`
	Input   string `json:"input,omitempty"`
	Text    string `json:"text,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
}

func liveFrom(e events.Event) (liveEvent, bool) {
	v := liveEvent{Kind: string(e.Kind), Tool: e.Tool, ID: e.ToolID, IsError: e.IsError}
	switch e.Kind {
	case events.Text, events.Thinking:
		v.Text = e.Text
	case events.ToolStart:
		v.Input = shortText(string(e.Input), liveInput)
	case events.ToolDone, events.Review, events.Retry, events.TurnError:
		v.Text = shortText(e.Text, liveOutput)
	case events.TurnDone:
	default:
		return v, false
	}
	return v, true
}

// live streams the activity of the conversation's current session from the
// events bus: tool calls as they start and finish, the reply as it is
// written, reviewer decisions and errors. It follows the conversation to a
// new session (/new, another agent) when the next turn starts.
func (s *Server) live(w http.ResponseWriter, r *http.Request) {
	conv := r.PathValue("conversation")
	if !conversationID.MatchString(conv) {
		httpError(w, http.StatusBadRequest, "bad conversation id")
		return
	}
	bus := s.Gateway.Runner.Bus
	if bus == nil {
		notAvailable(w, "live activity")
		return
	}
	ctx := r.Context()
	rc := http.NewResponseController(w)
	wake := s.hub.subscribe(conv)
	defer s.hub.unsubscribe(conv, wake)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "retry: 3000\n\n")

	addr := channel.Address{Account: s.Account, Conversation: conv}
	var (
		session string
		evs     <-chan events.Event
		cancel  = func() {}
	)
	defer func() { cancel() }()
	follow := func() {
		c, err := s.Gateway.Store.Conversation(ctx, addr)
		if err != nil || c == nil || c.SessionID == session {
			return
		}
		cancel()
		session = c.SessionID
		evs, cancel = bus.Subscribe(session)
		data, _ := json.Marshal(map[string]string{"session": session, "agent": c.Agent})
		fmt.Fprintf(w, "event: session\ndata: %s\n\n", data)
	}
	follow()
	tick := time.NewTicker(keepalive)
	defer tick.Stop()
	for {
		if err := rc.Flush(); err != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-wake.wake:
			follow()
		case <-wake.typing:
			follow()
		case e := <-evs:
			if v, ok := liveFrom(e); ok {
				data, _ := json.Marshal(v)
				fmt.Fprintf(w, "event: activity\ndata: %s\n\n", data)
			}
		case <-tick.C:
			fmt.Fprint(w, ": keepalive\n\n")
		}
	}
}

// speech speaks one of the agent's replies with the configured tts
// provider, for the page's "speak replies" toggle.
func (s *Server) speech(w http.ResponseWriter, r *http.Request) {
	conv := r.PathValue("conversation")
	seq, err := strconv.ParseInt(r.PathValue("seq"), 10, 64)
	if !conversationID.MatchString(conv) || err != nil {
		httpError(w, http.StatusBadRequest, "bad conversation or message")
		return
	}
	sp := s.Gateway.Speech
	if sp == nil {
		httpError(w, http.StatusNotImplemented, "speech is not configured (tts in config.yaml)")
		return
	}
	e, err := s.Store.Message(r.Context(), conv, seq)
	if err != nil {
		s.fail(w, "read the message", err)
		return
	}
	if e == nil || e.Role != "assistant" {
		httpError(w, http.StatusNotFound, "no such reply")
		return
	}
	text := mediatools.SpeechText(e.Text)
	if strings.TrimSpace(text) == "" {
		httpError(w, http.StatusUnprocessableEntity, "this reply has nothing to read aloud")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	audio, err := sp.Synthesize(ctx, text, "")
	if err != nil {
		if errors.Is(err, mediatools.ErrTooLong) {
			httpError(w, http.StatusRequestEntityTooLarge, err.Error())
			return
		}
		s.Log.Warn("dashboard speech failed", "err", err)
		httpError(w, http.StatusBadGateway, "speech failed: "+shortText(err.Error(), 200))
		return
	}
	h := w.Header()
	h.Set("Content-Type", audio.MediaType)
	h.Set("Cache-Control", "private, max-age=3600")
	h.Set("Content-Length", strconv.Itoa(len(audio.Data)))
	w.Write(audio.Data)
}

// Message returns one message of a conversation, or nil.
func (s *Store) Message(ctx context.Context, conversation string, seq int64) (*Event, error) {
	var e Event
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT seq, role, text, at FROM messages WHERE conversation = ? AND seq = ?`,
		conversation, seq).Scan(&e.Seq, &e.Role, &e.Text, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	e.At = time.UnixMilli(at).UTC()
	return &e, err
}

// chatUpload is POST /api/chat as multipart/form-data: the fields of the
// JSON form (conversation, text, agent, id), any number of "file" parts,
// which become attachments like photos and documents on other channels,
// and an optional "audio" part, a recording from the page's mic, which is
// transcribed here so the transcript is the message (and a spoken "yes"
// answers an approval).
func (s *Server) chatUpload(w http.ResponseWriter, r *http.Request) {
	// A form post needs no preflight, so ask for a header only scripts can
	// set; cross-site pages cannot send it without one.
	if r.Header.Get("X-Habakkuk-Client") == "" || crossSite(r) {
		httpError(w, http.StatusForbidden, "uploads need the X-Habakkuk-Client header")
		return
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(5 * time.Minute))
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload+maxBody)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		if errors.As(err, new(*http.MaxBytesError)) {
			httpError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("files are larger than %d MB together", maxUpload>>20))
		} else {
			httpError(w, http.StatusBadRequest, "bad multipart form: "+err.Error())
		}
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	form := r.MultipartForm
	field := func(k string) string {
		if v := form.Value[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	req := chatRequest{Conversation: field("conversation"), Text: strings.TrimSpace(field("text")),
		Agent: field("agent"), ID: field("id")}
	if len(form.File["file"]) > maxUploadFiles {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("at most %d files per message", maxUploadFiles))
		return
	}
	var atts []channel.Attachment
	var names []string
	for _, fh := range form.File["file"] {
		data, err := readPart(fh)
		if err != nil {
			httpError(w, http.StatusBadRequest, "could not read "+fh.Filename)
			return
		}
		name := media.SafeName(fh.Filename)
		atts = append(atts, channel.Attachment{
			Name: name, MediaType: media.Detect(name, fh.Header.Get("Content-Type"), data), Size: int64(len(data)),
			Fetch: func(context.Context) ([]byte, error) { return data, nil },
		})
		names = append(names, name)
	}
	transcript := ""
	if fhs := form.File["audio"]; len(fhs) > 0 {
		if s.Gateway.Transcribe == nil {
			httpError(w, http.StatusNotImplemented, "transcription is not configured (transcription in config.yaml)")
			return
		}
		data, err := readPart(fhs[0])
		if err != nil {
			httpError(w, http.StatusBadRequest, "could not read the recording")
			return
		}
		name := media.SafeName(fhs[0].Filename)
		// Recordings sniff as their container (WebM and MP4 read as video);
		// this part is a voice, so say audio.
		mt := media.Detect(name, fhs[0].Header.Get("Content-Type"), data)
		if rest, ok := strings.CutPrefix(mt, "video/"); ok {
			mt = "audio/" + rest
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		transcript, err = s.Gateway.Transcribe(ctx, name, mt, data)
		cancel()
		if err != nil {
			s.Log.Warn("dashboard transcription failed", "err", err)
			httpError(w, http.StatusBadGateway, "transcription failed: "+shortText(err.Error(), 200))
			return
		}
		transcript = strings.TrimSpace(transcript)
		if transcript == "" {
			httpError(w, http.StatusUnprocessableEntity, "heard nothing in the recording")
			return
		}
		req.Text = strings.TrimSpace(req.Text + "\n" + transcript)
	}
	if req.Text == "" && len(atts) == 0 {
		httpError(w, http.StatusBadRequest, "send text, a file or a recording")
		return
	}
	shown := req.Text
	if len(names) > 0 {
		shown = strings.TrimSpace(shown + "\n\nAttached: " + strings.Join(names, ", "))
	}
	out, status, msg := s.deliverChat(r.Context(), req, shown, atts)
	if msg != "" {
		httpError(w, status, msg)
		return
	}
	if transcript != "" {
		out["transcript"] = transcript
	}
	writeJSON(w, status, out)
}

func readPart(fh *multipart.FileHeader) ([]byte, error) {
	f, err := fh.Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxUpload+1))
}

// deliverChat records a web message and hands it to the gateway, like the
// JSON form of POST /api/chat. shown is the text kept in the chat's log.
// It returns the reply body, or a status and error message.
func (s *Server) deliverChat(rctx context.Context, req chatRequest, shown string, atts []channel.Attachment) (map[string]string, int, string) {
	switch {
	case !conversationID.MatchString(req.Conversation):
		return nil, http.StatusBadRequest, "conversation must be 1-128 letters, digits, '.', '_', ':' or '-'"
	case utf8.RuneCountInString(req.Text) > maxText:
		return nil, http.StatusRequestEntityTooLarge, fmt.Sprintf("text is longer than %d characters", maxText)
	case len(req.ID) > 128:
		return nil, http.StatusBadRequest, "id is longer than 128 characters"
	}
	if _, ok := s.Gateway.Runner.Agents[req.Agent]; req.Agent != "" && !ok {
		return nil, http.StatusBadRequest, fmt.Sprintf("unknown agent %q", req.Agent)
	}
	in := s.currentInbox()
	if in == nil {
		return nil, http.StatusServiceUnavailable, "the web channel is starting; try again shortly"
	}
	ctx := context.WithoutCancel(rctx)
	to := channel.Address{Account: s.Account, Conversation: req.Conversation}
	if req.Agent == "" {
		if conv, err := s.Gateway.Store.Conversation(ctx, to); err == nil && conv != nil {
			req.Agent = conv.Agent
		}
	}
	if req.ID == "" {
		req.ID = jobs.NewID("web")
	}
	if _, err := s.Store.Append(ctx, req.Conversation, "user", req.ID, shown); err != nil {
		s.Log.Error("record web message failed", "err", err)
		return nil, http.StatusInternalServerError, "could not record the message"
	}
	s.hub.notify(req.Conversation, false)
	m := channel.Message{
		Account: s.Account, Conversation: req.Conversation, ID: req.ID, Sender: "web",
		Text: req.Text, Direct: true, Agent: req.Agent, Attachments: atts,
	}
	in.Deliver(ctx, m)
	out := map[string]string{"message": req.ID, "turn": ""}
	if j, err := s.Gateway.Queue.Get(ctx, channel.TurnID(m)); err == nil && j != nil {
		out["turn"] = j.ID
	}
	return out, http.StatusAccepted, ""
}
