package logs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Record is one parsed log line.
type Record struct {
	Time  time.Time
	Level slog.Level
	Msg   string
	Attrs []Attr // in the order they were written
	Raw   string
}

// Attr is one key and its JSON value.
type Attr struct {
	Key   string
	Value json.RawMessage
}

// Parse reads one JSON log line. A line that is not a JSON object becomes
// a record whose message is the whole line.
func Parse(line string) Record {
	rec := Record{Raw: line, Level: slog.LevelInfo, Msg: line}
	dec := json.NewDecoder(strings.NewReader(line))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return rec
	}
	rec.Msg = ""
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return Record{Raw: line, Level: slog.LevelInfo, Msg: line}
		}
		key, _ := t.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return Record{Raw: line, Level: slog.LevelInfo, Msg: line}
		}
		switch key {
		case slog.TimeKey:
			var s string
			if json.Unmarshal(v, &s) == nil {
				rec.Time, _ = time.Parse(time.RFC3339Nano, s)
			}
		case slog.LevelKey:
			var s string
			if json.Unmarshal(v, &s) == nil {
				_ = rec.Level.UnmarshalText([]byte(s))
			}
		case slog.MessageKey:
			_ = json.Unmarshal(v, &rec.Msg)
		default:
			rec.Attrs = append(rec.Attrs, Attr{Key: key, Value: v})
		}
	}
	return rec
}

// Filter selects records.
type Filter struct {
	MinLevel slog.Level     // records below this level are skipped
	Since    time.Time      // records before this are skipped (zero: all)
	Grep     *regexp.Regexp // the raw line must match (nil: all)
}

// NewGrep compiles a case-insensitive pattern; one that is not a valid
// regular expression is matched literally.
func NewGrep(pattern string) *regexp.Regexp {
	if pattern == "" {
		return nil
	}
	re, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		re = regexp.MustCompile("(?i)" + regexp.QuoteMeta(pattern))
	}
	return re
}

func (f Filter) Match(r Record) bool {
	if r.Level < f.MinLevel {
		return false
	}
	if !f.Since.IsZero() && !r.Time.IsZero() && r.Time.Before(f.Since) {
		return false
	}
	return f.Grep == nil || f.Grep.MatchString(r.Raw)
}

// Format renders a record on one line: time, level, message, then
// key=value pairs. Color adds ANSI colors by level.
func Format(r Record, color bool) string {
	var b strings.Builder
	if !r.Time.IsZero() {
		b.WriteString(r.Time.Local().Format("2006-01-02 15:04:05"))
		b.WriteByte(' ')
	}
	lvl := r.Level.String()
	code := ""
	switch {
	case r.Level >= slog.LevelError:
		code = "31"
	case r.Level >= slog.LevelWarn:
		code = "33"
	case r.Level < slog.LevelInfo:
		code = "2"
	}
	if color && code != "" {
		fmt.Fprintf(&b, "\033[%sm%-5s\033[0m ", code, lvl)
	} else {
		fmt.Fprintf(&b, "%-5s ", lvl)
	}
	if color {
		fmt.Fprintf(&b, "\033[1m%s\033[0m", r.Msg)
	} else {
		b.WriteString(r.Msg)
	}
	for _, a := range r.Attrs {
		b.WriteString("  ")
		if color {
			fmt.Fprintf(&b, "\033[36m%s\033[0m=", a.Key)
		} else {
			b.WriteString(a.Key + "=")
		}
		b.WriteString(value(a.Value))
	}
	return b.String()
}

// value prints strings bare when they hold no spaces, quoted otherwise, and
// other JSON as written.
func value(v json.RawMessage) string {
	var s string
	if json.Unmarshal(v, &s) == nil {
		if s == "" || strings.ContainsAny(s, " \t\n\"=") {
			return strconv.Quote(s)
		}
		return s
	}
	return string(v)
}

// Last writes the last n records matching f, across the rotated files
// oldest first. n <= 0 writes every match.
func Last(home string, f Filter, n int, w io.Writer, color bool) error {
	var keep []Record
	for _, path := range Files(home) {
		if err := scanFile(path, func(r Record) {
			if !f.Match(r) {
				return
			}
			keep = append(keep, r)
			if n > 0 && len(keep) > 2*n { // trim now and then, not per line
				keep = append(keep[:0], keep[len(keep)-n:]...)
			}
		}); err != nil {
			return err
		}
	}
	if n > 0 && len(keep) > n {
		keep = keep[len(keep)-n:]
	}
	for _, r := range keep {
		fmt.Fprintln(w, Format(r, color))
	}
	return nil
}

// Scan reads every record of the log and its rotated files, oldest first.
func Scan(home string, fn func(Record)) error {
	for _, path := range Files(home) {
		if err := scanFile(path, fn); err != nil {
			return err
		}
	}
	return nil
}

func scanFile(path string, fn func(Record)) error {
	fh, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		if line := sc.Text(); strings.TrimSpace(line) != "" {
			fn(Parse(line))
		}
	}
	return sc.Err()
}

// Follow writes records matching f as they are appended to the log, from
// its current end, until ctx ends. It follows the log across rotations.
func Follow(ctx context.Context, home string, f Filter, w io.Writer, color bool, poll time.Duration) error {
	path := filepath.Join(home, FileName)
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	var fh *os.File
	var partial []byte
	open := func(atEnd bool) {
		var err error
		if fh, err = os.Open(path); err != nil {
			fh = nil
			return
		}
		if atEnd {
			_, _ = fh.Seek(0, io.SeekEnd)
		}
		partial = partial[:0]
	}
	open(true)
	defer func() {
		if fh != nil {
			fh.Close()
		}
	}()
	buf := make([]byte, 64<<10)
	drain := func() {
		for fh != nil {
			n, err := fh.Read(buf)
			partial = append(partial, buf[:n]...)
			for {
				i := bytes.IndexByte(partial, '\n')
				if i < 0 {
					break
				}
				line := string(partial[:i])
				partial = partial[i+1:]
				if r := Parse(line); strings.TrimSpace(line) != "" && f.Match(r) {
					fmt.Fprintln(w, Format(r, color))
				}
			}
			if n == 0 || err != nil {
				return
			}
		}
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		drain()
		// Rotated or recreated: finish the old file, then read the new one
		// from its start.
		if onDisk, err := os.Stat(path); err == nil {
			if fh == nil {
				open(false)
				continue
			}
			pos, _ := fh.Seek(0, io.SeekCurrent)
			if cur, err := fh.Stat(); err == nil && (!os.SameFile(cur, onDisk) || onDisk.Size() < pos) {
				drain()
				fh.Close()
				open(false)
				continue
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// ParseLevel reads debug, info, warn or error (any case).
func ParseLevel(s string) (slog.Level, error) {
	var l slog.Level
	if s == "" {
		return slog.LevelDebug, nil
	}
	if strings.EqualFold(s, "warning") {
		s = "warn"
	}
	err := l.UnmarshalText([]byte(s))
	return l, err
}
