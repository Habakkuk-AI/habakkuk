// Package frontmatter reads and writes Markdown files that start with a YAML
// block between "---" lines, the format of memory notes and SKILL.md files.
package frontmatter

import (
	"bytes"
	"strings"

	"gopkg.in/yaml.v3"
)

// Split separates the leading YAML block from the body. ok is false when the
// file has no frontmatter; body is then the whole file.
func Split(data []byte) (meta, body []byte, ok bool) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")) // UTF-8 byte order mark
	first, rest, found := cutLine(data)
	if !found || strings.TrimRight(string(first), " \t") != "---" {
		return nil, data, false
	}
	start := len(data) - len(rest)
	for pos := start; pos < len(data); {
		line, next, _ := cutLine(data[pos:])
		if s := strings.TrimRight(string(line), " \t"); s == "---" || s == "..." {
			return data[start:pos], data[len(data)-len(next):], true
		}
		pos = len(data) - len(next)
	}
	return nil, data, false
}

// cutLine returns the first line without its terminator, and what follows.
func cutLine(b []byte) (line, rest []byte, found bool) {
	line, rest, found = bytes.Cut(b, []byte("\n"))
	return bytes.TrimSuffix(line, []byte("\r")), rest, found
}

// Parse decodes the frontmatter into v (unknown keys are ignored, since
// humans edit these files) and returns the trimmed body. A file without
// frontmatter, or with frontmatter that is not valid YAML, is all body and
// leaves v untouched; err reports the YAML problem in the second case.
func Parse(data []byte, v any) (body string, err error) {
	meta, rest, ok := Split(data)
	if !ok {
		return strings.TrimSpace(string(data)), nil
	}
	if err := yaml.Unmarshal(meta, v); err != nil {
		return strings.TrimSpace(string(data)), err
	}
	return strings.TrimSpace(string(rest)), nil
}

// Format renders meta as a frontmatter block followed by the body.
func Format(meta any, body string) ([]byte, error) {
	y, err := yaml.Marshal(meta)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteString("---\n")
	b.Write(y)
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimSpace(body))
	b.WriteString("\n")
	return b.Bytes(), nil
}
