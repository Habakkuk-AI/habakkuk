package mediatools

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

type approver bool

func (a approver) Approve(context.Context, tools.ApprovalRequest) (bool, error) { return bool(a), nil }

func TestViewImageFile(t *testing.T) {
	env := testEnv(t)
	data := pngBytes(t, 40, 30)
	os.WriteFile(filepath.Join(env.Workspace, "shot.png"), data, 0o644)
	res, media := run(t, env, viewImageTool{client: http.DefaultClient}, `{"path":"shot.png"}`)
	if res.IsError || len(media) != 1 {
		t.Fatalf("result %q, media %d", res.Text, len(media))
	}
	if !strings.Contains(res.Text, "image/png") || !strings.Contains(res.Text, "40x30") {
		t.Fatalf("result: %s", res.Text)
	}
	stored, err := env.Blobs.Get(media[0].Media)
	if err != nil || !bytes.Equal(stored, data) || media[0].MediaType != "image/png" || media[0].Name != "shot.png" {
		t.Fatalf("blob: %v %+v", err, media[0])
	}
}

func TestViewImageScalesLargeImages(t *testing.T) {
	env := testEnv(t)
	img := image.NewNRGBA(image.Rect(0, 0, 3000, 600))
	for y := range 600 {
		for x := range 3000 {
			img.Set(x, y, color.NRGBA{255, 0, 0, uint8(x % 256)}) // varying transparency
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	os.WriteFile(filepath.Join(env.Workspace, "wide.png"), buf.Bytes(), 0o644)
	res, media := run(t, env, viewImageTool{client: http.DefaultClient}, `{"path":"wide.png"}`)
	if res.IsError || len(media) != 1 {
		t.Fatalf("result %q", res.Text)
	}
	if media[0].MediaType != "image/jpeg" || !strings.Contains(res.Text, "scaled to 1568x313") {
		t.Fatalf("not scaled: %s %s", media[0].MediaType, res.Text)
	}
	stored, _ := env.Blobs.Get(media[0].Media)
	small, err := jpeg.Decode(bytes.NewReader(stored))
	if err != nil || small.Bounds().Dx() != 1568 {
		t.Fatalf("decoded: %v %v", err, small.Bounds())
	}
	// Transparent red over white is pale: the first column had alpha ~0.
	if r, g, _, _ := small.At(0, 10).RGBA(); r>>8 < 200 || g>>8 < 150 {
		t.Fatalf("transparency not flattened onto white: %d %d", r>>8, g>>8)
	}
}

func TestViewImageURLAndPolicy(t *testing.T) {
	data := pngBytes(t, 5, 5)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cat.png" {
			w.Write(data)
			return
		}
		w.Write([]byte("<html>not an image</html>"))
	}))
	defer srv.Close()
	tool := viewImageTool{client: srv.Client()}
	env := testEnv(t)
	res, media := run(t, env, tool, `{"path":"`+srv.URL+`/cat.png?size=large"}`)
	if res.IsError || len(media) != 1 || media[0].Name != "cat.png" {
		t.Fatalf("url: %s %+v", res.Text, media)
	}
	res, _ = run(t, env, tool, `{"path":"`+srv.URL+`/page"}`)
	if !res.IsError || !strings.Contains(res.Text, "not an image") {
		t.Fatalf("html: %s", res.Text)
	}
	env.Mode = tools.ModeReadOnly
	res, _ = run(t, env, tool, `{"path":"`+srv.URL+`/cat.png"}`)
	if !res.IsError || !strings.Contains(res.Text, "read-only") {
		t.Fatalf("read-only: %s", res.Text)
	}
	env.Mode = tools.ModeAsk
	env.Approver = approver(false)
	res, _ = run(t, env, tool, `{"path":"`+srv.URL+`/cat.png"}`)
	if !res.IsError || !strings.Contains(res.Text, "declined") {
		t.Fatalf("declined: %s", res.Text)
	}
	env.Approver = approver(true)
	if res, media = run(t, env, tool, `{"path":"`+srv.URL+`/cat.png"}`); res.IsError || len(media) != 1 {
		t.Fatalf("approved: %s", res.Text)
	}
}

func TestViewImageNeedsMediaSink(t *testing.T) {
	env := testEnv(t)
	os.WriteFile(filepath.Join(env.Workspace, "a.png"), pngBytes(t, 2, 2), 0o644)
	_, err := viewImageTool{}.Run(context.Background(), env, []byte(`{"path":"a.png"}`))
	if err == nil || !strings.Contains(err.Error(), "cannot show images") {
		t.Fatalf("err: %v", err)
	}
}
