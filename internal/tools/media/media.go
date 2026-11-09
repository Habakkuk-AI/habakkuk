// Package media handles files that arrive with messages: type detection,
// safe names, and speech-to-text for voice notes.
package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Detect returns a file's media type. For images and PDFs the content wins,
// since a mislabelled image is rejected by models; otherwise the declared
// type, then the extension, then the content.
func Detect(name, declared string, data []byte) string {
	sniffed := strings.Split(http.DetectContentType(data), ";")[0]
	if strings.HasPrefix(sniffed, "image/") || sniffed == "application/pdf" {
		return sniffed
	}
	declared = strings.TrimSpace(strings.Split(declared, ";")[0])
	if declared != "" && declared != "application/octet-stream" {
		return strings.ToLower(declared)
	}
	if t := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); t != "" {
		return strings.Split(t, ";")[0]
	}
	return sniffed
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// SafeName turns a platform file name into one safe to write to disk.
func SafeName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Trim(unsafeName.ReplaceAllString(name, "_"), "._")
	if name == "" {
		name = "file"
	}
	if len(name) > 100 {
		ext := filepath.Ext(name)
		if len(ext) > 10 {
			ext = ""
		}
		name = name[:100-len(ext)] + ext
	}
	return name
}

// Transcriber calls an OpenAI-compatible /audio/transcriptions endpoint.
type Transcriber struct {
	BaseURL string // e.g. https://api.openai.com/v1
	Key     string
	Model   string
	Prompt  string // vocabulary hints
	HTTP    *http.Client
}

func (t *Transcriber) Transcribe(ctx context.Context, name, mediaType string, data []byte) (string, error) {
	if t.Key == "" {
		return "", errors.New("transcription key is not set")
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err := w.WriteField("model", t.Model); err != nil {
		return "", err
	}
	if t.Prompt != "" {
		if err := w.WriteField("prompt", t.Prompt); err != nil {
			return "", err
		}
	}
	// Endpoints pick the decoder from the file name; voice notes are Ogg Opus.
	if filepath.Ext(name) == "" {
		if strings.Contains(mediaType, "ogg") || strings.Contains(mediaType, "opus") {
			name += ".ogg"
		} else if exts, _ := mime.ExtensionsByType(mediaType); len(exts) > 0 {
			name += exts[0]
		}
	}
	fw, err := w.CreateFormFile("file", name)
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(data); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(t.BaseURL, "/")+"/audio/transcriptions", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+t.Key)
	req.Header.Set("Content-Type", w.FormDataContentType())
	client := t.HTTP
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("transcription: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw[:min(len(raw), 300)])))
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("transcription: unreadable response: %w", err)
	}
	return strings.TrimSpace(out.Text), nil
}
