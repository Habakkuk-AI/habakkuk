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
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
	"github.com/habakkuk-ai/habakkuk/internal/tools/media"
)

const (
	maxImages    = 4        // per call
	maxEditInput = 20 << 20 // per input image
)

// ImageGen makes images with one provider.
type ImageGen struct {
	Provider  string // openai | gemini | fal
	Key       string
	KeyEnv    string
	BaseURL   string
	Model     string
	EditModel string // fal: model for edits
	Size      string // default size
	HTTP      *http.Client
	// PollEvery is how often fal's queue is checked; default 1s.
	PollEvery time.Duration
}

// Picture is one generated image.
type Picture struct {
	Data      []byte
	MediaType string
}

// inputImage is an image to edit from.
type inputImage struct {
	Name      string
	MediaType string
	Data      []byte
}

// Generate makes n images from prompt, editing from inputs when given.
func (g *ImageGen) Generate(ctx context.Context, prompt, size string, n int, inputs []inputImage) ([]Picture, error) {
	if g.Key == "" {
		return nil, missingKey(g.Provider+" images", g.KeyEnv)
	}
	if size == "" {
		size = g.Size
	}
	var (
		pics []Picture
		err  error
	)
	switch g.Provider {
	case "openai":
		pics, err = g.openai(ctx, prompt, size, n, inputs)
	case "gemini":
		pics, err = g.gemini(ctx, prompt, size, n, inputs)
	case "fal":
		pics, err = g.fal(ctx, prompt, size, n, inputs)
	default:
		return nil, fmt.Errorf("unknown image provider %q", g.Provider)
	}
	if err != nil {
		return nil, fmt.Errorf("%s images: %w", g.Provider, err)
	}
	if len(pics) == 0 {
		return nil, fmt.Errorf("%s images: no image came back", g.Provider)
	}
	return pics, nil
}

func (g *ImageGen) client() *http.Client {
	if g.HTTP != nil {
		return g.HTTP
	}
	return newClient(5 * time.Minute)
}

// openai: POST /images/generations, or /images/edits (multipart) with
// input images. gpt-image models always answer base64; dall-e models are
// asked to.
func (g *ImageGen) openai(ctx context.Context, prompt, size string, n int, inputs []inputImage) ([]Picture, error) {
	base := strings.TrimRight(g.BaseURL, "/")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	model := firstNonEmpty(g.Model, "gpt-image-2")
	dalle := strings.HasPrefix(model, "dall-e")
	var req *http.Request
	var err error
	if len(inputs) == 0 {
		body := map[string]any{"model": model, "prompt": prompt, "n": n}
		if size != "" {
			body["size"] = size
		}
		if dalle {
			body["response_format"] = "b64_json"
		}
		raw, _ := json.Marshal(body)
		req, err = http.NewRequest(http.MethodPost, base+"/images/generations", bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
	} else {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		fields := map[string]string{"model": model, "prompt": prompt, "n": strconv.Itoa(n)}
		if size != "" {
			fields["size"] = size
		}
		if dalle {
			fields["response_format"] = "b64_json"
		}
		for k, v := range fields {
			if err := w.WriteField(k, v); err != nil {
				return nil, err
			}
		}
		field := "image[]"
		if dalle {
			field = "image"
		}
		for _, in := range inputs {
			h := textproto.MIMEHeader{}
			h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, field, in.Name))
			h.Set("Content-Type", in.MediaType)
			fw, err := w.CreatePart(h)
			if err != nil {
				return nil, err
			}
			if _, err := fw.Write(in.Data); err != nil {
				return nil, err
			}
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		req, err = http.NewRequest(http.MethodPost, base+"/images/edits", &buf)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", w.FormDataContentType())
	}
	req.Header.Set("Authorization", "Bearer "+g.Key)
	var out struct {
		Data []struct {
			B64 string `json:"b64_json"`
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := doJSONLarge(ctx, g.client(), req, &out); err != nil {
		return nil, err
	}
	var pics []Picture
	for _, d := range out.Data {
		switch {
		case d.B64 != "":
			data, err := base64.StdEncoding.DecodeString(d.B64)
			if err != nil {
				return nil, fmt.Errorf("bad image data: %w", err)
			}
			pics = append(pics, picture(data))
		case d.URL != "":
			data, err := g.download(ctx, d.URL, nil)
			if err != nil {
				return nil, err
			}
			pics = append(pics, picture(data))
		}
	}
	return pics, nil
}

// gemini: generateContent on an image output model, once per image.
func (g *ImageGen) gemini(ctx context.Context, prompt, size string, n int, inputs []inputImage) ([]Picture, error) {
	base := strings.TrimRight(g.BaseURL, "/")
	if base == "" {
		base = "https://generativelanguage.googleapis.com/v1beta"
	}
	model := firstNonEmpty(g.Model, "gemini-2.5-flash-image")
	type inline struct {
		MimeType string `json:"mime_type"`
		Data     string `json:"data"`
	}
	type part struct {
		Text       string  `json:"text,omitempty"`
		InlineData *inline `json:"inline_data,omitempty"`
	}
	parts := []part{{Text: prompt}}
	for _, in := range inputs {
		parts = append(parts, part{InlineData: &inline{MimeType: in.MediaType, Data: base64.StdEncoding.EncodeToString(in.Data)}})
	}
	gen := map[string]any{"responseModalities": []string{"TEXT", "IMAGE"}}
	if ar := aspectRatio(size); ar != "" {
		gen["imageConfig"] = map[string]string{"aspectRatio": ar}
	}
	raw, _ := json.Marshal(map[string]any{
		"contents":         []map[string]any{{"role": "user", "parts": parts}},
		"generationConfig": gen,
	})
	var pics []Picture
	for range n {
		req, err := http.NewRequest(http.MethodPost, base+"/models/"+model+":generateContent", bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		req.Header.Set("x-goog-api-key", g.Key)
		req.Header.Set("Content-Type", "application/json")
		var out struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text       string `json:"text"`
						InlineData *struct {
							MimeType string `json:"mimeType"`
							Data     string `json:"data"`
						} `json:"inlineData"`
						InlineSnake *struct {
							MimeType string `json:"mime_type"`
							Data     string `json:"data"`
						} `json:"inline_data"`
					} `json:"parts"`
				} `json:"content"`
				FinishReason string `json:"finishReason"`
			} `json:"candidates"`
			PromptFeedback struct {
				BlockReason string `json:"blockReason"`
			} `json:"promptFeedback"`
		}
		if err := doJSONLarge(ctx, g.client(), req, &out); err != nil {
			return nil, err
		}
		got, said, why := 0, "", out.PromptFeedback.BlockReason
		for _, c := range out.Candidates {
			if c.FinishReason != "" && c.FinishReason != "STOP" {
				why = c.FinishReason
			}
			for _, p := range c.Content.Parts {
				d := ""
				switch {
				case p.InlineData != nil:
					d = p.InlineData.Data
				case p.InlineSnake != nil:
					d = p.InlineSnake.Data
				}
				if d == "" {
					said += p.Text
					continue
				}
				data, err := base64.StdEncoding.DecodeString(d)
				if err != nil {
					return nil, fmt.Errorf("bad image data: %w", err)
				}
				pics = append(pics, picture(data))
				got++
			}
		}
		if got == 0 {
			msg := "the model returned no image"
			if why != "" {
				msg += " (" + why + ")"
			}
			if said = strings.TrimSpace(said); said != "" {
				msg += ": " + shorten(said, 300)
			}
			return pics, errors.New(msg)
		}
	}
	return pics, nil
}

// fal: submit to the queue, poll the request's status, then fetch the
// result and download its images.
func (g *ImageGen) fal(ctx context.Context, prompt, size string, n int, inputs []inputImage) ([]Picture, error) {
	base := strings.TrimRight(g.BaseURL, "/")
	if base == "" {
		base = "https://queue.fal.run"
	}
	model := firstNonEmpty(g.Model, "fal-ai/flux/dev")
	body := map[string]any{"prompt": prompt, "num_images": n, "output_format": "png"}
	if len(inputs) > 0 {
		model = firstNonEmpty(g.EditModel, "fal-ai/flux-pro/kontext/max/multi")
		var urls []string
		for _, in := range inputs {
			urls = append(urls, "data:"+in.MediaType+";base64,"+base64.StdEncoding.EncodeToString(in.Data))
		}
		body["image_urls"] = urls
	}
	if w, h, ok := parseSize(size); ok {
		body["image_size"] = map[string]int{"width": w, "height": h}
	} else if size != "" && size != "auto" {
		body["image_size"] = size // a named size such as landscape_16_9
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, base+"/"+strings.Trim(model, "/"), bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	auth := http.Header{"Authorization": {"Key " + g.Key}}
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
		parts := strings.Split(strings.Trim(model, "/"), "/") // the queue keys requests by owner/app
		app := strings.Join(parts[:min(2, len(parts))], "/")
		sub.ResponseURL = base + "/" + app + "/requests/" + sub.RequestID
		sub.StatusURL = sub.ResponseURL + "/status"
	}
	every := g.PollEvery
	if every == 0 {
		every = time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	for {
		req, err := http.NewRequest(http.MethodGet, sub.StatusURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header = auth.Clone()
		var st struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		if err := doJSON(ctx, g.client(), req, &st); err != nil {
			return nil, fmt.Errorf("queue status: %w", err)
		}
		if st.Error != "" {
			return nil, errors.New(st.Error)
		}
		if st.Status == "COMPLETED" {
			break
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting in the queue: %w", ctx.Err())
		case <-time.After(every):
		}
	}
	req, err = http.NewRequest(http.MethodGet, sub.ResponseURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header = auth.Clone()
	var res struct {
		Images []struct {
			URL         string `json:"url"`
			ContentType string `json:"content_type"`
		} `json:"images"`
		Image *struct {
			URL string `json:"url"`
		} `json:"image"`
		Detail json.RawMessage `json:"detail"`
	}
	if err := doJSONLarge(ctx, g.client(), req, &res); err != nil {
		return nil, fmt.Errorf("queue result: %w", err)
	}
	if res.Image != nil {
		res.Images = append(res.Images, struct {
			URL         string `json:"url"`
			ContentType string `json:"content_type"`
		}{URL: res.Image.URL})
	}
	if len(res.Images) == 0 && len(res.Detail) > 0 {
		return nil, fmt.Errorf("the model failed: %s", snippet(res.Detail))
	}
	var pics []Picture
	for _, im := range res.Images {
		data, err := g.download(ctx, im.URL, nil)
		if err != nil {
			return nil, err
		}
		pics = append(pics, picture(data))
	}
	return pics, nil
}

// download fetches a result image, which may be a data URI.
func (g *ImageGen) download(ctx context.Context, u string, h http.Header) ([]byte, error) {
	if rest, ok := strings.CutPrefix(u, "data:"); ok {
		_, b64, ok := strings.Cut(rest, ";base64,")
		if !ok {
			return nil, errors.New("unsupported data URI")
		}
		return base64.StdEncoding.DecodeString(b64)
	}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if h != nil {
		req.Header = h.Clone()
	}
	data, err := doRaw(ctx, g.client(), req, 50<<20)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", u, err)
	}
	return data, nil
}

func picture(data []byte) Picture {
	return Picture{Data: data, MediaType: media.Detect("", "", data)}
}

// doJSONLarge is doJSON for responses carrying base64 images.
func doJSONLarge(ctx context.Context, client *http.Client, req *http.Request, out any) error {
	raw, err := doRaw(ctx, client, req, 200<<20)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("unreadable response: %w", err)
	}
	return nil
}

var reSize = regexp.MustCompile(`^(\d{2,5})\s*[xX×]\s*(\d{2,5})$`)

func parseSize(s string) (w, h int, ok bool) {
	m := reSize.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, 0, false
	}
	w, _ = strconv.Atoi(m[1])
	h, _ = strconv.Atoi(m[2])
	return w, h, w > 0 && h > 0
}

// aspectRatio maps a size (1536x1024) or ratio (3:2) to the nearest ratio
// Gemini image models accept.
func aspectRatio(size string) string {
	ratios := []string{"1:1", "2:3", "3:2", "3:4", "4:3", "4:5", "5:4", "9:16", "16:9", "21:9"}
	var want float64
	if w, h, ok := parseSize(size); ok {
		want = float64(w) / float64(h)
	} else if a, b, ok := strings.Cut(size, ":"); ok {
		x, err1 := strconv.ParseFloat(a, 64)
		y, err2 := strconv.ParseFloat(b, 64)
		if err1 != nil || err2 != nil || y == 0 {
			return ""
		}
		want = x / y
	} else {
		return ""
	}
	best, diff := "", math.Inf(1)
	for _, r := range ratios {
		a, b, _ := strings.Cut(r, ":")
		x, _ := strconv.ParseFloat(a, 64)
		y, _ := strconv.ParseFloat(b, 64)
		if d := math.Abs(math.Log(want) - math.Log(x/y)); d < diff {
			best, diff = r, d
		}
	}
	return best
}

func shorten(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}

// image_generate

type imageGenerateTool struct{ gen *ImageGen }

// Risk is network: the prompt (and any input images) go to the provider.
// The images are written into the workspace only.
func (imageGenerateTool) Risk() tools.Risk { return tools.RiskNetwork }
func (imageGenerateTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "image_generate",
		Description: `Generate images from a text prompt, or edit existing images. Images are saved as files in your workspace (images/) and shown to you; send one to the user with send_file. "size" is WIDTHxHEIGHT (e.g. 1024x1024, 1536x1024, 1024x1536) or an aspect ratio like 16:9. "n" is how many images (1-4, default 1). "edit_from" lists image paths in your workspace to edit or combine; describe the change in the prompt. Write a detailed prompt: subject, style, composition, lighting, text to render.`,
		Schema: json.RawMessage(`{"type":"object","properties":{
			"prompt":{"type":"string"},
			"size":{"type":"string","description":"WIDTHxHEIGHT or W:H"},
			"n":{"type":"integer","minimum":1,"maximum":4},
			"edit_from":{"type":"array","items":{"type":"string"},"description":"workspace image paths to edit"}
		},"required":["prompt"]}`),
	}
}

func (t imageGenerateTool) Run(ctx context.Context, env *tools.Env, input json.RawMessage) (string, error) {
	in, err := decode[struct {
		Prompt   string          `json:"prompt"`
		Size     string          `json:"size"`
		N        int             `json:"n"`
		EditFrom json.RawMessage `json:"edit_from"`
	}](input)
	if err != nil {
		return "", err
	}
	in.Prompt = strings.TrimSpace(in.Prompt)
	if in.Prompt == "" {
		return "", errors.New("prompt is required")
	}
	n := min(max(in.N, 1), maxImages)
	var paths []string
	if len(in.EditFrom) > 0 && string(in.EditFrom) != "null" {
		if err := json.Unmarshal(in.EditFrom, &paths); err != nil {
			var one string
			if json.Unmarshal(in.EditFrom, &one) != nil {
				return "", errors.New("edit_from must be a list of image paths")
			}
			paths = []string{one}
		}
	}
	var inputs []inputImage
	for _, p := range paths {
		img, err := readInputImage(env, p)
		if err != nil {
			return "", err
		}
		inputs = append(inputs, img)
	}
	pics, err := t.gen.Generate(ctx, in.Prompt, strings.TrimSpace(in.Size), n, inputs)
	if err != nil {
		return "", err
	}
	dir, err := env.OutputDir("images")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if dir, err = env.OutputDir(dir); err != nil {
		return "", err
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	slug := slugify(in.Prompt)
	var b strings.Builder
	fmt.Fprintf(&b, "Generated %d image(s) with %s:\n", len(pics), t.gen.Provider)
	for i, pic := range pics {
		ext := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp", "image/gif": ".gif"}[pic.MediaType]
		if ext == "" {
			return "", fmt.Errorf("the provider returned %s, not an image", pic.MediaType)
		}
		name := fmt.Sprintf("%s-%s-%d%s", stamp, slug, i+1, ext)
		path := filepath.Join(dir, name)
		if err := store.WriteFileAtomic(path, pic.Data, 0o644); err != nil {
			return "", err
		}
		view, mt, note, err := modelImage(name, pic.Data)
		if err == nil {
			err = attachImage(ctx, env, name, mt, view)
		}
		if note != "" {
			note = ", " + note
		}
		fmt.Fprintf(&b, "- %s (%s%s)\n", path, humanSize(int64(len(pic.Data))), note)
		if err != nil {
			fmt.Fprintf(&b, "  (not shown: %v)\n", err)
		}
	}
	b.WriteString("The images follow. Use send_file to send one to the user.")
	return b.String(), nil
}

// readInputImage loads an image to edit. Workspace files always; others
// only in full mode or in ask mode (the user approved the call).
func readInputImage(env *tools.Env, p string) (inputImage, error) {
	path, err := filepath.EvalSymlinks(env.Resolve(p))
	if err != nil {
		return inputImage{}, err
	}
	ws := *env
	if real, err := filepath.EvalSymlinks(env.Workspace); err == nil {
		ws.Workspace = real
	}
	if !ws.InWorkspace(path) && env.Mode != tools.ModeFull && env.Mode != tools.ModeAsk {
		return inputImage{}, fmt.Errorf("%w: %s is outside the workspace; copy it into the workspace to edit it", tools.ErrDenied, p)
	}
	info, err := os.Stat(path)
	if err != nil {
		return inputImage{}, err
	}
	if info.Size() > maxEditInput {
		return inputImage{}, fmt.Errorf("%s is %s; images to edit must be under %s", p, humanSize(info.Size()), humanSize(maxEditInput))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return inputImage{}, err
	}
	name := filepath.Base(path)
	mt := media.Detect(name, "", data)
	if mt != "image/png" && mt != "image/jpeg" && mt != "image/webp" {
		return inputImage{}, fmt.Errorf("%s is %s; edit from PNG, JPEG or WebP images", p, mt)
	}
	return inputImage{Name: media.SafeName(name), MediaType: mt, Data: data}, nil
}

var reSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = strings.Trim(reSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "-")
	}
	if s == "" {
		s = "image"
	}
	return s
}
