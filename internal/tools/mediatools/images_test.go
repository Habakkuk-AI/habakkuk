package mediatools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.NRGBA{uint8(x), uint8(y), 200, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// testEnv is a workspace with a blob store, in workspace mode.
func testEnv(t *testing.T) *tools.Env {
	t.Helper()
	dir := t.TempDir()
	ws := filepath.Join(dir, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	blobs, err := store.NewBlobs(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	return &tools.Env{Workspace: ws, Mode: tools.ModeWorkspace, Blobs: blobs, BlobDir: filepath.Join(dir, "blobs")}
}

func run(t *testing.T, env *tools.Env, tool tools.Tool, input string) (llm.Block, []llm.Block) {
	t.Helper()
	r := tools.NewRegistry()
	r.Register("core", tool)
	return r.ExecuteMedia(context.Background(), env, llm.Block{Type: llm.ToolUse, ToolUseID: "c1",
		ToolName: tool.Spec().Name, Input: json.RawMessage(input)})
}

func TestOpenAIImageGenerate(t *testing.T) {
	img := pngBytes(t, 8, 8)
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/generations" || r.Header.Get("Authorization") != "Bearer sk" {
			http.Error(w, "wrong "+r.URL.Path, 400)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		b64 := base64.StdEncoding.EncodeToString(img)
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"b64_json": b64}, {"b64_json": b64}}})
	}))
	defer srv.Close()
	gen := &ImageGen{Provider: "openai", Key: "sk", BaseURL: srv.URL + "/v1"}
	env := testEnv(t)
	res, media := run(t, env, imageGenerateTool{gen: gen}, `{"prompt":"A red fox, watercolor","size":"1024x1536","n":2}`)
	if res.IsError {
		t.Fatal(res.Text)
	}
	if got["model"] != "gpt-image-2" || got["prompt"] != "A red fox, watercolor" || got["size"] != "1024x1536" || got["n"] != float64(2) {
		t.Fatalf("request: %v", got)
	}
	if _, ok := got["response_format"]; ok {
		t.Fatal("gpt-image models reject response_format")
	}
	if len(media) != 2 || media[0].Type != llm.Image || media[0].MediaType != "image/png" || !strings.HasPrefix(media[0].Media, "sha256:") {
		t.Fatalf("media: %+v", media)
	}
	files, _ := filepath.Glob(filepath.Join(env.Workspace, "images", "*-a-red-fox-watercolor-*.png"))
	if len(files) != 2 {
		t.Fatalf("saved files: %v\n%s", files, res.Text)
	}
	if data, _ := os.ReadFile(files[0]); !bytes.Equal(data, img) {
		t.Fatal("saved image differs")
	}
	if !strings.Contains(res.Text, files[0]) || !strings.Contains(res.Text, "send_file") {
		t.Fatalf("result: %s", res.Text)
	}
}

func TestOpenAIImageEdit(t *testing.T) {
	img := pngBytes(t, 4, 4)
	var fields map[string]string
	var files []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/images/edits" {
			http.Error(w, "wrong path", 400)
			return
		}
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		fields = map[string]string{}
		for k, v := range r.MultipartForm.Value {
			fields[k] = v[0]
		}
		for _, fh := range r.MultipartForm.File["image[]"] {
			files = append(files, fh.Filename+":"+fh.Header.Get("Content-Type"))
		}
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(img)}}})
	}))
	defer srv.Close()
	env := testEnv(t)
	os.WriteFile(filepath.Join(env.Workspace, "cat.png"), img, 0o644)
	os.WriteFile(filepath.Join(env.Workspace, "hat.png"), img, 0o644)
	gen := &ImageGen{Provider: "openai", Key: "sk", BaseURL: srv.URL}
	res, media := run(t, env, imageGenerateTool{gen: gen}, `{"prompt":"Put the hat on the cat","edit_from":["cat.png","hat.png"]}`)
	if res.IsError {
		t.Fatal(res.Text)
	}
	if fields["prompt"] != "Put the hat on the cat" || fields["model"] != "gpt-image-2" || fields["n"] != "1" {
		t.Fatalf("fields: %v", fields)
	}
	if len(files) != 2 || files[0] != "cat.png:image/png" {
		t.Fatalf("files: %v", files)
	}
	if len(media) != 1 {
		t.Fatalf("media: %d", len(media))
	}
}

func TestImageEditPolicy(t *testing.T) {
	env := testEnv(t)
	outside := filepath.Join(filepath.Dir(env.Workspace), "private.png")
	os.WriteFile(outside, pngBytes(t, 2, 2), 0o644)
	os.WriteFile(filepath.Join(env.Workspace, "notes.txt"), []byte("hello"), 0o644)
	gen := &ImageGen{Provider: "openai", Key: "sk", BaseURL: "http://127.0.0.1:1"}
	res, _ := run(t, env, imageGenerateTool{gen: gen}, `{"prompt":"x","edit_from":[`+strconv.Quote(outside)+`]}`)
	if !res.IsError || !strings.Contains(res.Text, "outside the workspace") {
		t.Fatalf("outside: %s", res.Text)
	}
	res, _ = run(t, env, imageGenerateTool{gen: gen}, `{"prompt":"x","edit_from":"notes.txt"}`)
	if !res.IsError || !strings.Contains(res.Text, "text/plain") {
		t.Fatalf("not an image: %s", res.Text)
	}
	res, _ = run(t, env, imageGenerateTool{gen: &ImageGen{Provider: "fal", KeyEnv: "FAL_KEY"}}, `{"prompt":"x"}`)
	if !res.IsError || !strings.Contains(res.Text, "FAL_KEY") {
		t.Fatalf("missing key: %s", res.Text)
	}
}

func TestGeminiImageGenerate(t *testing.T) {
	img := pngBytes(t, 4, 4)
	var calls int
	var body struct {
		Contents []struct {
			Parts []struct {
				Text       string `json:"text"`
				InlineData *struct {
					MimeType string `json:"mime_type"`
					Data     string `json:"data"`
				} `json:"inline_data"`
			} `json:"parts"`
		} `json:"contents"`
		GenerationConfig struct {
			ResponseModalities []string          `json:"responseModalities"`
			ImageConfig        map[string]string `json:"imageConfig"`
		} `json:"generationConfig"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1beta/models/gemini-2.5-flash-image:generateContent" || r.Header.Get("x-goog-api-key") != "gk" {
			http.Error(w, "wrong "+r.URL.Path, 400)
			return
		}
		json.NewDecoder(r.Body).Decode(&body)
		json.NewEncoder(w).Encode(map[string]any{"candidates": []any{map[string]any{
			"content": map[string]any{"parts": []any{
				map[string]any{"text": "Here you go"},
				map[string]any{"inlineData": map[string]string{"mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(img)}},
			}},
			"finishReason": "STOP",
		}}})
	}))
	defer srv.Close()
	gen := &ImageGen{Provider: "gemini", Key: "gk", BaseURL: srv.URL + "/v1beta"}
	pics, err := gen.Generate(context.Background(), "a lighthouse", "1536x1024", 2,
		[]inputImage{{Name: "ref.png", MediaType: "image/png", Data: img}})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(pics) != 2 || pics[0].MediaType != "image/png" {
		t.Fatalf("calls %d, pics %d", calls, len(pics))
	}
	parts := body.Contents[0].Parts
	if parts[0].Text != "a lighthouse" || parts[1].InlineData == nil || parts[1].InlineData.MimeType != "image/png" {
		t.Fatalf("parts: %+v", parts)
	}
	if body.GenerationConfig.ImageConfig["aspectRatio"] != "3:2" || len(body.GenerationConfig.ResponseModalities) != 2 {
		t.Fatalf("config: %+v", body.GenerationConfig)
	}
}

func TestGeminiRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"I can't make that."}]},"finishReason":"IMAGE_SAFETY"}]}`)
	}))
	defer srv.Close()
	gen := &ImageGen{Provider: "gemini", Key: "gk", BaseURL: srv.URL}
	_, err := gen.Generate(context.Background(), "x", "", 1, nil)
	if err == nil || !strings.Contains(err.Error(), "IMAGE_SAFETY") || !strings.Contains(err.Error(), "can't make that") {
		t.Fatalf("err: %v", err)
	}
}

func TestFalImageQueue(t *testing.T) {
	img := pngBytes(t, 4, 4)
	var mu sync.Mutex
	polls := 0
	var submitted map[string]any
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if !strings.HasPrefix(r.URL.Path, "/files/") && r.Header.Get("Authorization") != "Key fk" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/fal-ai/flux/dev":
			json.NewDecoder(r.Body).Decode(&submitted)
			json.NewEncoder(w).Encode(map[string]string{"request_id": "req1",
				"status_url": srv.URL + "/fal-ai/flux/requests/req1/status", "response_url": srv.URL + "/fal-ai/flux/requests/req1"})
		case r.URL.Path == "/fal-ai/flux/requests/req1/status":
			polls++
			status := "IN_PROGRESS"
			if polls >= 3 {
				status = "COMPLETED"
			}
			json.NewEncoder(w).Encode(map[string]string{"status": status})
		case r.URL.Path == "/fal-ai/flux/requests/req1":
			json.NewEncoder(w).Encode(map[string]any{"images": []map[string]string{
				{"url": srv.URL + "/files/a.png", "content_type": "image/png"},
				{"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(img)},
			}})
		case r.URL.Path == "/files/a.png":
			w.Write(img)
		default:
			http.Error(w, "unknown "+r.URL.Path, 404)
		}
	}))
	defer srv.Close()
	gen := &ImageGen{Provider: "fal", Key: "fk", BaseURL: srv.URL, PollEvery: time.Millisecond}
	pics, err := gen.Generate(context.Background(), "a moon base", "1280x720", 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pics) != 2 || !bytes.Equal(pics[0].Data, img) || !bytes.Equal(pics[1].Data, img) {
		t.Fatalf("pics: %d", len(pics))
	}
	mu.Lock()
	defer mu.Unlock()
	if polls != 3 || submitted["prompt"] != "a moon base" || submitted["num_images"] != float64(2) || submitted["output_format"] != "png" {
		t.Fatalf("polls %d, submitted %v", polls, submitted)
	}
	if size, _ := submitted["image_size"].(map[string]any); size["width"] != float64(1280) || size["height"] != float64(720) {
		t.Fatalf("size: %v", submitted["image_size"])
	}
}

func TestFalEditUsesEditModel(t *testing.T) {
	var path string
	var submitted map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			path = r.URL.Path
			json.NewDecoder(r.Body).Decode(&submitted)
		}
		http.Error(w, `{"detail":"stop here"}`, http.StatusUnprocessableEntity)
	}))
	defer srv.Close()
	gen := &ImageGen{Provider: "fal", Key: "fk", BaseURL: srv.URL, EditModel: "fal-ai/some/edit"}
	_, err := gen.Generate(context.Background(), "make it blue", "", 1, []inputImage{{MediaType: "image/png", Data: []byte("PNG")}})
	if err == nil || !strings.Contains(err.Error(), "422") {
		t.Fatalf("err: %v", err)
	}
	urls, _ := submitted["image_urls"].([]any)
	if path != "/fal-ai/some/edit" || len(urls) != 1 || !strings.HasPrefix(urls[0].(string), "data:image/png;base64,") {
		t.Fatalf("path %s, urls %v", path, urls)
	}
}

func TestAspectRatio(t *testing.T) {
	for in, want := range map[string]string{"1024x1024": "1:1", "1920x1080": "16:9", "1024x1536": "2:3", "9:16": "9:16", "auto": "", "": ""} {
		if got := aspectRatio(in); got != want {
			t.Errorf("%s: got %s, want %s", in, got, want)
		}
	}
}
