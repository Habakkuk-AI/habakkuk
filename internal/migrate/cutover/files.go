package cutover

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// globAll lists the files in dir that match any of the patterns.
func globAll(dir string, patterns ...string) []string {
	var out []string
	for _, p := range patterns {
		m, _ := filepath.Glob(filepath.Join(dir, p))
		for _, f := range m {
			if !slices.Contains(out, f) {
				out = append(out, f)
			}
		}
	}
	slices.Sort(out)
	return out
}

// readSmall reads the start of a service file; "" when it cannot be read.
func readSmall(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, 256<<10))
	return string(b)
}

// mentionsDir reports whether a service file names dir or a path in it.
func mentionsDir(text, dir string) bool {
	if dir == "" || text == "" {
		return false
	}
	for _, d := range []string{dir, filepath.ToSlash(dir)} {
		for i := 0; ; {
			j := strings.Index(text[i:], d)
			if j < 0 {
				break
			}
			end := i + j + len(d)
			// The whole folder name: followed by a separator, a quote, an
			// end of value or the end of the text.
			if end == len(text) || strings.ContainsRune("/\\\"'<\n\r\t ;:", rune(text[end])) {
				return true
			}
			i = end
		}
	}
	return false
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func joinErrs(msgs []string) error {
	if len(msgs) == 0 {
		return nil
	}
	return errors.New(strings.Join(msgs, "; "))
}

// brake is an old agent's own switch that stops it starting new work
// while a file is there. Finish sets it, so a process of the old agent
// that starts again later (a supervisor, a login item it missed) does
// nothing; rollback removes it, but only when finish made it.
type brake struct {
	file func(dir string) string
	text func(now time.Time) string
}
