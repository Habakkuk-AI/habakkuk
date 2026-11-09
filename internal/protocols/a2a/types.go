// Package a2a speaks the Agent2Agent protocol (JSON-RPC over HTTP): a
// server that exposes Habakkuk's agents to other agents, mapping A2A tasks to
// Habakkuk turns and contexts to sessions, and a client with the a2a toolset
// for calling other agents.
//
// Two versions of the protocol are in use and both are spoken. Version 1.0
// (the current specification) names methods after its gRPC service
// (SendMessage, SendStreamingMessage, GetTask, CancelTask), writes enums as
// TASK_STATE_COMPLETED and ROLE_USER, tells parts apart by which field is
// set, and wraps results ({"task": ...}). Version 0.3, which most deployed
// agents still speak, uses message/send, message/stream, tasks/get and
// tasks/cancel, lowercase states and roles, and a "kind" field on every
// object. The server answers each request in the version of its method; a
// client reads both shapes.
package a2a

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Version is a protocol version Habakkuk speaks.
type Version int

const (
	V03 Version = iota // "0.3"
	V10                // "1.0"
)

func (v Version) String() string {
	if v == V10 {
		return "1.0"
	}
	return "0.3"
}

// Task states, in 0.3 spelling; 1.0 spells them TASK_STATE_<UPPER>.
const (
	StateSubmitted     = "submitted"
	StateWorking       = "working"
	StateInputRequired = "input-required"
	StateAuthRequired  = "auth-required"
	StateCompleted     = "completed"
	StateCanceled      = "canceled"
	StateFailed        = "failed"
	StateRejected      = "rejected"
	StateUnknown       = "unknown"
)

// Terminal reports whether a task in this state is finished for good.
func Terminal(state string) bool {
	switch state {
	case StateCompleted, StateCanceled, StateFailed, StateRejected:
		return true
	}
	return false
}

// Interrupted reports whether a task in this state waits for the client.
func Interrupted(state string) bool {
	return state == StateInputRequired || state == StateAuthRequired
}

// Part is one piece of content: text, structured data, or a file by URL or
// by value.
type Part struct {
	Text      string
	IsText    bool
	Data      json.RawMessage
	URL       string
	Raw       []byte
	MediaType string
	Filename  string
}

// TextPart is a text part.
func TextPart(s string) Part { return Part{Text: s, IsText: true} }

// Message is one turn of conversation. Role is "user" or "agent".
type Message struct {
	ID        string
	Role      string
	ContextID string
	TaskID    string
	Parts     []Part
	Metadata  map[string]any
}

// Text joins the message's text parts and renders data parts as JSON.
func (m *Message) Text() string {
	if m == nil {
		return ""
	}
	return partsText(m.Parts)
}

func partsText(parts []Part) string {
	var out []string
	for _, p := range parts {
		switch {
		case p.IsText:
			out = append(out, p.Text)
		case len(p.Data) > 0:
			out = append(out, string(p.Data))
		case p.URL != "":
			out = append(out, fmt.Sprintf("[file %s %s]", p.Filename, p.URL))
		case p.Raw != nil:
			out = append(out, fmt.Sprintf("[file %s, %s, %d bytes]", p.Filename, p.MediaType, len(p.Raw)))
		}
	}
	return strings.Join(out, "\n")
}

// Artifact is an output of a task.
type Artifact struct {
	ID          string
	Name        string
	Description string
	Parts       []Part
}

// Task is a unit of work with a lifecycle.
type Task struct {
	ID        string
	ContextID string
	State     string   // 0.3 spelling
	Status    *Message // the status message, if any
	Timestamp time.Time
	Artifacts []Artifact
	History   []Message
}

// Text is what the task produced: its artifacts' text, else the status
// message's.
func (t *Task) Text() string {
	var out []string
	for _, a := range t.Artifacts {
		if s := partsText(a.Parts); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return t.Status.Text()
	}
	return strings.Join(out, "\n\n")
}

// StatusUpdate is a stream event: the task's state changed.
type StatusUpdate struct {
	TaskID    string
	ContextID string
	State     string
	Message   *Message
	Final     bool // 0.3 only: the last event of the stream
}

// ArtifactUpdate is a stream event: an artifact or a chunk of one.
type ArtifactUpdate struct {
	TaskID    string
	ContextID string
	Artifact  Artifact
	Append    bool
	LastChunk bool
}

// Event is one result of SendMessage or of a stream: exactly one field is
// set.
type Event struct {
	Task     *Task
	Message  *Message
	Status   *StatusUpdate
	Artifact *ArtifactUpdate
}

// --- encoding ---

func encodePart(v Version, p Part) map[string]any {
	out := map[string]any{}
	switch {
	case p.IsText:
		out["text"] = p.Text
		if v == V03 {
			out["kind"] = "text"
		}
	case len(p.Data) > 0:
		out["data"] = p.Data
		if v == V03 {
			out["kind"] = "data"
		}
	default:
		if v == V03 {
			f := map[string]any{}
			if p.Filename != "" {
				f["name"] = p.Filename
			}
			if p.MediaType != "" {
				f["mimeType"] = p.MediaType
			}
			if p.URL != "" {
				f["uri"] = p.URL
			} else {
				f["bytes"] = base64.StdEncoding.EncodeToString(p.Raw)
			}
			return map[string]any{"kind": "file", "file": f}
		}
		if p.URL != "" {
			out["url"] = p.URL
		} else {
			out["raw"] = base64.StdEncoding.EncodeToString(p.Raw)
		}
	}
	if v == V10 {
		if p.MediaType != "" {
			out["mediaType"] = p.MediaType
		}
		if p.Filename != "" {
			out["filename"] = p.Filename
		}
	}
	return out
}

func encodeParts(v Version, parts []Part) []map[string]any {
	out := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		out = append(out, encodePart(v, p))
	}
	return out
}

func encodeRole(v Version, role string) string {
	if v == V10 {
		return "ROLE_" + strings.ToUpper(role)
	}
	return role
}

func encodeState(v Version, state string) string {
	if v == V10 {
		return "TASK_STATE_" + strings.ToUpper(strings.ReplaceAll(state, "-", "_"))
	}
	return state
}

// EncodeMessage renders a message in a version's JSON shape.
func EncodeMessage(v Version, m *Message) map[string]any {
	out := map[string]any{"messageId": m.ID, "role": encodeRole(v, m.Role), "parts": encodeParts(v, m.Parts)}
	if v == V03 {
		out["kind"] = "message"
	}
	if m.ContextID != "" {
		out["contextId"] = m.ContextID
	}
	if m.TaskID != "" {
		out["taskId"] = m.TaskID
	}
	if len(m.Metadata) > 0 {
		out["metadata"] = m.Metadata
	}
	return out
}

func encodeArtifact(v Version, a Artifact) map[string]any {
	out := map[string]any{"artifactId": a.ID, "parts": encodeParts(v, a.Parts)}
	if a.Name != "" {
		out["name"] = a.Name
	}
	if a.Description != "" {
		out["description"] = a.Description
	}
	return out
}

func encodeStatus(v Version, state string, msg *Message, ts time.Time) map[string]any {
	st := map[string]any{"state": encodeState(v, state)}
	if msg != nil {
		st["message"] = EncodeMessage(v, msg)
	}
	if !ts.IsZero() {
		st["timestamp"] = ts.UTC().Format(time.RFC3339Nano)
	}
	return st
}

// EncodeTask renders a task in a version's JSON shape.
func EncodeTask(v Version, t *Task) map[string]any {
	out := map[string]any{"id": t.ID, "contextId": t.ContextID, "status": encodeStatus(v, t.State, t.Status, t.Timestamp)}
	if v == V03 {
		out["kind"] = "task"
	}
	if len(t.Artifacts) > 0 {
		arts := make([]map[string]any, 0, len(t.Artifacts))
		for _, a := range t.Artifacts {
			arts = append(arts, encodeArtifact(v, a))
		}
		out["artifacts"] = arts
	}
	if len(t.History) > 0 {
		hist := make([]map[string]any, 0, len(t.History))
		for i := range t.History {
			hist = append(hist, EncodeMessage(v, &t.History[i]))
		}
		out["history"] = hist
	}
	return out
}

// EncodeEvent renders a send result or stream event: in 0.3 the object
// itself (its kind says what it is), in 1.0 wrapped in a field naming it.
func EncodeEvent(v Version, e Event) map[string]any {
	var key string
	var body map[string]any
	switch {
	case e.Task != nil:
		key, body = "task", EncodeTask(v, e.Task)
	case e.Message != nil:
		key, body = "message", EncodeMessage(v, e.Message)
	case e.Status != nil:
		s := e.Status
		key = "statusUpdate"
		body = map[string]any{"taskId": s.TaskID, "contextId": s.ContextID, "status": encodeStatus(v, s.State, s.Message, time.Now())}
		if v == V03 {
			body["kind"], body["final"] = "status-update", s.Final
		}
	case e.Artifact != nil:
		a := e.Artifact
		key = "artifactUpdate"
		body = map[string]any{"taskId": a.TaskID, "contextId": a.ContextID, "artifact": encodeArtifact(v, a.Artifact)}
		if a.Append {
			body["append"] = true
		}
		if a.LastChunk {
			body["lastChunk"] = true
		}
		if v == V03 {
			body["kind"] = "artifact-update"
		}
	}
	if v == V03 {
		return body
	}
	return map[string]any{key: body}
}

// --- decoding (either version) ---

type wireFile struct {
	Name     string `json:"name"`
	MimeType string `json:"mimeType"`
	Bytes    string `json:"bytes"`
	URI      string `json:"uri"`
}

type wirePart struct {
	Kind      string          `json:"kind"`
	Text      *string         `json:"text"`
	Data      json.RawMessage `json:"data"`
	File      *wireFile       `json:"file"`
	URL       string          `json:"url"`
	Raw       string          `json:"raw"`
	MediaType string          `json:"mediaType"`
	Filename  string          `json:"filename"`
}

func (w wirePart) part() (Part, error) {
	switch {
	case w.Text != nil:
		return Part{Text: *w.Text, IsText: true}, nil
	case len(w.Data) > 0 && string(w.Data) != "null":
		return Part{Data: w.Data, MediaType: w.MediaType}, nil
	case w.File != nil:
		p := Part{URL: w.File.URI, Filename: w.File.Name, MediaType: w.File.MimeType}
		if p.URL == "" {
			b, err := base64.StdEncoding.DecodeString(w.File.Bytes)
			if err != nil {
				return Part{}, fmt.Errorf("file part: bytes are not base64")
			}
			p.Raw = b
		}
		return p, nil
	case w.URL != "":
		return Part{URL: w.URL, Filename: w.Filename, MediaType: w.MediaType}, nil
	case w.Raw != "":
		b, err := base64.StdEncoding.DecodeString(w.Raw)
		if err != nil {
			return Part{}, fmt.Errorf("raw part is not base64")
		}
		return Part{Raw: b, Filename: w.Filename, MediaType: w.MediaType}, nil
	}
	return Part{}, fmt.Errorf("part has none of text, data, file, url or raw")
}

type wireMessage struct {
	Kind      string         `json:"kind"`
	MessageID string         `json:"messageId"`
	Role      string         `json:"role"`
	ContextID string         `json:"contextId"`
	TaskID    string         `json:"taskId"`
	Parts     []wirePart     `json:"parts"`
	Metadata  map[string]any `json:"metadata"`
}

func decodeRole(r string) string {
	return strings.ToLower(strings.TrimPrefix(r, "ROLE_"))
}

func decodeState(s string) string {
	s = strings.TrimPrefix(s, "TASK_STATE_")
	return strings.ReplaceAll(strings.ToLower(s), "_", "-")
}

func (w *wireMessage) message() (*Message, error) {
	m := &Message{ID: w.MessageID, Role: decodeRole(w.Role), ContextID: w.ContextID, TaskID: w.TaskID, Metadata: w.Metadata}
	for i, wp := range w.Parts {
		p, err := wp.part()
		if err != nil {
			return nil, fmt.Errorf("parts[%d]: %w", i, err)
		}
		m.Parts = append(m.Parts, p)
	}
	return m, nil
}

// DecodeMessage reads a message in either version's shape.
func DecodeMessage(raw json.RawMessage) (*Message, error) {
	var w wireMessage
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, err
	}
	return w.message()
}

type wireStatus struct {
	State     string       `json:"state"`
	Message   *wireMessage `json:"message"`
	Timestamp string       `json:"timestamp"`
}

func (w wireStatus) decode() (string, *Message, time.Time, error) {
	var msg *Message
	if w.Message != nil {
		m, err := w.Message.message()
		if err != nil {
			return "", nil, time.Time{}, err
		}
		msg = m
	}
	ts, _ := time.Parse(time.RFC3339Nano, w.Timestamp)
	return decodeState(w.State), msg, ts, nil
}

type wireArtifact struct {
	ArtifactID  string     `json:"artifactId"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Parts       []wirePart `json:"parts"`
}

func (w wireArtifact) artifact() (Artifact, error) {
	a := Artifact{ID: w.ArtifactID, Name: w.Name, Description: w.Description}
	for i, wp := range w.Parts {
		p, err := wp.part()
		if err != nil {
			return a, fmt.Errorf("artifact %s parts[%d]: %w", w.ArtifactID, i, err)
		}
		a.Parts = append(a.Parts, p)
	}
	return a, nil
}

type wireTask struct {
	Kind      string         `json:"kind"`
	ID        string         `json:"id"`
	ContextID string         `json:"contextId"`
	Status    wireStatus     `json:"status"`
	Artifacts []wireArtifact `json:"artifacts"`
	History   []wireMessage  `json:"history"`
}

func (w *wireTask) task() (*Task, error) {
	t := &Task{ID: w.ID, ContextID: w.ContextID}
	var err error
	if t.State, t.Status, t.Timestamp, err = w.Status.decode(); err != nil {
		return nil, err
	}
	for _, wa := range w.Artifacts {
		a, err := wa.artifact()
		if err != nil {
			return nil, err
		}
		t.Artifacts = append(t.Artifacts, a)
	}
	for i := range w.History {
		m, err := w.History[i].message()
		if err != nil {
			return nil, err
		}
		t.History = append(t.History, *m)
	}
	return t, nil
}

// DecodeTask reads a task in either version's shape.
func DecodeTask(raw json.RawMessage) (*Task, error) {
	var w wireTask
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, err
	}
	if w.ID == "" {
		return nil, fmt.Errorf("task has no id")
	}
	return w.task()
}

type wireStatusUpdate struct {
	TaskID    string     `json:"taskId"`
	ContextID string     `json:"contextId"`
	Status    wireStatus `json:"status"`
	Final     bool       `json:"final"`
}

type wireArtifactUpdate struct {
	TaskID    string       `json:"taskId"`
	ContextID string       `json:"contextId"`
	Artifact  wireArtifact `json:"artifact"`
	Append    bool         `json:"append"`
	LastChunk bool         `json:"lastChunk"`
}

// DecodeEvent reads a send result or stream event in either version's
// shape: a 1.0 wrapper ({"task": ...}) or a 0.3 object with a kind.
func DecodeEvent(raw json.RawMessage) (Event, error) {
	var probe struct {
		Kind           string          `json:"kind"`
		Task           json.RawMessage `json:"task"`
		Message        json.RawMessage `json:"message"`
		StatusUpdate   json.RawMessage `json:"statusUpdate"`
		ArtifactUpdate json.RawMessage `json:"artifactUpdate"`
		ID             string          `json:"id"`
		MessageID      string          `json:"messageId"`
		Status         json.RawMessage `json:"status"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return Event{}, err
	}
	kind, body := probe.Kind, raw
	switch {
	case kind != "":
	case len(probe.Task) > 0:
		kind, body = "task", probe.Task
	case len(probe.StatusUpdate) > 0:
		kind, body = "status-update", probe.StatusUpdate
	case len(probe.ArtifactUpdate) > 0:
		kind, body = "artifact-update", probe.ArtifactUpdate
	case len(probe.Message) > 0 && probe.MessageID == "":
		kind, body = "message", probe.Message
	case probe.ID != "" && len(probe.Status) > 0:
		kind = "task"
	case probe.MessageID != "":
		kind = "message"
	}
	switch kind {
	case "task":
		t, err := DecodeTask(body)
		return Event{Task: t}, err
	case "message":
		m, err := DecodeMessage(body)
		return Event{Message: m}, err
	case "status-update":
		var w wireStatusUpdate
		if err := json.Unmarshal(body, &w); err != nil {
			return Event{}, err
		}
		state, msg, _, err := w.Status.decode()
		return Event{Status: &StatusUpdate{TaskID: w.TaskID, ContextID: w.ContextID, State: state, Message: msg, Final: w.Final}}, err
	case "artifact-update":
		var w wireArtifactUpdate
		if err := json.Unmarshal(body, &w); err != nil {
			return Event{}, err
		}
		a, err := w.Artifact.artifact()
		return Event{Artifact: &ArtifactUpdate{TaskID: w.TaskID, ContextID: w.ContextID, Artifact: a, Append: w.Append, LastChunk: w.LastChunk}}, err
	}
	return Event{}, fmt.Errorf("unrecognized A2A result")
}

// JSON-RPC error codes: the standard ones and A2A's (the same numbers in
// 0.3 and 1.0).
const (
	CodeParse              = -32700
	CodeInvalidRequest     = -32600
	CodeMethodNotFound     = -32601
	CodeInvalidParams      = -32602
	CodeInternal           = -32603
	CodeTaskNotFound       = -32001
	CodeTaskNotCancelable  = -32002
	CodePushNotSupported   = -32003
	CodeUnsupported        = -32004
	CodeContentType        = -32005
	CodeVersionUnsupported = -32009
)

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("A2A error %d: %s", e.Code, e.Message) }
