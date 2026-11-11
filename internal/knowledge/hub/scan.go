package hub

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Finding is one suspicious thing the install-time scan saw.
type Finding struct {
	File string // slash path inside the package
	Line int    // 1-based; 0 for a whole-file finding
	Rule string // pipe-to-shell, env-exfiltration, credential-files, write-outside, privilege, obfuscation, binary
	Text string // the line, shortened
}

func (f Finding) String() string {
	loc := f.File
	if f.Line > 0 {
		loc = fmt.Sprintf("%s:%d", f.File, f.Line)
	}
	if f.Text == "" {
		return loc + ": " + f.Rule
	}
	return fmt.Sprintf("%s: %s: %s", loc, f.Rule, f.Text)
}

const maxScanFile = 2 << 20

type lineRule struct {
	name string
	re   *regexp.Regexp
}

// Rules that fire on a single line.
var lineRules = []lineRule{
	{"pipe-to-shell", regexp.MustCompile(`(?i)\b(curl|wget|fetch|iwr|invoke-webrequest)\b[^\n|]*\|\s*(sudo\s+)?(-?[a-z]*\s+)?(ba|z|k|da|fi)?sh\b`)},
	{"pipe-to-shell", regexp.MustCompile(`(?i)\b(curl|wget)\b[^\n|]*\|\s*(sudo\s+)?(python[0-9.]*|perl|ruby|node|php)\b`)},
	{"pipe-to-shell", regexp.MustCompile(`(?i)\b(ba|z|k|da)?sh\s+(-c\s+)?["']?(\$\(|<\(|` + "`" + `)\s*(curl|wget)\b`)},
	{"pipe-to-shell", regexp.MustCompile(`(?i)\b(iex|invoke-expression)\b.*\b(iwr|invoke-webrequest|downloadstring|irm|invoke-restmethod)\b`)},
	{"pipe-to-shell", regexp.MustCompile(`(?i)\b(source|\.)\s+<\(\s*(curl|wget)\b`)},

	{"write-outside", regexp.MustCompile(`(>>?|\btee\s+(-a\s+)?)\s*["']?(~|\$HOME\b|\$\{HOME\}|/(etc|usr|bin|sbin|opt|var|Library|System|root|home|Users|Applications)\b)`)},
	{"write-outside", regexp.MustCompile(`\b(cp|mv|install|ln|rsync|dd)\b[^\n]*\s["']?(of=)?(~|\$HOME\b|\$\{HOME\}|/(etc|usr|bin|sbin|opt|var|Library|System|root|home|Users|Applications)\b)`)},
	{"write-outside", regexp.MustCompile(`(?i)(\bcrontab\s+-?[a-z]|\.bashrc\b|\.zshrc\b|\.bash_profile\b|\.zprofile\b|\.profile\b|authorized_keys|LaunchAgents|LaunchDaemons|systemctl\s+(--user\s+)?enable|/etc/rc\.local|\.config/autostart|/etc/sudoers)`)},
	{"write-outside", regexp.MustCompile(`\brm\s+(-[a-zA-Z]+\s+)*["']?(/|~/?|\$HOME/?|\$\{HOME\}/?)["']?(\*|\s|$)`)},
	{"write-outside", regexp.MustCompile(`\bopen\(\s*(f?["'](/|~)|os\.path\.expanduser|Path\.home\(\)|os\.environ\[["']HOME)[^)]*,\s*["'](w|a|x|wb|ab|r\+)`)},
	{"write-outside", regexp.MustCompile(`\b(writeFile|writeFileSync|appendFile|appendFileSync|createWriteStream)\(\s*(["'` + "`" + `](/|~)|os\.homedir\(\)|process\.env\.HOME)`)},
	{"write-outside", regexp.MustCompile(`(>>?|\b(cp|mv|tee|writeFile\w*|open)\b)[^\n]*(\.\./){2,}`)},

	{"privilege", regexp.MustCompile(`(^|[\s;&|(` + "`" + `])sudo\s+\S`)},
	{"privilege", regexp.MustCompile(`\bchmod\s+([ugoa]*\+s|[0-7]?[4-7][0-7]{3})\b`)},

	{"obfuscation", regexp.MustCompile(`(?i)\bbase64\s+(-d|--decode|-D)\b[^\n]*\|\s*(sudo\s+)?(ba|z|k|da)?sh\b`)},
	{"obfuscation", regexp.MustCompile(`(?i)\|\s*base64\s+(-d|--decode|-D)\s*\|\s*(sudo\s+)?(ba|z|k|da)?sh\b`)},
	{"obfuscation", regexp.MustCompile(`(?i)\beval\b[^\n]*\b(base64|xxd\s+-r|openssl\s+enc)\b`)},
	{"obfuscation", regexp.MustCompile(`\b(eval|exec)\s*\(\s*(atob|Buffer\.from|unescape|decodeURIComponent|base64\.b64decode|codecs\.decode|bytes\.fromhex|zlib\.decompress|marshal\.loads|__import__\(\s*["'](base64|zlib|marshal|codecs))`)},
	{"obfuscation", regexp.MustCompile(`\bnew\s+Function\s*\(\s*(atob|Buffer\.from|unescape)`)},
	{"obfuscation", regexp.MustCompile(`String\.fromCharCode\(\s*(\d+\s*,\s*){20,}`)},
	{"obfuscation", regexp.MustCompile(`(\\x[0-9a-fA-F]{2}){24,}`)},
}

// Whole-environment dumps and credential files are only dangerous together
// with a way to send them out, anywhere in the same file.
var (
	reEnvDump = regexp.MustCompile(`(\bprintenv\b|(^|[\s;|&(` + "`" + `])env\s*($|[|>;)` + "`" + `])|/proc/(self|\d+)/environ|\bos\.environ\b\s*($|[),;}]|\.copy\(\)|\.items\(\))|\bdict\(\s*os\.environ\s*\)|\bprocess\.env\s*($|[),;}])|JSON\.stringify\(\s*process\.env|\bENV\.to_h\b|\$\(\s*(env|printenv|set)\s*\))`)
	reSecrets = regexp.MustCompile(`(~|\$HOME|\$\{HOME\}|homedir\(\)|expanduser\(\s*["']~)[/"', ]*\.?(ssh/|aws/credentials|netrc|docker/config\.json|kube/config|gnupg|config/gh/|git-credentials)|\bid_(rsa|ed25519|ecdsa)\b|security\s+find-(generic|internet)-password|/etc/shadow`)
	// A long run of base64, outside images and PDFs, is usually a hidden payload.
	reBlob    = regexp.MustCompile(`[A-Za-z0-9+/]{240,}={0,2}`)
	reNetwork = regexp.MustCompile(`(?i)(\bcurl\b|\bwget\b|\bnc\b|\bncat\b|\bnetcat\b|\bscp\b|\brequests\.(post|get|put)|\burllib\b|http\.client|\bfetch\(|\baxios\b|XMLHttpRequest|invoke-webrequest|https?://|/dev/tcp/|\bsocket\.)`)
)

var imageExt = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".ico": true, ".svg": true, ".pdf": true}

// Scan looks through every file under dir for scripts that pipe a download
// into a shell, send the environment or credential files over the network,
// write outside the package folder, escalate privileges or hide a payload
// in an encoding, and for executable binaries. It is a tripwire, not a
// sandbox: a clean scan does not make a package safe, and a finding is a
// reason to read the file before installing.
func Scan(dir string) ([]Finding, error) {
	var out []Finding
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out = append(out, scanFile(rel, data)...)
		return nil
	})
	return out, err
}

// ScanFile is Scan for one file held in memory, named rel.
func ScanFile(rel string, data []byte) []Finding { return scanFile(rel, data) }

func scanFile(rel string, data []byte) []Finding {
	if isExecutable(data) {
		return []Finding{{File: rel, Rule: "binary", Text: "an executable program"}}
	}
	head := data[:min(len(data), 8000)]
	if bytes.IndexByte(head, 0) >= 0 || len(data) > maxScanFile {
		return nil // other binary data, or too large to be a script
	}
	ext := strings.ToLower(filepath.Ext(rel))
	var out []Finding
	seen := map[string]bool{}
	add := func(line int, rule, text string) {
		key := fmt.Sprint(line, rule)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, Finding{File: rel, Line: line, Rule: rule, Text: shorten(strings.TrimSpace(text), 160)})
	}
	lines := strings.Split(string(data), "\n")
	network := reNetwork.Match(data)
	for i, line := range lines {
		for _, r := range lineRules {
			if r.re.MatchString(line) {
				add(i+1, r.name, line)
			}
		}
		if !imageExt[ext] && reBlob.MatchString(line) {
			add(i+1, "obfuscation", line)
		}
		if network && reEnvDump.MatchString(line) {
			add(i+1, "env-exfiltration", line)
		}
		if network && reSecrets.MatchString(line) {
			add(i+1, "credential-files", line)
		}
	}
	slices.SortStableFunc(out, func(a, b Finding) int { return a.Line - b.Line })
	return out
}

func isExecutable(data []byte) bool {
	for _, magic := range [][]byte{
		{0x7f, 'E', 'L', 'F'},
		{0xcf, 0xfa, 0xed, 0xfe}, {0xce, 0xfa, 0xed, 0xfe}, // Mach-O
		{0xfe, 0xed, 0xfa, 0xcf}, {0xfe, 0xed, 0xfa, 0xce},
	} {
		if bytes.HasPrefix(data, magic) {
			return true
		}
	}
	// PE: "MZ" and a "PE\0\0" header where the DOS stub points.
	if len(data) > 0x40 && bytes.HasPrefix(data, []byte("MZ")) {
		off := int(data[0x3c]) | int(data[0x3d])<<8 | int(data[0x3e])<<16 | int(data[0x3f])<<24
		if off > 0 && off+4 <= len(data) && bytes.Equal(data[off:off+4], []byte("PE\x00\x00")) {
			return true
		}
	}
	return false
}

func shorten(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// Summaries renders findings one per line.
func Summaries(fs []Finding) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.String()
	}
	return out
}
