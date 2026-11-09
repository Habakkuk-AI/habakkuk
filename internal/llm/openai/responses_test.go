package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

func writeSSE(w io.Writer, events ...string) {
	for _, e := range events {
		var head struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(e), &head)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", head.Type, e)
	}
}

func TestResponsesChatGPT(t *testing.T) {
	var body map[string]any
	var hdr http.Header
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, hdr = r.URL.Path, r.Header.Clone()
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w,
			`{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","status":"in_progress","output":[]}}`,
			`{"type":"response.reasoning_summary_text.delta","sequence_number":1,"item_id":"rs_1","delta":"plan"}`,
			`{"type":"response.output_item.done","sequence_number":2,"output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"plan"}],"encrypted_content":"ENC"}}`,
			`{"type":"response.output_text.delta","sequence_number":3,"item_id":"msg_1","delta":"On it."}`,
			`{"type":"response.output_item.done","sequence_number":4,"output_index":1,"item":{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"On it.","annotations":[]}]}}`,
			`{"type":"response.output_item.added","sequence_number":5,"output_index":2,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"ls","arguments":""}}`,
			`{"type":"response.output_item.done","sequence_number":6,"output_index":2,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"ls","arguments":"{\"dir\":\".\"}","status":"completed"}}`,
			`{"type":"response.completed","sequence_number":7,"response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":900,"input_tokens_details":{"cached_tokens":800},"output_tokens":40,"output_tokens_details":{"reasoning_tokens":10},"total_tokens":940}}}`,
		)
	}))
	defer srv.Close()

	p := &Responses{BaseURL: srv.URL, ChatGPT: true, AccountID: func() string { return "acct_1" }}
	ctx := llm.WithCaller(context.Background(), llm.Caller{Agent: "habakkuk", Session: "ses_1"})
	req := llm.Request{
		Model: "gpt-5.5", System: "be brief", MaxTokens: 5000, Effort: "high",
		Tools: []llm.ToolSpec{{Name: "ls", Description: "list", Schema: json.RawMessage(`{"type":"object","properties":{"dir":{"type":"string"}}}`)}},
		Messages: []llm.Message{
			llm.UserText("hello"),
			{Role: llm.Assistant, Blocks: []llm.Block{
				{Type: llm.Thinking, Text: "earlier", Signature: "OLDENC", Data: "rs_0", Provider: responsesType},
				{Type: llm.Thinking, Text: "foreign", Signature: "x", Provider: "anthropic"},
				{Type: llm.ToolUse, ToolUseID: "call_0", ToolName: "ls", Input: json.RawMessage(`{}`)},
			}},
			{Role: llm.User, Blocks: []llm.Block{{Type: llm.ToolResult, ToolUseID: "call_0", Text: "a.txt"}}},
		},
	}
	var text, thinking strings.Builder
	resp, err := p.Stream(ctx, "ACCESS", req, func(e llm.Event) {
		switch e.Type {
		case llm.EventText:
			text.WriteString(e.Text)
		case llm.EventThinking:
			thinking.WriteString(e.Text)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/responses" {
		t.Errorf("path %s", path)
	}
	for k, want := range map[string]string{"Authorization": "Bearer ACCESS", "Chatgpt-Account-Id": "acct_1", "Originator": "habakkuk", "Session-Id": "ses_1"} {
		if hdr.Get(k) != want {
			t.Errorf("header %s = %q, want %q", k, hdr.Get(k), want)
		}
	}
	if body["store"] != false || body["instructions"] != "be brief" || body["max_output_tokens"] != nil || body["prompt_cache_key"] != "ses_1" ||
		body["stream"] != true || body["include"].([]any)[0] != "reasoning.encrypted_content" {
		t.Errorf("body %v", body)
	}
	if r := body["reasoning"].(map[string]any); r["effort"] != "high" || r["summary"] != "auto" {
		t.Errorf("reasoning %v", r)
	}
	input := body["input"].([]any)
	if len(input) != 4 {
		t.Fatalf("input items: %v", input)
	}
	rs := input[1].(map[string]any)
	fc := input[2].(map[string]any)
	out := input[3].(map[string]any)
	if rs["type"] != "reasoning" || rs["encrypted_content"] != "OLDENC" || rs["id"] != "rs_0" ||
		fc["type"] != "function_call" || fc["call_id"] != "call_0" ||
		out["type"] != "function_call_output" || out["call_id"] != "call_0" || out["output"] != "a.txt" {
		t.Errorf("replayed items: %v %v %v", rs, fc, out)
	}

	if text.String() != "On it." || thinking.String() != "plan" || resp.StopReason != llm.StopToolUse {
		t.Errorf("text %q thinking %q stop %s", text.String(), thinking.String(), resp.StopReason)
	}
	b := resp.Message.Blocks
	if len(b) != 3 || b[0].Signature != "ENC" || b[0].Data != "rs_1" || b[1].Text != "On it." || b[2].ToolUseID != "call_1" || string(b[2].Input) != `{"dir":"."}` {
		t.Fatalf("blocks %+v", b)
	}
	if want := (llm.Usage{InputTokens: 100, OutputTokens: 40, CacheReadTokens: 800}); resp.Usage != want {
		t.Errorf("usage %+v", resp.Usage)
	}
}

func TestResponsesErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		events []string
		want   llm.ErrorKind
	}{
		{429, nil, llm.ErrRateLimit},
		{401, nil, llm.ErrAuth},
		{200, []string{`{"type":"response.failed","sequence_number":1,"response":{"id":"r","status":"failed","output":[],"error":{"code":"rate_limit_exceeded","message":"slow down"}}}`}, llm.ErrRateLimit},
		{200, []string{`{"type":"error","sequence_number":1,"code":"context_length_exceeded","message":"too long"}`}, llm.ErrContextOverflow},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if tc.status != 200 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				io.WriteString(w, `{"error":{"message":"nope","type":"x"}}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			writeSSE(w, tc.events...)
		}))
		_, err := (&Responses{BaseURL: srv.URL}).Stream(context.Background(), "k", llm.Request{Model: "m"}, func(llm.Event) {})
		srv.Close()
		if got := llm.Classify(err).Kind; got != tc.want {
			t.Errorf("%d %v: %s, want %s (%v)", tc.status, tc.events, got, tc.want, err)
		}
	}
}

func TestChatCompletionsHeadersAndModels(t *testing.T) {
	var seen http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"object":"list","data":[{"id":"gpt-5.5","object":"model"},{"id":"claude-sonnet-5-5","object":"model"}]}`)
	}))
	defer srv.Close()
	p := &Provider{BaseURL: srv.URL, Headers: func(llm.Request) map[string]string { return map[string]string{"Copilot-Integration-Id": "x"} }}
	models, err := p.ListModels(context.Background(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(models, ",") != "gpt-5.5,claude-sonnet-5-5" || seen.Get("Copilot-Integration-Id") != "x" || seen.Get("Authorization") != "Bearer tok" {
		t.Fatalf("models %v headers %v", models, seen)
	}
}
