// Package upload builds multipart/form-data bodies for the platform APIs
// that take files as form uploads.
package upload

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/textproto"
	"slices"
)

// File is the one file in a form.
type File struct {
	Field, Name, MediaType string
	Data                   []byte
}

// Form returns a multipart body with the fields (in name order) followed
// by the file, and its Content-Type.
func Form(fields map[string]string, f File) ([]byte, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if err := mw.WriteField(k, fields[k]); err != nil {
			return nil, "", err
		}
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, f.Field, f.Name))
	if f.MediaType == "" {
		f.MediaType = "application/octet-stream"
	}
	h.Set("Content-Type", f.MediaType)
	w, err := mw.CreatePart(h)
	if err != nil {
		return nil, "", err
	}
	if _, err := w.Write(f.Data); err != nil {
		return nil, "", err
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}
