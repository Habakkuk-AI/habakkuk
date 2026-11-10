// Package logs writes and reads Habakkuk's JSON log ($HABAKKUK_HOME/habakkuk.log):
// size-based rotation that several processes can share, redaction of
// secret values before anything reaches the disk, and pretty printing for
// `hk logs`.
package logs

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/habakkuk-ai/habakkuk/internal/core/platform"

	"gopkg.in/yaml.v3"
)

// FileName is the log's name in $HABAKKUK_HOME.
const FileName = "habakkuk.log"

// Defaults when the config does not say.
const (
	DefaultMaxMB   = 20
	DefaultBackups = 5
)

// Rotating is an append-only file that is renamed to path.1 (path.1 to
// path.2, and so on, keeping Backups of them) once it would grow past
// MaxBytes. Every process writing the log rotates under a lock file, and a
// process that finds the file renamed under it reopens the new one.
type Rotating struct {
	Path     string
	MaxBytes int64
	Backups  int

	mu sync.Mutex
	f  *os.File
}

// OpenRotating opens path for appending, creating it with mode 0600.
func OpenRotating(path string, maxBytes int64, backups int) (*Rotating, error) {
	r := &Rotating{Path: path, MaxBytes: maxBytes, Backups: backups}
	if err := r.reopen(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Rotating) reopen() error {
	if r.f != nil {
		r.f.Close()
		r.f = nil
	}
	f, err := os.OpenFile(r.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	r.f = f
	return nil
}

// Write appends p, rotating first when p would take the file past MaxBytes.
// One call is one log record, so records never straddle two files.
func (r *Rotating) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		if err := r.reopen(); err != nil {
			return 0, err
		}
	}
	// Another process may have rotated the file since we opened it.
	if cur, err := r.f.Stat(); err == nil {
		if onDisk, err := os.Stat(r.Path); err != nil || !os.SameFile(cur, onDisk) {
			if err := r.reopen(); err != nil {
				return 0, err
			}
		}
	}
	if r.MaxBytes > 0 {
		if st, err := r.f.Stat(); err == nil && st.Size() > 0 && st.Size()+int64(len(p)) > r.MaxBytes {
			if err := r.rotate(); err != nil {
				// Keep logging into the big file rather than lose records.
				fmt.Fprintf(os.Stderr, "habakkuk: log rotation failed: %v\n", err)
			}
		}
	}
	if r.f == nil { // a failed rotation could not reopen the file
		if err := r.reopen(); err != nil {
			return 0, err
		}
	}
	return r.f.Write(p)
}

// rotate shifts the backups under an exclusive lock, rechecking the size
// once it holds the lock, since another process may have just rotated.
func (r *Rotating) rotate() error {
	lock, err := os.OpenFile(r.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := platform.Lock(lock); err != nil {
		return err
	}
	defer platform.Unlock(lock) //nolint:errcheck // closing releases it anyway

	if st, err := os.Stat(r.Path); err == nil && st.Size() < r.MaxBytes/2 {
		return r.reopen() // someone else rotated
	}
	if r.Backups <= 0 {
		if err := os.Truncate(r.Path, 0); err != nil {
			return err
		}
		return r.reopen()
	}
	os.Remove(fmt.Sprintf("%s.%d", r.Path, r.Backups))
	for i := r.Backups - 1; i >= 1; i-- {
		err := os.Rename(fmt.Sprintf("%s.%d", r.Path, i), fmt.Sprintf("%s.%d", r.Path, i+1))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if platform.IsWindows && r.f != nil {
		// Windows cannot rename a file this process holds open.
		_ = r.f.Close()
		r.f = nil
	}
	if err := os.Rename(r.Path, r.Path+".1"); err != nil {
		if r.f == nil {
			_ = r.reopen() // keep logging into the big file
		}
		return err
	}
	return r.reopen()
}

// Close closes the file.
func (r *Rotating) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

// Settings reads logging.max_mb and logging.backups from config.yaml
// leniently, so a config that does not load still gets a rotated log.
func Settings(home string) (maxMB, backups int) {
	maxMB, backups = DefaultMaxMB, DefaultBackups
	data, err := os.ReadFile(filepath.Join(home, "config.yaml"))
	if err != nil {
		return maxMB, backups
	}
	var c struct {
		Logging struct {
			MaxMB   int `yaml:"max_mb"`
			Backups int `yaml:"backups"`
		} `yaml:"logging"`
	}
	if yaml.Unmarshal(data, &c) != nil {
		return maxMB, backups
	}
	if c.Logging.MaxMB > 0 {
		maxMB = c.Logging.MaxMB
	}
	if c.Logging.Backups > 0 {
		backups = c.Logging.Backups
	}
	return maxMB, backups
}

// Open opens $HABAKKUK_HOME/habakkuk.log as a JSON logger that rotates by size
// and redacts secrets. Add secret values to the returned Redactor as they
// become known; the patterns of well-known token formats apply from the
// start, as do the values of secret-looking variables in .env.
func Open(home string) (*slog.Logger, *Redactor, func(), error) {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, nil, nil, err
	}
	maxMB, backups := Settings(home)
	f, err := OpenRotating(filepath.Join(home, FileName), int64(maxMB)<<20, backups)
	if err != nil {
		return nil, nil, nil, err
	}
	red := NewRedactor(f)
	red.Add(DotEnvSecrets(filepath.Join(home, ".env"))...)
	return slog.New(slog.NewJSONHandler(red, nil)), red, func() { _ = f.Close() }, nil
}

// Files lists the log and its backups, oldest first.
func Files(home string) []string {
	base := filepath.Join(home, FileName)
	var out []string
	for i := 50; i >= 1; i-- {
		p := fmt.Sprintf("%s.%d", base, i)
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	if _, err := os.Stat(base); err == nil {
		out = append(out, base)
	}
	return out
}

// secretName reports whether a variable name looks like it holds a secret.
func secretName(name string) bool {
	n := strings.ToUpper(name)
	for _, w := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD", "PASS", "AUTH", "CREDENTIAL", "PRIVATE", "SESSION", "COOKIE", "DSN", "WEBHOOK"} {
		if strings.Contains(n, w) {
			return true
		}
	}
	return false
}
