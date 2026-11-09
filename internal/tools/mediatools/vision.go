package mediatools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	// Decoders for image.Decode.
	_ "image/gif"
	_ "image/png"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
	"github.com/habakkuk-ai/habakkuk/internal/tools/media"
)

// Limits for images handed to the model. Larger or bigger images are scaled
// down (JPEG, PNG and GIF; WebP cannot be decoded without a codec, so an
// oversize WebP is refused).
const (
	maxModelImage = 5 << 20  // bytes, the strictest provider's cap
	maxImageEdge  = 2048     // pixels on the long edge before scaling
	scaledEdge    = 1568     // pixels on the long edge after scaling
	maxImageInput = 50 << 20 // largest file view_image reads
)

var modelImageTypes = map[string]bool{"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true}

// modelImage checks that data is an image a model can read and scales it
// down when it is too large. note says what was done, for the tool result.
func modelImage(name string, data []byte) (out []byte, mediaType, note string, err error) {
	mediaType = media.Detect(name, "", data)
	if !modelImageTypes[mediaType] {
		return nil, "", "", fmt.Errorf("%s is %s, not an image the model can read (JPEG, PNG, GIF or WebP)", name, mediaType)
	}
	w, h := 0, 0
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		w, h = cfg.Width, cfg.Height
		note = fmt.Sprintf("%dx%d", w, h)
	}
	if len(data) <= maxModelImage && max(w, h) <= maxImageEdge {
		return data, mediaType, note, nil
	}
	if mediaType == "image/webp" {
		if len(data) > maxModelImage {
			return nil, "", "", fmt.Errorf("%s is a %s WebP; convert it to PNG or JPEG under 5 MB first", name, humanSize(int64(len(data))))
		}
		return data, mediaType, note, nil
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", "", fmt.Errorf("%s: cannot decode the image to scale it down: %w", name, err)
	}
	small := downscale(img, scaledEdge)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, small, &jpeg.Options{Quality: 85}); err != nil {
		return nil, "", "", err
	}
	b := small.Bounds()
	return buf.Bytes(), "image/jpeg", fmt.Sprintf("%dx%d, scaled to %dx%d for viewing", w, h, b.Dx(), b.Dy()), nil
}

// downscale shrinks img so its long edge is at most edge pixels, averaging
// each source box and flattening transparency onto white.
func downscale(img image.Image, edge int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w == 0 || h == 0 {
		return img
	}
	nw, nh := w, h
	if w >= h && w > edge {
		nw, nh = edge, max(1, h*edge/w)
	} else if h > w && h > edge {
		nw, nh = max(1, w*edge/h), edge
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := range nh {
		y0, y1 := b.Min.Y+y*h/nh, b.Min.Y+(y+1)*h/nh
		y1 = max(y1, y0+1)
		for x := range nw {
			x0, x1 := b.Min.X+x*w/nw, b.Min.X+(x+1)*w/nw
			x1 = max(x1, x0+1)
			var r, g, bl, a, n uint64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					cr, cg, cb, ca := img.At(sx, sy).RGBA()
					r, g, bl, a, n = r+uint64(cr), g+uint64(cg), bl+uint64(cb), a+uint64(ca), n+1
				}
			}
			white := 0xffff - a/n // premultiplied: add white where it is transparent
			i := dst.PixOffset(x, y)
			dst.Pix[i+0] = uint8(min(r/n+white, 0xffff) >> 8)
			dst.Pix[i+1] = uint8(min(g/n+white, 0xffff) >> 8)
			dst.Pix[i+2] = uint8(min(bl/n+white, 0xffff) >> 8)
			dst.Pix[i+3] = 0xff
		}
	}
	return dst
}

// attachImage stores an image in the blob store and attaches it to the
// running tool call's result.
func attachImage(ctx context.Context, env *tools.Env, name, mediaType string, data []byte) error {
	if env.Blobs == nil {
		return errNoBlobs
	}
	ref, err := env.Blobs.Put(data)
	if err != nil {
		return err
	}
	if !tools.AddMedia(ctx, llm.Block{Type: llm.Image, Media: ref, MediaType: mediaType, Name: name}) {
		return errors.New("this run cannot show images to the model")
	}
	return nil
}

// view_image

type viewImageTool struct{ client *http.Client }

// Risk is read: a local image is a read. A URL is a network call, which
// the tool itself refuses in read-only mode and asks about in ask mode.
func (viewImageTool) Risk() tools.Risk { return tools.RiskRead }
func (viewImageTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "view_image",
		Description: `Look at an image: loads a JPEG, PNG, GIF or WebP into the conversation so you can see it. "path" is a file (relative to your workspace unless absolute) or an http(s) URL. Large images are scaled down. Use it for screenshots, photos, charts and images you generated or were sent.`,
		Schema: json.RawMessage(`{"type":"object","properties":{
			"path":{"type":"string","description":"file path or http(s) URL of the image"}
		},"required":["path"]}`),
	}
}

func (t viewImageTool) Run(ctx context.Context, env *tools.Env, input json.RawMessage) (string, error) {
	in, err := decode[struct {
		Path string `json:"path"`
		URL  string `json:"url"`
	}](input)
	if err != nil {
		return "", err
	}
	src := strings.TrimSpace(firstNonEmpty(in.Path, in.URL))
	if src == "" {
		return "", errors.New("path is required")
	}
	var data []byte
	name := ""
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		if err := allowNetwork(ctx, env, "view_image", "fetch the image at "+src); err != nil {
			return "", err
		}
		req, err := http.NewRequest(http.MethodGet, src, nil)
		if err != nil {
			return "", err
		}
		if data, err = doRaw(ctx, t.client, req, maxImageInput); err != nil {
			return "", fmt.Errorf("fetch %s: %w", src, err)
		}
		name = media.SafeName(path.Base(strings.SplitN(src, "?", 2)[0]))
	} else {
		p := env.Resolve(src)
		info, err := os.Stat(p)
		if err != nil {
			return "", err
		}
		if info.Size() > maxImageInput {
			return "", fmt.Errorf("%s is %s; the limit is %s", src, humanSize(info.Size()), humanSize(maxImageInput))
		}
		if data, err = os.ReadFile(p); err != nil {
			return "", err
		}
		name = filepath.Base(p)
	}
	out, mt, note, err := modelImage(name, data)
	if err != nil {
		return "", err
	}
	if err := attachImage(ctx, env, name, mt, out); err != nil {
		return "", err
	}
	if note != "" {
		note = ", " + note
	}
	return fmt.Sprintf("Showing %s (%s, %s%s). The image follows.", src, mt, humanSize(int64(len(data))), note), nil
}

// allowNetwork applies the network rule to a read-risk tool that reaches
// the network for one call: denied in read-only mode, asked in ask mode.
func allowNetwork(ctx context.Context, env *tools.Env, tool, summary string) error {
	switch env.Mode {
	case tools.ModeReadOnly:
		return fmt.Errorf("%w: %s cannot reach the network in read-only mode", tools.ErrDenied, tool)
	case tools.ModeAsk:
		if env.Approver == nil {
			return fmt.Errorf("%w: %s needs approval to reach the network and no one is attending this run", tools.ErrDenied, tool)
		}
		ok, err := env.Approver.Approve(ctx, tools.ApprovalRequest{Tool: tool, Summary: summary,
			Session: env.Session, Task: env.Task, Workspace: env.Workspace})
		if errors.Is(err, tools.ErrNoApprover) {
			return fmt.Errorf("%w: %s needs approval to reach the network and no one is attending this run", tools.ErrDenied, tool)
		}
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: the user declined: %s", tools.ErrDenied, summary)
		}
	}
	return nil
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", (n+1023)>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}
