package logs

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// Redacted replaces a secret in the log.
const Redacted = "[redacted]"

// minSecret is the shortest value treated as a secret; shorter ones would
// blank out ordinary words.
const minSecret = 6

// tokenPatterns match well-known credential formats, whatever variable they
// came from.
var tokenPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-(?:ant-|proj-|or-v1-)?[A-Za-z0-9_\-]{20,}`),                       // Anthropic, OpenAI, OpenRouter
	regexp.MustCompile(`\b(?:sk|rk|pk)_(?:live|test)_[A-Za-z0-9]{16,}`),                      // Stripe
	regexp.MustCompile(`\bxox[abposre]-[A-Za-z0-9\-]{10,}`),                                  // Slack bot and user tokens
	regexp.MustCompile(`\bxapp-[A-Za-z0-9\-]{10,}`),                                          // Slack app tokens
	regexp.MustCompile(`\bgh[opsur]_[A-Za-z0-9]{30,}`),                                       // GitHub tokens
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{30,}`),                                     // GitHub fine-grained tokens
	regexp.MustCompile(`\bglpat-[A-Za-z0-9_\-]{20,}`),                                        // GitLab
	regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),                                      // AWS access key ids
	regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}`),                                           // Google API keys
	regexp.MustCompile(`\b\d{8,10}:AA[A-Za-z0-9_\-]{33}`),                                    // Telegram bot tokens
	regexp.MustCompile(`\bhf_[A-Za-z0-9]{30,}`),                                              // Hugging Face
	regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{8,}\.eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`), // JWTs
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`),
}

// bearer keeps the scheme visible and hides the credential.
var bearer = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/\-]{12,}=*`)

// Redactor rewrites each write, replacing known secret values and token
// patterns with [redacted], before passing it on. A write is one log
// record, so a secret is never split across two writes.
type Redactor struct {
	next io.Writer

	mu      sync.RWMutex
	secrets []string // longest first, so a secret containing another is replaced whole
}

func NewRedactor(next io.Writer) *Redactor { return &Redactor{next: next} }

// Add registers secret values. Short and empty values are ignored. The
// JSON-escaped form of a value is registered too, since that is how a
// value with quotes or backslashes appears in a JSON log line.
func (r *Redactor) Add(values ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range values {
		v = strings.TrimSpace(v)
		if len(v) < minSecret {
			continue
		}
		forms := []string{v}
		if b, err := json.Marshal(v); err == nil {
			if esc := string(b[1 : len(b)-1]); esc != v {
				forms = append(forms, esc)
			}
		}
		for _, f := range forms {
			if !slices.Contains(r.secrets, f) {
				r.secrets = append(r.secrets, f)
			}
		}
	}
	slices.SortFunc(r.secrets, func(a, b string) int { return len(b) - len(a) })
}

// Redact returns s with secrets replaced.
func (r *Redactor) Redact(s string) string {
	r.mu.RLock()
	for _, sec := range r.secrets {
		if strings.Contains(s, sec) {
			s = strings.ReplaceAll(s, sec, Redacted)
		}
	}
	r.mu.RUnlock()
	return RedactPatterns(s)
}

// RedactPatterns replaces well-known token formats and bearer credentials.
func RedactPatterns(s string) string {
	for _, re := range tokenPatterns {
		s = re.ReplaceAllString(s, Redacted)
	}
	return bearer.ReplaceAllString(s, "$1 "+Redacted)
}

func (r *Redactor) Write(p []byte) (int, error) {
	out := r.Redact(string(p))
	if _, err := io.WriteString(r.next, out); err != nil {
		return 0, err
	}
	return len(p), nil
}

// DotEnvSecrets returns the values in a .env file whose names look secret
// (KEY, TOKEN, SECRET, PASSWORD, ...).
func DotEnvSecrets(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if secretName(strings.TrimSpace(k)) {
			out = append(out, v)
		}
	}
	return out
}
