// Package fakehabakkuk is an in-memory stand-in for the parts of Habakkuk's HTTP
// API the desktop app uses, for tests.
package fakehabakkuk

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/habakkukapi"
)

// Server is a fake Habakkuk.
type Server struct {
	*httptest.Server
	Token string

	mu        sync.Mutex
	overview  habakkukapi.Overview
	approvals []habakkukapi.Approval
	convs     []habakkukapi.Conversation
	msgs      map[string][]habakkukapi.Message
	wake      chan struct{} // closed and replaced on every change
	drop      chan struct{} // closed to end every open stream
	answers   map[string]bool
	sent      []map[string]string
	follows   []string // "<conversation> after=<n>" per stream request
	headers   []http.Header
}

// New starts a fake that wants token ("" for none).
func New(token string) *Server {
	s := &Server{Token: token, msgs: map[string][]habakkukapi.Message{}, wake: make(chan struct{}),
		drop: make(chan struct{}), answers: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /app/{file...}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, "<!doctype html><title>Habakkuk</title>dashboard %s", r.PathValue("file"))
	})
	mux.HandleFunc("GET /api/app", s.auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, habakkukapi.AppInfo{Account: "web", Agents: []string{"habakkuk", "ops"}, Default: "habakkuk"})
	}))
	mux.HandleFunc("GET /api/overview", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		writeJSON(w, s.overview)
	}))
	mux.HandleFunc("GET /api/approvals", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		writeJSON(w, map[string]any{"approvals": append([]habakkukapi.Approval{}, s.approvals...)})
	}))
	mux.HandleFunc("POST /api/approvals/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Approve bool `json:"approve"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		id := r.PathValue("id")
		s.mu.Lock()
		defer s.mu.Unlock()
		for i, a := range s.approvals {
			if a.ID == id {
				s.approvals = append(s.approvals[:i], s.approvals[i+1:]...)
				s.answers[id] = body.Approve
				writeJSON(w, map[string]bool{"ok": true})
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, map[string]string{"error": "not waiting"})
	}))
	mux.HandleFunc("GET /api/chat/conversations", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		writeJSON(w, map[string]any{"conversations": append([]habakkukapi.Conversation{}, s.convs...)})
	}))
	mux.HandleFunc("POST /api/chat", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.sent = append(s.sent, body)
		s.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, map[string]string{"message": "m1", "turn": "t1"})
	}))
	mux.HandleFunc("GET /api/chat/{conversation}/events", s.auth(s.events))
	s.Server = httptest.NewServer(mux)
	return s
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.headers = append(s.headers, r.Header.Clone())
		s.mu.Unlock()
		if s.Token != "" && r.Header.Get("Authorization") != "Bearer "+s.Token {
			w.WriteHeader(http.StatusUnauthorized)
			writeJSON(w, map[string]string{"error": "missing or wrong token"})
			return
		}
		next(w, r)
	}
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	conv := r.PathValue("conversation")
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	s.mu.Lock()
	s.follows = append(s.follows, fmt.Sprintf("%s after=%d", conv, after))
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "retry: 3000\n\n: hello\n\n")
	flush := func() { w.(http.Flusher).Flush() }
	for {
		s.mu.Lock()
		var out []habakkukapi.Message
		for _, m := range s.msgs[conv] {
			if m.Seq > after {
				out = append(out, m)
			}
		}
		wake, drop := s.wake, s.drop
		s.mu.Unlock()
		for _, m := range out {
			data, _ := json.Marshal(m)
			fmt.Fprintf(w, "id: %d\nevent: message\ndata: %s\n\n", m.Seq, data)
			after = m.Seq
		}
		fmt.Fprint(w, "event: typing\ndata: {}\n\n")
		flush()
		select {
		case <-r.Context().Done():
			return
		case <-drop:
			return
		case <-wake:
		}
	}
}

func (s *Server) changed() { // callers hold s.mu
	close(s.wake)
	s.wake = make(chan struct{})
}

// SetOverview sets what GET /api/overview answers.
func (s *Server) SetOverview(o habakkukapi.Overview) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.overview = o
}

// AddApproval makes a call wait for approval.
func (s *Server) AddApproval(a habakkukapi.Approval) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.approvals = append(s.approvals, a)
}

// Answer returns how an approval was answered, and whether it was.
func (s *Server) Answer(id string) (approve, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	approve, ok = s.answers[id]
	return approve, ok
}

// AddConversation lists a web conversation.
func (s *Server) AddConversation(c habakkukapi.Conversation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.convs = append(s.convs, c)
}

// Push appends a message to a conversation and wakes its streams.
func (s *Server) Push(conv, role, text string, at time.Time) habakkukapi.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := habakkukapi.Message{Seq: int64(len(s.msgs[conv]) + 1), Role: role, Text: text, At: at}
	s.msgs[conv] = append(s.msgs[conv], m)
	s.changed()
	return m
}

// DropStreams ends every open event stream, as a restart or network
// failure would.
func (s *Server) DropStreams() {
	s.mu.Lock()
	defer s.mu.Unlock()
	close(s.drop)
	s.drop = make(chan struct{})
}

// Close ends open streams, then the server.
func (s *Server) Close() {
	s.DropStreams()
	s.CloseClientConnections()
	s.Server.Close()
}

// Follows lists the stream requests so far ("conv after=N").
func (s *Server) Follows() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.follows...)
}

// Sent lists the bodies POSTed to /api/chat.
func (s *Server) Sent() []map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]string{}, s.sent...)
}

// LastHeader returns the headers of the last API request.
func (s *Server) LastHeader() http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.headers) == 0 {
		return nil
	}
	return s.headers[len(s.headers)-1]
}

// HasFollow reports whether a stream request matched prefix.
func (s *Server) HasFollow(prefix string) bool {
	for _, f := range s.Follows() {
		if strings.HasPrefix(f, prefix) {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, v any) {
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	_ = json.NewEncoder(w).Encode(v)
}
