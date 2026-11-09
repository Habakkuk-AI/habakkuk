package web

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

func (r *rig) client(tok string) *openai.Client {
	c := openai.NewClient(option.WithBaseURL(r.ts.URL+"/v1/"), option.WithAPIKey(tok), option.WithMaxRetries(0))
	return &c
}

func apiErr(t *testing.T, err error, status int, want string) {
	t.Helper()
	var e *openai.Error
	if !errors.As(err, &e) {
		t.Fatalf("want an API error %d, got %v", status, err)
	}
	if e.StatusCode != status || !strings.Contains(e.Message, want) {
		t.Fatalf("got %d %q, want %d containing %q", e.StatusCode, e.Message, status, want)
	}
}

func (r *rig) transcript(t *testing.T, sessionID string) []llm.Message {
	t.Helper()
	recs, err := r.s.Gateway.Runner.Sessions.Messages(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	var out []llm.Message
	for _, rec := range recs {
		out = append(out, rec.Message)
	}
	return out
}

func TestOpenAIModels(t *testing.T) {
	r := newRig(t, token)
	ctx := context.Background()
	c := r.client(token)
	page, err := c.Models.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range page.Data {
		ids = append(ids, m.ID)
	}
	if !slices.Equal(ids, []string{"habakkuk/habakkuk", "habakkuk/ops"}) {
		t.Fatalf("models: %v", ids)
	}
	m, err := c.Models.Get(ctx, "habakkuk/ops")
	if err != nil || m.ID != "habakkuk/ops" {
		t.Fatalf("get: %v %+v", err, m)
	}
	_, err = c.Models.Get(ctx, "habakkuk/nobody")
	apiErr(t, err, http.StatusNotFound, "does not exist")

	_, err = r.client("wrong").Models.List(ctx)
	apiErr(t, err, http.StatusUnauthorized, "token")
}

func TestOpenAIChatCompletion(t *testing.T) {
	r := newRig(t, token)
	ctx := context.Background()
	c := r.client(token)
	var res *http.Response
	out, err := c.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: "habakkuk/habakkuk",
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage("Answer in French."),
			openai.UserMessage("first question"),
			openai.AssistantMessage("first answer"),
			openai.UserMessage("hello"),
		},
	}, option.WithResponseInto(&res))
	if err != nil {
		t.Fatal(err)
	}
	if got := out.Choices[0].Message.Content; got != "echo: hello" {
		t.Fatalf("reply %q", got)
	}
	if out.Model != "habakkuk/habakkuk" || out.Choices[0].FinishReason != "stop" || !strings.HasPrefix(out.ID, "chatcmpl-") {
		t.Fatalf("completion %+v", out)
	}
	sid := res.Header.Get(sessionHeader)
	if sid == "" {
		t.Fatal("no session header")
	}
	// The fresh session was seeded with the history; the system message
	// became a note at its start.
	msgs := r.transcript(t, sid)
	if len(msgs) != 4 || msgs[0].Role != llm.User || !strings.Contains(msgs[0].Text(), "Answer in French.") ||
		!strings.Contains(msgs[0].Text(), "first question") || msgs[1].Text() != "first answer" ||
		!strings.HasSuffix(msgs[2].Text(), "hello") || msgs[3].Text() != "echo: hello" {
		t.Fatalf("transcript %+v", msgs)
	}

	// The header continues that session with only the last user message.
	out, err = c.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:    "habakkuk/habakkuk",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("ignored history"), openai.AssistantMessage("x"), openai.UserMessage("again")},
	}, option.WithHeader(sessionHeader, sid))
	if err != nil || out.Choices[0].Message.Content != "echo: again" {
		t.Fatalf("continue: %v %+v", err, out)
	}
	if n := len(r.transcript(t, sid)); n != 6 {
		t.Fatalf("continued transcript has %d messages, want 6", n)
	}

	// A session belongs to one agent.
	_, err = c.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: "habakkuk/ops", Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hi")},
	}, option.WithHeader(sessionHeader, sid))
	apiErr(t, err, http.StatusBadRequest, "belongs to habakkuk/habakkuk")
}

func TestOpenAIUserKeepsSession(t *testing.T) {
	r := newRig(t, token)
	ctx := context.Background()
	c := r.client(token)
	ask := func(user, text string) string {
		t.Helper()
		var res *http.Response
		out, err := c.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
			Model: "habakkuk/habakkuk", User: openai.String(user), Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage(text)},
		}, option.WithResponseInto(&res))
		if err != nil || out.Choices[0].Message.Content != "echo: "+text {
			t.Fatalf("ask: %v %+v", err, out)
		}
		return res.Header.Get(sessionHeader)
	}
	a1, a2, b := ask("alice", "one"), ask("alice", "two"), ask("bob", "three")
	if a1 == "" || a1 != a2 || a1 == b {
		t.Fatalf("sessions: alice %s %s, bob %s", a1, a2, b)
	}
}

func TestOpenAIChatStream(t *testing.T) {
	r := newRig(t, token)
	c := r.client(token)
	stream := c.Chat.Completions.NewStreaming(context.Background(), openai.ChatCompletionNewParams{
		Model: "habakkuk/habakkuk", Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("streamed")},
		StreamOptions: openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)},
	})
	acc := openai.ChatCompletionAccumulator{}
	chunks := 0
	for stream.Next() {
		acc.AddChunk(stream.Current())
		chunks++
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if got := acc.Choices[0].Message.Content; got != "echo: streamed" {
		t.Fatalf("streamed %q", got)
	}
	if chunks < 4 { // role, two text pieces, stop, usage
		t.Fatalf("only %d chunks", chunks)
	}
	if acc.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish %q", acc.Choices[0].FinishReason)
	}
}

func TestOpenAIRefusesClientTools(t *testing.T) {
	r := newRig(t, token)
	_, err := r.client(token).Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{
		Model:    "habakkuk/habakkuk",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hi")},
		Tools:    []openai.ChatCompletionToolUnionParam{openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{Name: "lookup"})},
	})
	apiErr(t, err, http.StatusBadRequest, "client-side tools are not supported")

	_, err = r.client(token).Responses.New(context.Background(), responses.ResponseNewParams{
		Model: "habakkuk/habakkuk", Input: responses.ResponseNewParamsInputUnion{OfString: openai.String("hi")},
		Tools: []responses.ToolUnionParam{{OfFunction: &responses.FunctionToolParam{Name: "lookup"}}},
	})
	apiErr(t, err, http.StatusBadRequest, "client-side tools are not supported")

	_, err = r.client(token).Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{
		Model: "habakkuk/habakkuk", Messages: []openai.ChatCompletionMessageParamUnion{openai.AssistantMessage("no user")},
	})
	apiErr(t, err, http.StatusBadRequest, "last message must be from the user")
}

func TestOpenAIIdempotencyKey(t *testing.T) {
	r := newRig(t, token)
	c := r.client(token)
	ask := func() *openai.ChatCompletion {
		t.Helper()
		out, err := c.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{
			Model: "habakkuk/habakkuk", Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("once")},
		}, option.WithHeader("Idempotency-Key", "key-1"))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	first, second := ask(), ask()
	if first.ID != second.ID || second.Choices[0].Message.Content != "echo: once" {
		t.Fatalf("retry got %s %q, first %s", second.ID, second.Choices[0].Message.Content, first.ID)
	}
	if n := r.model.count(); n != 1 {
		t.Fatalf("model ran %d times, want 1", n)
	}
}

func TestOpenAIResponses(t *testing.T) {
	r := newRig(t, token)
	ctx := context.Background()
	c := r.client(token)
	first, err := c.Responses.New(ctx, responses.ResponseNewParams{
		Model: "habakkuk/ops", Input: responses.ResponseNewParamsInputUnion{OfString: openai.String("ping")},
		Instructions: openai.String("Be brief."),
	})
	if err != nil {
		t.Fatal(err)
	}
	// With no earlier messages, the instructions lead the new user message.
	if got := first.OutputText(); !strings.Contains(got, "Be brief.") || !strings.HasSuffix(got, "\n\nping") ||
		first.Status != "completed" || !strings.HasPrefix(first.ID, "resp_") {
		t.Fatalf("response %s %s %q", first.ID, first.Status, got)
	}
	// previous_response_id continues the session.
	second, err := c.Responses.New(ctx, responses.ResponseNewParams{
		Model: "habakkuk/ops", PreviousResponseID: openai.String(first.ID),
		Input: responses.ResponseNewParamsInputUnion{OfInputItemList: responses.ResponseInputParam{
			responses.ResponseInputItemParamOfMessage("pong", responses.EasyInputMessageRoleUser),
		}},
	})
	if err != nil || second.OutputText() != "echo: pong" {
		t.Fatalf("second: %v %+v", err, second)
	}
	sid, aerr := r.s.responseSession(ctx, second.ID)
	if aerr != nil {
		t.Fatal(aerr)
	}
	if first1, _ := r.s.responseSession(ctx, first.ID); first1 != sid {
		t.Fatalf("sessions differ: %s %s", first1, sid)
	}
	if n := len(r.transcript(t, sid)); n != 4 {
		t.Fatalf("transcript has %d messages, want 4", n)
	}
	_, err = c.Responses.New(ctx, responses.ResponseNewParams{
		Model: "habakkuk/ops", PreviousResponseID: openai.String("resp_nope"),
		Input: responses.ResponseNewParamsInputUnion{OfString: openai.String("x")},
	})
	apiErr(t, err, http.StatusNotFound, "not found")
}

func TestOpenAIResponsesStream(t *testing.T) {
	r := newRig(t, token)
	stream := r.client(token).Responses.NewStreaming(context.Background(), responses.ResponseNewParams{
		Model: "habakkuk/habakkuk", Input: responses.ResponseNewParamsInputUnion{OfString: openai.String("live")},
	})
	var types []string
	var deltas strings.Builder
	var final responses.Response
	seq := int64(-1)
	for stream.Next() {
		e := stream.Current()
		types = append(types, e.Type)
		if e.SequenceNumber != seq+1 {
			t.Fatalf("sequence %d after %d", e.SequenceNumber, seq)
		}
		seq = e.SequenceNumber
		switch e.Type {
		case "response.output_text.delta":
			deltas.WriteString(e.Delta)
		case "response.completed":
			final = e.Response
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if deltas.String() != "echo: live" || final.OutputText() != "echo: live" || final.Status != "completed" {
		t.Fatalf("deltas %q, final %q %s", deltas.String(), final.OutputText(), final.Status)
	}
	if types[0] != "response.created" || types[len(types)-1] != "response.completed" {
		t.Fatalf("events %v", types)
	}
}
