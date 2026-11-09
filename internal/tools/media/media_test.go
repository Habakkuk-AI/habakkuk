package media

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDetectAndSafeName(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n0000")
	if got := Detect("report.pdf", "", []byte("%PDF-1.7\n")); got != "application/pdf" {
		t.Errorf("pdf: %q", got)
	}
	cases := []struct{ name, declared, want string }{
		// content (png) wins only for images; the rest use no content
		{"a.jpg", "", "image/png"},
		{"x", "application/octet-stream", "image/png"},
		{"v.ogg", "audio/ogg; codecs=opus", "audio/ogg"},
		{"photo.jpg", "image/jpeg", "image/png"}, // content wins for images
		{"notes.txt", "", "text/plain"},
	}
	for _, c := range cases {
		data := []byte(nil)
		if strings.HasPrefix(c.want, "image/") {
			data = png
		}
		if got := Detect(c.name, c.declared, data); got != c.want {
			t.Errorf("Detect(%q, %q) = %q, want %q", c.name, c.declared, got, c.want)
		}
	}
	for in, want := range map[string]string{"../../etc/passwd": "passwd", "my report (final).pdf": "my_report_final_.pdf", "": "file", "..": "file"} {
		if got := SafeName(in); got != want {
			t.Errorf("SafeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTranscribe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" || r.URL.Path != "/v1/audio/transcriptions" {
			http.Error(w, `{"error":"nope"}`, http.StatusUnauthorized)
			return
		}
		r.ParseMultipartForm(1 << 20)
		f, h, err := r.FormFile("file")
		if err != nil || r.FormValue("model") != "whisper-x" || !strings.HasSuffix(h.Filename, ".ogg") {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		data, _ := io.ReadAll(f)
		w.Write([]byte(`{"text":" heard ` + string(data) + ` "}`))
	}))
	defer srv.Close()
	tr := &Transcriber{BaseURL: srv.URL + "/v1", Key: "k", Model: "whisper-x"}
	got, err := tr.Transcribe(context.Background(), "voice", "audio/ogg", []byte("hello"))
	if err != nil || got != "heard hello" {
		t.Fatalf("got %q, %v", got, err)
	}
	tr.Key = "wrong"
	if _, err := tr.Transcribe(context.Background(), "voice.ogg", "audio/ogg", nil); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("bad key: %v", err)
	}
}
