package gemini

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

func sse(chunks ...string) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString("data: " + c + "\r\n\r\n")
	}
	return b.String()
}

func TestStreamRequestAndResponse(t *testing.T) {
	var got map[string]any
	var path, key string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, key = r.URL.Path+"?"+r.URL.RawQuery, r.Header.Get("x-goog-api-key")
		data, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(data, &got); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse(
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"weighing it","thought":true}]}}]}`,
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"Let me "},{"text":"check."}]}}]}`,
			`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"c1","name":"read_file","args":{"path":"a.txt"}},"thoughtSignature":"SIG1"},{"functionCall":{"id":"c2","name":"ls","args":{}}}]},"finishReason":"STOP"}],`+
				`"usageMetadata":{"promptTokenCount":1200,"cachedContentTokenCount":1000,"candidatesTokenCount":30,"thoughtsTokenCount":12}}`,
		))
	}))
	defer srv.Close()

	media := map[string][]byte{"sha256:img": []byte("PNGDATA")}
	req := llm.Request{
		Model: "gemini-3.1-pro-preview", System: "be brief", MaxTokens: 2048, Effort: "xhigh",
		Tools: []llm.ToolSpec{{Name: "read_file", Description: "read", Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"additionalProperties":false}`)}},
		Messages: []llm.Message{
			{Role: llm.User, Blocks: []llm.Block{{Type: llm.Text, Text: "hi"}, {Type: llm.Image, Media: "sha256:img", MediaType: "image/png"}}},
			// A tool call written by another provider: gets the bypass signature.
			{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Thinking, Text: "hidden", Provider: "anthropic"}, {Type: llm.ToolUse, ToolUseID: "toolu_1", ToolName: "read_file", Input: json.RawMessage(`{"path":"x"}`)}}},
			{Role: llm.User, Blocks: []llm.Block{{Type: llm.ToolResult, ToolUseID: "toolu_1", Text: "contents", IsError: false}}},
		},
		Media: func(ref string) ([]byte, error) { return media[ref], nil },
	}
	var texts, thoughts, calls []string
	resp, err := (&Provider{BaseURL: srv.URL}).Stream(context.Background(), "KEY", req, func(e llm.Event) {
		switch e.Type {
		case llm.EventText:
			texts = append(texts, e.Text)
		case llm.EventThinking:
			thoughts = append(thoughts, e.Text)
		case llm.EventToolCall:
			calls = append(calls, e.Text)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/models/gemini-3.1-pro-preview:streamGenerateContent?alt=sse" || key != "KEY" {
		t.Fatalf("path %q key %q", path, key)
	}

	// Request shape.
	if got["systemInstruction"].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"] != "be brief" {
		t.Errorf("system instruction: %v", got["systemInstruction"])
	}
	gc := got["generationConfig"].(map[string]any)
	if gc["maxOutputTokens"].(float64) != 2048 || gc["thinkingConfig"].(map[string]any)["thinkingLevel"] != "high" ||
		gc["thinkingConfig"].(map[string]any)["includeThoughts"] != true {
		t.Errorf("generation config: %v", gc)
	}
	decl := got["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)[0].(map[string]any)
	if decl["name"] != "read_file" || decl["parametersJsonSchema"].(map[string]any)["additionalProperties"] != false {
		t.Errorf("tool declaration: %v", decl)
	}
	contents := got["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("contents: %v", contents)
	}
	user := contents[0].(map[string]any)
	if user["role"] != "user" || user["parts"].([]any)[1].(map[string]any)["inlineData"].(map[string]any)["data"] != "UE5HREFUQQ==" {
		t.Errorf("user content: %v", user)
	}
	model := contents[1].(map[string]any)
	parts := model["parts"].([]any)
	if model["role"] != "model" || len(parts) != 1 {
		t.Fatalf("other providers' thinking must be dropped: %v", model)
	}
	fc := parts[0].(map[string]any)
	if fc["thoughtSignature"] != skipSignature || fc["functionCall"].(map[string]any)["id"] != "toolu_1" {
		t.Errorf("foreign function call: %v", fc)
	}
	fr := contents[2].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionResponse"].(map[string]any)
	if fr["name"] != "read_file" || fr["id"] != "toolu_1" || fr["response"].(map[string]any)["result"] != "contents" {
		t.Errorf("function response: %v", fr)
	}

	// Response.
	if strings.Join(texts, "") != "Let me check." || strings.Join(thoughts, "") != "weighing it" || strings.Join(calls, ",") != "read_file,ls" {
		t.Errorf("events: %q %q %q", texts, thoughts, calls)
	}
	if resp.StopReason != llm.StopToolUse {
		t.Errorf("stop = %s", resp.StopReason)
	}
	want := llm.Usage{InputTokens: 200, OutputTokens: 42, CacheReadTokens: 1000}
	if resp.Usage != want {
		t.Errorf("usage = %+v, want %+v", resp.Usage, want)
	}
	b := resp.Message.Blocks
	if len(b) != 4 || b[0].Type != llm.Thinking || b[1].Text != "Let me check." ||
		b[2].ToolUseID != "c1" || b[2].Signature != "SIG1" || b[3].ToolUseID != "c2" || b[3].Signature != "" {
		t.Fatalf("blocks: %+v", b)
	}

	// Replaying our own answer keeps its signatures exactly.
	again := buildRequest(llm.Request{Model: "gemini-3.1-pro-preview", Messages: []llm.Message{resp.Message}})
	ps := again.Contents[0].Parts
	if len(ps) != 4 || !ps[0].Thought || ps[2].ThoughtSignature != "SIG1" || ps[3].ThoughtSignature != "" {
		t.Fatalf("replay: %+v", ps)
	}
}

func TestThinkingConfig(t *testing.T) {
	if tc := thinking("gemini-2.5-flash", "low"); tc.ThinkingBudget == nil || *tc.ThinkingBudget != 1024 || tc.ThinkingLevel != "" {
		t.Errorf("2.5: %+v", tc)
	}
	if tc := thinking("gemini-4-argon", "medium"); tc.ThinkingLevel != "medium" || tc.ThinkingBudget != nil {
		t.Errorf("4: %+v", tc)
	}
	if tc := thinking("gemma-4", "high"); tc != nil {
		t.Errorf("non-gemini model got thinking config: %+v", tc)
	}
}

func TestLocalCallIDsAreNotSentBack(t *testing.T) {
	acc := &accumulator{onEvent: func(llm.Event) {}}
	acc.add(chunk{Candidates: []struct {
		Content      content `json:"content"`
		FinishReason string  `json:"finishReason"`
	}{{Content: content{Parts: []part{{FunctionCall: &functionCall{Name: "ls"}}}}}}})
	resp := acc.response()
	id := resp.Message.Blocks[0].ToolUseID
	if !strings.HasPrefix(id, localIDPrefix) {
		t.Fatalf("id %q", id)
	}
	req := buildRequest(llm.Request{Model: "gemini-2.5-flash", Messages: []llm.Message{resp.Message,
		{Role: llm.User, Blocks: []llm.Block{{Type: llm.ToolResult, ToolUseID: id, Text: "ok"}}}}})
	if req.Contents[0].Parts[0].FunctionCall.ID != "" || req.Contents[1].Parts[0].FunctionResponse.ID != "" {
		t.Fatal("made-up ids were sent")
	}
}

func TestErrorClasses(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   llm.ErrorKind
	}{
		{429, `{"error":{"code":429,"message":"Resource has been exhausted","status":"RESOURCE_EXHAUSTED"}}`, llm.ErrRateLimit},
		{503, `{"error":{"code":503,"message":"The model is overloaded. Please try again later.","status":"UNAVAILABLE"}}`, llm.ErrOverloaded},
		{500, `{"error":{"code":500,"message":"internal","status":"INTERNAL"}}`, llm.ErrServer},
		{403, `{"error":{"code":403,"message":"API key not valid","status":"PERMISSION_DENIED"}}`, llm.ErrAuth},
		{400, `{"error":{"code":400,"message":"The input token count (2000000) exceeds the maximum number of tokens allowed (1048576).","status":"INVALID_ARGUMENT"}}`, llm.ErrContextOverflow},
		{400, `{"error":{"code":400,"message":"Gemini API free tier is not available in your country. Please enable billing.","status":"FAILED_PRECONDITION"}}`, llm.ErrBilling},
		{400, `[{"error":{"code":400,"message":"bad schema","status":"INVALID_ARGUMENT"}}]`, llm.ErrBadRequest},
		{404, `{"error":{"code":404,"message":"models/nope is not found","status":"NOT_FOUND"}}`, llm.ErrNotFound},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			io.WriteString(w, tc.body)
		}))
		_, err := (&Provider{BaseURL: srv.URL}).Stream(context.Background(), "k", llm.Request{Model: "gemini-x"}, func(llm.Event) {})
		srv.Close()
		if got := llm.Classify(err).Kind; got != tc.want {
			t.Errorf("%d %s: %s, want %s (%v)", tc.status, tc.body, got, tc.want, err)
		}
	}
}

func TestErrorInsideStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, sse(`{"candidates":[{"content":{"parts":[{"text":"par"}]}}]}`,
			`{"error":{"code":503,"message":"The model is overloaded.","status":"UNAVAILABLE"}}`))
	}))
	defer srv.Close()
	_, err := (&Provider{BaseURL: srv.URL}).Stream(context.Background(), "k", llm.Request{Model: "gemini-x"}, func(llm.Event) {})
	if llm.Classify(err).Kind != llm.ErrOverloaded {
		t.Fatalf("err = %v", err)
	}
}

func TestListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "k" {
			w.WriteHeader(403)
			return
		}
		if r.URL.Query().Get("pageToken") == "" {
			io.WriteString(w, `{"models":[{"name":"models/gemini-4-argon","supportedGenerationMethods":["generateContent","countTokens"]},
				{"name":"models/text-embedding-5","supportedGenerationMethods":["embedContent"]}],"nextPageToken":"p2"}`)
			return
		}
		io.WriteString(w, `{"models":[{"name":"models/gemini-3.8-flash","supportedGenerationMethods":["generateContent"]}]}`)
	}))
	defer srv.Close()
	got, err := (&Provider{BaseURL: srv.URL}).ListModels(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "gemini-4-argon,gemini-3.8-flash" {
		t.Fatalf("models = %v", got)
	}
}
