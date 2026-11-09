package bedrock

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/smithy-go/eventstream"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// The AWS Signature Version 4 test suite's "get-vanilla" case: a fixed
// request, key and time with a published signature.
const (
	vectorAccessKey = "AKIDEXAMPLE"
	vectorSecret    = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	vectorSignature = "5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
)

var vectorTime = time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)

// sigV4 computes a request's signature from first principles, so the
// signature the SDK puts on a real Converse request can be checked
// independently of the SDK.
func sigV4(method, host, path, rawQuery string, headers http.Header, signed []string, payloadHash, secret, region, service string, at time.Time) string {
	var canon strings.Builder
	canon.WriteString(method + "\n" + path + "\n")
	q, _ := url.ParseQuery(rawQuery)
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var qs []string
	for _, k := range keys {
		for _, v := range q[k] {
			qs = append(qs, url.QueryEscape(k)+"="+url.QueryEscape(v))
		}
	}
	canon.WriteString(strings.Join(qs, "&") + "\n")
	for _, h := range signed {
		v := headers.Values(h)
		if h == "host" {
			v = []string{host}
		}
		for i := range v {
			v[i] = strings.Join(strings.Fields(v[i]), " ")
		}
		canon.WriteString(h + ":" + strings.Join(v, ",") + "\n")
	}
	canon.WriteString("\n" + strings.Join(signed, ";") + "\n" + payloadHash)
	scope := at.Format("20060102") + "/" + region + "/" + service + "/aws4_request"
	sum := sha256.Sum256([]byte(canon.String()))
	toSign := "AWS4-HMAC-SHA256\n" + at.Format("20060102T150405Z") + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	mac := func(key []byte, s string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(s))
		return h.Sum(nil)
	}
	k := mac([]byte("AWS4"+secret), at.Format("20060102"))
	k = mac(k, region)
	k = mac(k, service)
	k = mac(k, "aws4_request")
	return hex.EncodeToString(mac(k, toSign))
}

const emptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func TestSigV4FixedVector(t *testing.T) {
	h := http.Header{"X-Amz-Date": {"20150830T123600Z"}}
	got := sigV4("GET", "example.amazonaws.com", "/", "", h, []string{"host", "x-amz-date"}, emptyHash,
		vectorSecret, "us-east-1", "service", vectorTime)
	if got != vectorSignature {
		t.Fatalf("reference signer: %s, want %s", got, vectorSignature)
	}
	// The SDK signer Bedrock uses agrees with the published vector.
	req, _ := http.NewRequest("GET", "https://example.amazonaws.com/", nil)
	err := v4.NewSigner().SignHTTP(context.Background(), aws.Credentials{AccessKeyID: vectorAccessKey, SecretAccessKey: vectorSecret},
		req, emptyHash, "service", "us-east-1", vectorTime)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(req.Header.Get("Authorization"), "Signature="+vectorSignature) {
		t.Fatalf("SDK signature: %s", req.Header.Get("Authorization"))
	}
}

func isolateAWS(t *testing.T) {
	t.Setenv("AWS_CONFIG_FILE", "/nonexistent/habakkuk-test")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/nonexistent/habakkuk-test")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "")
	t.Setenv("AWS_ENDPOINT_URL", "")
	t.Setenv("AWS_ENDPOINT_URL_BEDROCK_RUNTIME", "")
}

func eventStream(t *testing.T, events ...[2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := eventstream.NewEncoder()
	for _, e := range events {
		var h eventstream.Headers
		h.Set(":message-type", eventstream.StringValue("event"))
		h.Set(":event-type", eventstream.StringValue(e[0]))
		h.Set(":content-type", eventstream.StringValue("application/json"))
		if err := enc.Encode(&buf, eventstream.Message{Headers: h, Payload: []byte(e[1])}); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func TestConverseStreamSignedAndParsed(t *testing.T) {
	isolateAWS(t)
	var body map[string]any
	var sigErr string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
		sigErr = checkSignature(r, data)
		if r.URL.Path != "/model/test-model/converse-stream" {
			sigErr += " path " + r.URL.Path
		}
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.Write(eventStream(t,
			[2]string{"messageStart", `{"role":"assistant"}`},
			[2]string{"contentBlockDelta", `{"contentBlockIndex":0,"delta":{"reasoningContent":{"text":"thinking"}}}`},
			[2]string{"contentBlockDelta", `{"contentBlockIndex":0,"delta":{"reasoningContent":{"signature":"SIG"}}}`},
			[2]string{"contentBlockStop", `{"contentBlockIndex":0}`},
			[2]string{"contentBlockDelta", `{"contentBlockIndex":1,"delta":{"text":"Checking "}}`},
			[2]string{"contentBlockDelta", `{"contentBlockIndex":1,"delta":{"text":"now."}}`},
			[2]string{"contentBlockStop", `{"contentBlockIndex":1}`},
			[2]string{"contentBlockStart", `{"contentBlockIndex":2,"start":{"toolUse":{"toolUseId":"tu1","name":"read_file"}}}`},
			[2]string{"contentBlockDelta", `{"contentBlockIndex":2,"delta":{"toolUse":{"input":"{\"path\":"}}}`},
			[2]string{"contentBlockDelta", `{"contentBlockIndex":2,"delta":{"toolUse":{"input":"\"a.txt\"}"}}}`},
			[2]string{"contentBlockStop", `{"contentBlockIndex":2}`},
			[2]string{"messageStop", `{"stopReason":"tool_use"}`},
			[2]string{"metadata", `{"usage":{"inputTokens":100,"outputTokens":20,"totalTokens":620,"cacheReadInputTokens":500,"cacheWriteInputTokens":7},"metrics":{"latencyMs":5}}`},
		))
	}))
	defer srv.Close()

	p := &Provider{Region: "us-east-1", BaseURL: srv.URL,
		Credentials: credentials.NewStaticCredentialsProvider("AKIDTEST", "SECRETTEST", "")}
	req := llm.Request{
		Model: "test-model", System: "be brief", MaxTokens: 1000,
		Tools: []llm.ToolSpec{{Name: "read_file", Description: "read", Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)}},
		Messages: []llm.Message{
			{Role: llm.User, Blocks: []llm.Block{{Type: llm.Text, Text: "hi"}, {Type: llm.Image, Media: "img", MediaType: "image/png"},
				{Type: llm.Document, Media: "doc", MediaType: "application/pdf", Name: "q3 report (final).pdf"}}},
			{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.ToolUse, ToolUseID: "t0", ToolName: "read_file", Input: json.RawMessage(`{"path":"b"}`)}}},
			{Role: llm.User, Blocks: []llm.Block{{Type: llm.ToolResult, ToolUseID: "t0", Text: "nope", IsError: true}}},
		},
		Media: func(ref string) ([]byte, error) { return []byte("bytes-of-" + ref), nil },
	}
	var text strings.Builder
	var calls []string
	resp, err := p.Stream(context.Background(), "", req, func(e llm.Event) {
		switch e.Type {
		case llm.EventText:
			text.WriteString(e.Text)
		case llm.EventToolCall:
			calls = append(calls, e.Text)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if sigErr != "" {
		t.Fatal(sigErr)
	}

	// Request body.
	if body["system"].([]any)[0].(map[string]any)["text"] != "be brief" {
		t.Errorf("system: %v", body["system"])
	}
	if body["inferenceConfig"].(map[string]any)["maxTokens"].(float64) != 1000 {
		t.Errorf("inference config: %v", body["inferenceConfig"])
	}
	spec := body["toolConfig"].(map[string]any)["tools"].([]any)[0].(map[string]any)["toolSpec"].(map[string]any)
	if spec["name"] != "read_file" || spec["inputSchema"].(map[string]any)["json"].(map[string]any)["type"] != "object" {
		t.Errorf("tool spec: %v", spec)
	}
	msgs := body["messages"].([]any)
	first := msgs[0].(map[string]any)["content"].([]any)
	img := first[1].(map[string]any)["image"].(map[string]any)
	doc := first[2].(map[string]any)["document"].(map[string]any)
	if img["format"] != "png" || img["source"].(map[string]any)["bytes"] != "Ynl0ZXMtb2YtaW1n" || doc["name"] != "q3 report (final)" || doc["format"] != "pdf" {
		t.Errorf("media: %v %v", img, doc)
	}
	tr := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)["toolResult"].(map[string]any)
	if tr["toolUseId"] != "t0" || tr["status"] != "error" {
		t.Errorf("tool result: %v", tr)
	}

	// Response.
	if text.String() != "Checking now." || strings.Join(calls, ",") != "read_file" || resp.StopReason != llm.StopToolUse {
		t.Errorf("text %q calls %v stop %s", text.String(), calls, resp.StopReason)
	}
	b := resp.Message.Blocks
	if len(b) != 3 || b[0].Type != llm.Thinking || b[0].Signature != "SIG" || b[0].Provider != providerType ||
		b[2].ToolUseID != "tu1" || string(b[2].Input) != `{"path":"a.txt"}` {
		t.Fatalf("blocks: %+v", b)
	}
	want := llm.Usage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 500, CacheWriteTokens: 7}
	if resp.Usage != want {
		t.Errorf("usage %+v, want %+v", resp.Usage, want)
	}
}

// checkSignature recomputes the request's SigV4 signature and returns a
// description of any mismatch.
func checkSignature(r *http.Request, body []byte) string {
	authz := r.Header.Get("Authorization")
	const prefix = "AWS4-HMAC-SHA256 Credential=AKIDTEST/"
	if !strings.HasPrefix(authz, prefix) {
		return "not SigV4 with the test key: " + authz
	}
	var scope, signedList, sig string
	for _, f := range strings.Split(strings.TrimPrefix(authz, "AWS4-HMAC-SHA256 "), ", ") {
		k, v, _ := strings.Cut(f, "=")
		switch k {
		case "Credential":
			scope = strings.TrimPrefix(v, "AKIDTEST/")
		case "SignedHeaders":
			signedList = v
		case "Signature":
			sig = v
		}
	}
	if !strings.HasSuffix(scope, "/us-east-1/bedrock/aws4_request") {
		return "scope " + scope
	}
	at, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	if err != nil {
		return "x-amz-date: " + err.Error()
	}
	h := r.Header.Clone()
	h.Set("Content-Length", strconv.FormatInt(r.ContentLength, 10))
	sum := sha256.Sum256(body)
	payload := hex.EncodeToString(sum[:])
	if v := r.Header.Get("X-Amz-Content-Sha256"); v != "" && v != payload {
		return "payload hash header does not match the body"
	}
	want := sigV4(r.Method, r.Host, r.URL.EscapedPath(), r.URL.RawQuery, h, strings.Split(signedList, ";"), payload,
		"SECRETTEST", "us-east-1", "bedrock", at)
	if sig != want {
		return fmt.Sprintf("signature %s, recomputed %s (signed %s)", sig, want, signedList)
	}
	return ""
}

func TestBedrockAPIKeyUsesBearer(t *testing.T) {
	isolateAWS(t)
	var authz string
	// Bearer tokens are only sent over HTTPS.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authz = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.Write(eventStream(t, [2]string{"messageStop", `{"stopReason":"end_turn"}`}))
	}))
	defer srv.Close()
	p := &Provider{Region: "us-west-2", BaseURL: srv.URL, HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("A", "S", "")}
	if _, err := p.Stream(context.Background(), "BEDROCK-KEY", llm.Request{Model: "m", Messages: []llm.Message{llm.UserText("hi")}}, func(llm.Event) {}); err != nil {
		t.Fatal(err)
	}
	if authz != "Bearer BEDROCK-KEY" {
		t.Fatalf("Authorization = %q", authz)
	}
}

func TestBedrockErrorClasses(t *testing.T) {
	isolateAWS(t)
	for _, tc := range []struct {
		status int
		kind   string
		msg    string
		want   llm.ErrorKind
	}{
		{429, "ThrottlingException", "Too many requests", llm.ErrRateLimit},
		{503, "ServiceUnavailableException", "busy", llm.ErrOverloaded},
		{403, "AccessDeniedException", "no access to model", llm.ErrAuth},
		{400, "ValidationException", "Input is too long for requested model.", llm.ErrContextOverflow},
		{400, "ValidationException", "bad field", llm.ErrBadRequest},
		{404, "ResourceNotFoundException", "no such model", llm.ErrNotFound},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Amzn-ErrorType", tc.kind)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(tc.status)
			fmt.Fprintf(w, `{"message":%q}`, tc.msg)
		}))
		p := &Provider{Region: "us-east-1", BaseURL: srv.URL, Credentials: credentials.NewStaticCredentialsProvider("A", "S", "")}
		_, err := p.Stream(context.Background(), "", llm.Request{Model: "m", Messages: []llm.Message{llm.UserText("hi")}}, func(llm.Event) {})
		srv.Close()
		if got := llm.Classify(err).Kind; got != tc.want {
			t.Errorf("%s: %s, want %s (%v)", tc.kind, got, tc.want, err)
		}
	}
}

func TestDocumentName(t *testing.T) {
	for in, want := range map[string]string{"q3 report (final).pdf": "q3 report (final)", "a__b..c.pdf": "a b c", "%%%": "document"} {
		if got := documentName(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}
