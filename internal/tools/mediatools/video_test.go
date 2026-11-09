package mediatools

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeMP4 starts like an MP4 file.
var fakeMP4 = []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00moov")

func TestOpenAIVideo(t *testing.T) {
	var mu sync.Mutex
	polls := 0
	form := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk" {
			http.Error(w, "auth", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/videos":
			r.ParseMultipartForm(1 << 20)
			for k, v := range r.MultipartForm.Value {
				form[k] = v[0]
			}
			if fh := r.MultipartForm.File["input_reference"]; len(fh) == 1 {
				form["image"] = fh[0].Header.Get("Content-Type")
			}
			w.Write([]byte(`{"id":"video_1","status":"queued"}`))
		case r.URL.Path == "/v1/videos/video_1":
			polls++
			if polls < 3 {
				w.Write([]byte(`{"id":"video_1","status":"in_progress"}`))
				return
			}
			w.Write([]byte(`{"id":"video_1","status":"completed"}`))
		case r.URL.Path == "/v1/videos/video_1/content":
			w.Write(fakeMP4)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	env := testEnv(t)
	os.WriteFile(filepath.Join(env.Workspace, "start.png"), pngBytes(t, 4, 4), 0o644)
	gen := &VideoGen{Provider: "openai", Key: "sk", BaseURL: srv.URL + "/v1", Size: "1280x720", Seconds: 8, PollEvery: time.Millisecond}
	res, _ := run(t, env, videoGenerateTool{gen: gen}, `{"prompt":"A paper boat in the rain","seconds":11,"image":"start.png"}`)
	if res.IsError {
		t.Fatal(res.Text)
	}
	mu.Lock()
	defer mu.Unlock()
	if form["model"] != "sora-2" || form["seconds"] != "12" || form["size"] != "1280x720" || form["image"] != "image/png" || polls != 3 {
		t.Fatalf("form = %v, polls = %d", form, polls)
	}
	files, _ := filepath.Glob(filepath.Join(env.Workspace, "videos", "*-a-paper-boat-in-the-rain.mp4"))
	if len(files) != 1 || !strings.Contains(res.Text, files[0]) || !strings.Contains(res.Text, "send_file") {
		t.Fatalf("files %v, result %q", files, res.Text)
	}
	if data, _ := os.ReadFile(files[0]); string(data) != string(fakeMP4) {
		t.Fatal("video bytes differ")
	}
}

func TestOpenAIVideoFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Write([]byte(`{"id":"v2"}`))
			return
		}
		w.Write([]byte(`{"status":"failed","error":{"message":"moderation_blocked"}}`))
	}))
	defer srv.Close()
	gen := &VideoGen{Provider: "openai", Key: "sk", BaseURL: srv.URL, PollEvery: time.Millisecond}
	res, _ := run(t, testEnv(t), videoGenerateTool{gen: gen}, `{"prompt":"x"}`)
	if !res.IsError || !strings.Contains(res.Text, "moderation_blocked") {
		t.Fatalf("result = %+v", res)
	}
}

func TestGeminiVeoOperation(t *testing.T) {
	var mu sync.Mutex
	var submitted map[string]any
	polls := 0
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "gk" {
			http.Error(w, "auth", http.StatusForbidden)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/v1beta/models/veo-3.0-generate-001:predictLongRunning":
			json.NewDecoder(r.Body).Decode(&submitted)
			w.Write([]byte(`{"name":"models/veo-3.0-generate-001/operations/op1"}`))
		case "/v1beta/models/veo-3.0-generate-001/operations/op1":
			polls++
			if polls == 1 {
				w.Write([]byte(`{"name":"op1"}`))
				return
			}
			w.Write([]byte(`{"name":"op1","done":true,"response":{"generateVideoResponse":{"generatedSamples":[{"video":{"uri":"` +
				srv.URL + `/v1beta/files/f1:download?alt=media"}}]}}}`))
		case "/v1beta/files/f1:download":
			w.Write(fakeMP4)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	gen := &VideoGen{Provider: "gemini", Key: "gk", BaseURL: srv.URL + "/v1beta", Size: "720x1280", PollEvery: time.Millisecond}
	res, _ := run(t, testEnv(t), videoGenerateTool{gen: gen}, `{"prompt":"a cat surfing","seconds":6}`)
	if res.IsError {
		t.Fatal(res.Text)
	}
	mu.Lock()
	defer mu.Unlock()
	params := submitted["parameters"].(map[string]any)
	inst := submitted["instances"].([]any)[0].(map[string]any)
	if params["durationSeconds"] != float64(6) || params["aspectRatio"] != "9:16" || inst["prompt"] != "a cat surfing" {
		t.Fatalf("submitted = %v", submitted)
	}
}

func TestGeminiVeoFiltered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Write([]byte(`{"name":"operations/op2"}`))
			return
		}
		w.Write([]byte(`{"done":true,"response":{"generateVideoResponse":{"raiMediaFilteredCount":1,"raiMediaFilteredReasons":["celebrity"]}}}`))
	}))
	defer srv.Close()
	gen := &VideoGen{Provider: "gemini", Key: "gk", BaseURL: srv.URL, PollEvery: time.Millisecond}
	if _, err := gen.Generate(t.Context(), "x", 0, nil); err == nil || !strings.Contains(err.Error(), "filtered: celebrity") {
		t.Fatalf("err = %v", err)
	}
}

func TestFalVideoQueue(t *testing.T) {
	var mu sync.Mutex
	var body map[string]any
	var path string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Key fk" && !strings.HasPrefix(r.URL.Path, "/cdn/") {
			http.Error(w, "auth", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost:
			path = r.URL.Path
			raw, _ := io.ReadAll(r.Body)
			json.Unmarshal(raw, &body)
			w.Write([]byte(`{"request_id":"r1","status_url":"` + srv.URL + `/q/r1/status","response_url":"` + srv.URL + `/q/r1"}`))
		case r.URL.Path == "/q/r1/status":
			w.Write([]byte(`{"status":"COMPLETED"}`))
		case r.URL.Path == "/q/r1":
			w.Write([]byte(`{"video":{"url":"` + srv.URL + `/cdn/v.mp4"}}`))
		case r.URL.Path == "/cdn/v.mp4":
			w.Write(fakeMP4)
		}
	}))
	defer srv.Close()
	env := testEnv(t)
	os.WriteFile(filepath.Join(env.Workspace, "a.png"), pngBytes(t, 4, 4), 0o644)
	gen := &VideoGen{Provider: "fal", Key: "fk", BaseURL: srv.URL, Size: "16:9", Seconds: 8, PollEvery: time.Millisecond}
	res, _ := run(t, env, videoGenerateTool{gen: gen}, `{"prompt":"waves","image":"a.png"}`)
	if res.IsError {
		t.Fatal(res.Text)
	}
	mu.Lock()
	defer mu.Unlock()
	if path != "/fal-ai/veo3/fast/image-to-video" || body["duration"] != "8s" || body["aspect_ratio"] != "16:9" ||
		!strings.HasPrefix(body["image_url"].(string), "data:image/png;base64,") {
		t.Fatalf("path %s body %v", path, body)
	}
}

func TestVideoTimeoutAndKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Write([]byte(`{"id":"slow"}`))
			return
		}
		w.Write([]byte(`{"status":"in_progress"}`))
	}))
	defer srv.Close()
	gen := &VideoGen{Provider: "openai", Key: "sk", BaseURL: srv.URL, PollEvery: 5 * time.Millisecond, Timeout: 50 * time.Millisecond}
	if _, err := gen.Generate(t.Context(), "x", 4, nil); err == nil || !strings.Contains(err.Error(), "no video after") {
		t.Fatalf("err = %v", err)
	}
	gen = &VideoGen{Provider: "openai", KeyEnv: "OPENAI_API_KEY"}
	if _, err := gen.Generate(t.Context(), "x", 4, nil); err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Fatalf("err = %v", err)
	}
	if got := videoType([]byte("\x1a\x45\xdf\xa3webm")); got != "video/webm" {
		t.Fatalf("webm = %s", got)
	}
	if nearest(11, 4, 8, 12) != 12 || nearest(5, 4, 8, 12) != 4 || nearest(10, 4, 8, 12) != 8 {
		t.Fatal("nearest")
	}
}
