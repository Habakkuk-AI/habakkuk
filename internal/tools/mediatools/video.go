package mediatools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// maxVideoBytes caps a downloaded video.
const maxVideoBytes = 500 << 20

// VideoGen makes short videos with one provider. Every provider works the
// same way: submit, poll until done, download.
type VideoGen struct {
	Provider   string // openai | gemini | fal
	Key        string
	KeyEnv     string
	BaseURL    string
	Model      string
	ImageModel string // fal: model when a start image is given
	Size       string // openai: WIDTHxHEIGHT; others: an aspect ratio
	Seconds    int    // default length
	Timeout    time.Duration
	HTTP       *http.Client
	// PollEvery is how often a job is checked; default 5s.
	PollEvery time.Duration
}

// Clip is one generated video.
type Clip struct {
	Data      []byte
	MediaType string
}

// Generate makes a video from prompt, starting from image when given.
func (g *VideoGen) Generate(ctx context.Context, prompt string, seconds int, image *inputImage) (Clip, error) {
	if g.Key == "" {
		return Clip{}, missingKey(g.Provider+" video", g.KeyEnv)
	}
	if seconds <= 0 {
		seconds = g.Seconds
	}
	if seconds <= 0 {
		seconds = 8
	}
	timeout := g.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var (
		data []byte
		err  error
	)
	switch g.Provider {
	case "openai":
		data, err = g.openai(ctx, prompt, seconds, image)
	case "gemini":
		data, err = g.gemini(ctx, prompt, seconds, image)
	case "fal":
		data, err = g.fal(ctx, prompt, seconds, image)
	default:
		return Clip{}, fmt.Errorf("unknown video provider %q", g.Provider)
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("no video after %s; try a shorter clip, or raise video.timeout: %w", timeout, err)
		}
		return Clip{}, fmt.Errorf("%s video: %w", g.Provider, err)
	}
	if len(data) == 0 {
		return Clip{}, fmt.Errorf("%s video: the provider returned an empty file", g.Provider)
	}
	return Clip{Data: data, MediaType: videoType(data)}, nil
}

func (g *VideoGen) client() *http.Client {
	if g.HTTP != nil {
		return g.HTTP
	}
	return newClient(10 * time.Minute)
}

func (g *VideoGen) every() time.Duration {
	if g.PollEvery > 0 {
		return g.PollEvery
	}
	return 5 * time.Second
}

// poll calls check until it reports done, waiting between calls.
func (g *VideoGen) poll(ctx context.Context, check func() (bool, error)) error {
	for {
		done, err := check()
		if err != nil || done {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(g.every()):
		}
	}
}

// openai: POST /videos (multipart, with input_reference for a start
// image), poll GET /videos/{id}, then GET /videos/{id}/content.
func (g *VideoGen) openai(ctx context.Context, prompt string, seconds int, image *inputImage) ([]byte, error) {
	base := strings.TrimRight(firstNonEmpty(g.BaseURL, "https://api.openai.com/v1"), "/")
	model := firstNonEmpty(g.Model, "sora-2")
	if strings.HasPrefix(model, "sora") {
		seconds = nearest(seconds, 4, 8, 12)
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fields := map[string]string{"model": model, "prompt": prompt, "seconds": strconv.Itoa(seconds)}
	if g.Size != "" {
		fields["size"] = g.Size
	}
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			return nil, err
		}
	}
	if image != nil {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="input_reference"; filename="%s"`, image.Name))
		h.Set("Content-Type", image.MediaType)
		fw, err := w.CreatePart(h)
		if err != nil {
			return nil, err
		}
		if _, err := fw.Write(image.Data); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	auth := http.Header{"Authorization": {"Bearer " + g.Key}}
	req, err := http.NewRequest(http.MethodPost, base+"/videos", &buf)
	if err != nil {
		return nil, err
	}
	req.Header = auth.Clone()
	req.Header.Set("Content-Type", w.FormDataContentType())
	var job struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := doJSON(ctx, g.client(), req, &job); err != nil {
		return nil, err
	}
	if job.ID == "" {
		return nil, errors.New("the API answered without a video id")
	}
	err = g.poll(ctx, func() (bool, error) {
		req, err := http.NewRequest(http.MethodGet, base+"/videos/"+job.ID, nil)
		if err != nil {
			return false, err
		}
		req.Header = auth.Clone()
		var st struct {
			Status string `json:"status"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := doJSON(ctx, g.client(), req, &st); err != nil {
			return false, fmt.Errorf("status: %w", err)
		}
		switch st.Status {
		case "completed":
			return true, nil
		case "failed", "cancelled":
			msg := st.Status
			if st.Error != nil && st.Error.Message != "" {
				msg += ": " + st.Error.Message
			}
			return false, errors.New(msg)
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	req, err = http.NewRequest(http.MethodGet, base+"/videos/"+job.ID+"/content", nil)
	if err != nil {
		return nil, err
	}
	req.Header = auth.Clone()
	return doRaw(ctx, g.client(), req, maxVideoBytes)
}

// gemini: Veo through the Gemini API's long-running operations:
// POST models/{model}:predictLongRunning, poll the operation until done,
// then download the generated sample's file.
func (g *VideoGen) gemini(ctx context.Context, prompt string, seconds int, image *inputImage) ([]byte, error) {
	base := strings.TrimRight(firstNonEmpty(g.BaseURL, "https://generativelanguage.googleapis.com/v1beta"), "/")
	model := firstNonEmpty(g.Model, "veo-3.0-generate-001")
	instance := map[string]any{"prompt": prompt}
	if image != nil {
		instance["image"] = map[string]string{"bytesBase64Encoded": base64.StdEncoding.EncodeToString(image.Data), "mimeType": image.MediaType}
	}
	params := map[string]any{"durationSeconds": seconds}
	if ar := videoAspect(g.Size); ar != "" {
		params["aspectRatio"] = ar
	}
	raw, _ := json.Marshal(map[string]any{"instances": []any{instance}, "parameters": params})
	auth := http.Header{"x-goog-api-key": {g.Key}}
	req, err := http.NewRequest(http.MethodPost, base+"/models/"+model+":predictLongRunning", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header = auth.Clone()
	req.Header.Set("Content-Type", "application/json")
	var op struct {
		Name string `json:"name"`
	}
	if err := doJSON(ctx, g.client(), req, &op); err != nil {
		return nil, err
	}
	if op.Name == "" {
		return nil, errors.New("the API answered without an operation name")
	}
	var uri string
	err = g.poll(ctx, func() (bool, error) {
		req, err := http.NewRequest(http.MethodGet, base+"/"+strings.TrimLeft(op.Name, "/"), nil)
		if err != nil {
			return false, err
		}
		req.Header = auth.Clone()
		var st struct {
			Done  bool `json:"done"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
			Response struct {
				Video struct {
					Samples []struct {
						Video struct {
							URI string `json:"uri"`
						} `json:"video"`
					} `json:"generatedSamples"`
					Filtered int      `json:"raiMediaFilteredCount"`
					Reasons  []string `json:"raiMediaFilteredReasons"`
				} `json:"generateVideoResponse"`
			} `json:"response"`
		}
		if err := doJSON(ctx, g.client(), req, &st); err != nil {
			return false, fmt.Errorf("operation: %w", err)
		}
		if !st.Done {
			return false, nil
		}
		if st.Error != nil {
			return false, errors.New(st.Error.Message)
		}
		v := st.Response.Video
		if len(v.Samples) == 0 || v.Samples[0].Video.URI == "" {
			if v.Filtered > 0 {
				return false, fmt.Errorf("the video was filtered: %s", strings.Join(v.Reasons, "; "))
			}
			return false, errors.New("the operation finished without a video")
		}
		uri = v.Samples[0].Video.URI
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	req, err = http.NewRequest(http.MethodGet, uri, nil)
	if err != nil {
		return nil, err
	}
	req.Header = auth.Clone()
	return doRaw(ctx, g.client(), req, maxVideoBytes)
}

// fal: submit to the queue, poll the status URL, read the result and
// download its video.
func (g *VideoGen) fal(ctx context.Context, prompt string, seconds int, image *inputImage) ([]byte, error) {
	base := strings.TrimRight(firstNonEmpty(g.BaseURL, "https://queue.fal.run"), "/")
	model := firstNonEmpty(g.Model, "fal-ai/veo3/fast")
	body := map[string]any{"prompt": prompt}
	if image != nil {
		model = firstNonEmpty(g.ImageModel, "fal-ai/veo3/fast/image-to-video")
		body["image_url"] = "data:" + image.MediaType + ";base64," + base64.StdEncoding.EncodeToString(image.Data)
	}
	// Veo models on fal take "8s"; most others a plain number.
	if strings.Contains(model, "veo") {
		body["duration"] = fmt.Sprintf("%ds", seconds)
	} else {
		body["duration"] = strconv.Itoa(seconds)
	}
	if ar := videoAspect(g.Size); ar != "" {
		body["aspect_ratio"] = ar
	}
	raw, _ := json.Marshal(body)
	auth := http.Header{"Authorization": {"Key " + g.Key}}
	req, err := http.NewRequest(http.MethodPost, base+"/"+strings.Trim(model, "/"), bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header = auth.Clone()
	req.Header.Set("Content-Type", "application/json")
	var sub struct {
		RequestID   string `json:"request_id"`
		StatusURL   string `json:"status_url"`
		ResponseURL string `json:"response_url"`
	}
	if err := doJSON(ctx, g.client(), req, &sub); err != nil {
		return nil, err
	}
	if sub.StatusURL == "" || sub.ResponseURL == "" {
		if sub.RequestID == "" {
			return nil, errors.New("the queue answered without a request id")
		}
		parts := strings.Split(strings.Trim(model, "/"), "/")
		app := strings.Join(parts[:min(2, len(parts))], "/")
		sub.ResponseURL = base + "/" + app + "/requests/" + sub.RequestID
		sub.StatusURL = sub.ResponseURL + "/status"
	}
	err = g.poll(ctx, func() (bool, error) {
		req, err := http.NewRequest(http.MethodGet, sub.StatusURL, nil)
		if err != nil {
			return false, err
		}
		req.Header = auth.Clone()
		var st struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		if err := doJSON(ctx, g.client(), req, &st); err != nil {
			return false, fmt.Errorf("queue status: %w", err)
		}
		if st.Error != "" {
			return false, errors.New(st.Error)
		}
		return st.Status == "COMPLETED", nil
	})
	if err != nil {
		return nil, err
	}
	req, err = http.NewRequest(http.MethodGet, sub.ResponseURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header = auth.Clone()
	var res struct {
		Video *struct {
			URL string `json:"url"`
		} `json:"video"`
		Detail json.RawMessage `json:"detail"`
	}
	if err := doJSON(ctx, g.client(), req, &res); err != nil {
		return nil, fmt.Errorf("queue result: %w", err)
	}
	if res.Video == nil || res.Video.URL == "" {
		if len(res.Detail) > 0 {
			return nil, fmt.Errorf("the model failed: %s", snippet(res.Detail))
		}
		return nil, errors.New("the result has no video")
	}
	req, err = http.NewRequest(http.MethodGet, res.Video.URL, nil)
	if err != nil {
		return nil, err
	}
	return doRaw(ctx, g.client(), req, maxVideoBytes)
}

// nearest picks the allowed value closest to n.
func nearest(n int, allowed ...int) int {
	best := allowed[0]
	for _, a := range allowed {
		if math.Abs(float64(a-n)) < math.Abs(float64(best-n)) {
			best = a
		}
	}
	return best
}

// videoAspect turns a size or ratio into 16:9 or 9:16, the shapes video
// models take.
func videoAspect(size string) string {
	size = strings.TrimSpace(size)
	switch size {
	case "":
		return ""
	case "16:9", "9:16", "1:1":
		return size
	}
	if w, h, ok := parseSize(size); ok {
		if h > w {
			return "9:16"
		}
		return "16:9"
	}
	return ""
}

// videoType sniffs the container: MP4/QuickTime ("ftyp") or WebM.
func videoType(data []byte) string {
	switch {
	case len(data) >= 12 && string(data[4:8]) == "ftyp":
		if string(data[8:10]) == "qt" {
			return "video/quicktime"
		}
		return "video/mp4"
	case len(data) >= 4 && bytes.Equal(data[:4], []byte{0x1a, 0x45, 0xdf, 0xa3}):
		return "video/webm"
	}
	return "application/octet-stream"
}

// video_generate

type videoGenerateTool struct{ gen *VideoGen }

// Risk is network: the prompt (and a start image) go to the provider. The
// video is written into the workspace only.
func (videoGenerateTool) Risk() tools.Risk { return tools.RiskNetwork }
func (t videoGenerateTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "video_generate",
		Description: `Generate a short video from a text prompt, optionally starting from an image. It takes a minute or several; the call waits until the video is ready. ` +
			`The video is saved in your workspace (videos/); send it to the user with send_file. "seconds" is the length (default ` + strconv.Itoa(max(t.gen.Seconds, 1)) + `; models support only a few lengths, such as 4, 8 or 12). ` +
			`"image" is a workspace image path to animate. Describe subject, action, camera movement, style, lighting and any sound.`,
		Schema: json.RawMessage(`{"type":"object","properties":{
			"prompt":{"type":"string"},
			"seconds":{"type":"integer","minimum":1,"maximum":60},
			"image":{"type":"string","description":"workspace image path to start from"}
		},"required":["prompt"]}`),
	}
}

func (t videoGenerateTool) Run(ctx context.Context, env *tools.Env, input json.RawMessage) (string, error) {
	in, err := decode[struct {
		Prompt  string `json:"prompt"`
		Seconds int    `json:"seconds"`
		Image   string `json:"image"`
	}](input)
	if err != nil {
		return "", err
	}
	in.Prompt = strings.TrimSpace(in.Prompt)
	if in.Prompt == "" {
		return "", errors.New("prompt is required")
	}
	if in.Seconds < 0 || in.Seconds > 60 {
		return "", errors.New("seconds must be between 1 and 60")
	}
	var img *inputImage
	if p := strings.TrimSpace(in.Image); p != "" {
		im, err := readInputImage(env, p)
		if err != nil {
			return "", err
		}
		img = &im
	}
	clip, err := t.gen.Generate(ctx, in.Prompt, in.Seconds, img)
	if err != nil {
		return "", err
	}
	ext := map[string]string{"video/mp4": ".mp4", "video/quicktime": ".mov", "video/webm": ".webm"}[clip.MediaType]
	if ext == "" {
		return "", fmt.Errorf("the provider returned %s, not a video", clip.MediaType)
	}
	dir, err := env.OutputDir("videos")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if dir, err = env.OutputDir(dir); err != nil {
		return "", err
	}
	path := filepath.Join(dir, time.Now().UTC().Format("20060102-150405")+"-"+slugify(in.Prompt)+ext)
	if err := store.WriteFileAtomic(path, clip.Data, 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("Generated a video with %s: %s (%s, %s). Send it to the user with send_file; you cannot watch it yourself.",
		t.gen.Provider, path, clip.MediaType, humanSize(int64(len(clip.Data)))), nil
}
