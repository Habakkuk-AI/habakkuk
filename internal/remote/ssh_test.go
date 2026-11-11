package remote

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/testutil"
)

// nasty are arguments a remote command must pass through unchanged.
var nasty = []string{
	"", "plain", "two words", "it's", `say "hi"`, "$HOME", "${PATH}", "$(id)", "`id`",
	"a;b", "a && b", "a|b", "*", "?", "[ab]", "~", "~/x", "=x", "a=b", "-x", "--flag=v",
	"back\\slash", "new\nline", "tab\there", "!bang", "#hash", "café ✓", "'", "''", `\'`,
	"%s %d", "{a,b}", ">out", "<in", "&",
}

// splitPOSIX splits a command line the way a POSIX shell does for the
// quoting Quote produces: words separated by blanks, '...' literal,
// backslash escaping one character outside quotes.
func splitPOSIX(t *testing.T, s string) []string {
	t.Helper()
	var words []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case ' ', '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		case '\'':
			inWord = true
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				t.Fatalf("unterminated quote in %q", s)
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
		case '\\':
			inWord = true
			if i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
			}
		default:
			if strings.IndexByte("\"$`;&|<>(){}*?[]~#!\n", c) >= 0 && !inWord {
				t.Fatalf("unquoted special %q starts a word in %q", c, s)
			}
			if strings.IndexByte("\"$`;&|<>()*?[\n", c) >= 0 {
				t.Fatalf("unquoted special %q in %q", c, s)
			}
			inWord = true
			cur.WriteByte(c)
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words
}

func TestQuote(t *testing.T) {
	for in, want := range map[string]string{
		"":          "''",
		"status":    "status",
		"-agent":    "-agent",
		"a=b":       "a=b",
		"two words": "'two words'",
		"it's":      `'it'\''s'`,
		"$HOME":     "'$HOME'",
		"~":         "'~'",
		"=x":        "'=x'",
		"a;b":       "'a;b'",
	} {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %s, want %s", in, got, want)
		}
	}
	for _, s := range nasty {
		if got := splitPOSIX(t, Quote(s)); len(got) != 1 || got[0] != s {
			t.Errorf("Quote(%q) = %s splits to %q", s, Quote(s), got)
		}
	}
}

func TestRemoteCommandWords(t *testing.T) {
	args := append([]string{"run"}, nasty...)
	words := splitPOSIX(t, RemoteCommand(Remote{Home: "/srv/my home"}, args))
	want := append([]string{"sh", "-c", script, "habakkuk", "habakkuk", "/srv/my home"}, args...)
	if !slices.Equal(words, want) {
		t.Fatalf("words:\n%q\nwant\n%q", words, want)
	}
	words = splitPOSIX(t, RemoteCommand(Remote{Bin: "/opt/hk/habakkuk"}, []string{"status"}))
	if words[4] != "/opt/hk/habakkuk" || words[5] != "" || words[6] != "status" {
		t.Fatalf("bin and empty home: %q", words)
	}
}

// TestQuoteShell checks Quote against the login shells a server may have.
func TestQuoteShell(t *testing.T) {
	testutil.UnixOnly(t, "runs POSIX shells")
	line := `printf '%s\0'`
	for _, s := range nasty {
		line += " " + Quote(s)
	}
	for _, sh := range []string{"sh", "bash", "dash", "zsh"} {
		if _, err := exec.LookPath(sh); err != nil {
			continue
		}
		out, err := exec.Command(sh, "-c", line).Output()
		if err != nil {
			t.Fatalf("%s: %v", sh, err)
		}
		got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
		if !slices.Equal(got, nasty) {
			t.Errorf("%s saw\n%q\nwant\n%q", sh, got, nasty)
		}
	}
}

func TestSSHArgs(t *testing.T) {
	tg, err := ParseTarget("ssh://sam@srv.example.com:2222")
	if err != nil {
		t.Fatal(err)
	}
	rem := Remote{Target: tg.String(), Identity: "/home/sam/.ssh/id_srv"}
	got := SSHArgs(rem, tg, []string{"status"}, false)
	want := []string{"-T", "-p", "2222", "-i", "/home/sam/.ssh/id_srv", "-o", "IdentitiesOnly=yes", "--", "sam@srv.example.com"}
	if !slices.Equal(got[:len(got)-1], want) {
		t.Fatalf("args %q, want %q + command", got, want)
	}
	if got := SSHArgs(Remote{}, Target{Kind: SSH, Host: "srv"}, []string{"chat"}, true); got[0] != "-t" || got[2] != "srv" || len(got) != 4 {
		t.Fatalf("tty args %q", got)
	}
}

func TestWantTTY(t *testing.T) {
	in, out := "stdin", "stdout"
	terms := map[any]bool{}
	isTerm := func(f any) bool { return terms[f] }
	for _, c := range []struct {
		in, out, want bool
	}{{true, true, true}, {true, false, false}, {false, true, false}, {false, false, false}} {
		terms[in], terms[out] = c.in, c.out
		if got := WantTTY(in, out, isTerm); got != c.want {
			t.Errorf("stdin terminal %v, stdout terminal %v: tty %v, want %v", c.in, c.out, got, c.want)
		}
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if WantTTY(r, w, IsTerminal) {
		t.Error("pipes are not terminals")
	}
	if IsTerminal(&bytes.Buffer{}) {
		t.Error("a buffer is not a terminal")
	}
}

// fakeSSHEnv puts a fake ssh first on PATH and returns the file its
// arguments are recorded in.
func fakeSSHEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	installFake(t, dir, "ssh")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(fakeEnv, "1")
	log := filepath.Join(t.TempDir(), "ssh.json")
	t.Setenv("HK_REMOTE_TEST_SSH_LOG", log)
	t.Setenv("HK_REMOTE_TEST_SHELL", "")
	t.Setenv("HK_REMOTE_TEST_EXIT", "")
	return log
}

func TestRunSSHForwardsExitCode(t *testing.T) {
	log := fakeSSHEnv(t)
	t.Setenv("HK_REMOTE_TEST_EXIT", "3")
	rem := Remote{Target: "ssh://sam@srv", Home: "~/hk"}
	args := []string{"run", "what's $HOME; rm -rf *"}
	var out, errOut bytes.Buffer
	code, err := RunSSH(rem, args, false, Stdio{In: strings.NewReader(""), Out: &out, Err: &errOut})
	if err != nil || code != 3 {
		t.Fatalf("code %d, err %v, want 3", code, err)
	}
	var rec sshRecord
	readRecord(t, log, &rec)
	if rec.Args[0] != "-T" || rec.Args[len(rec.Args)-2] != "sam@srv" {
		t.Fatalf("ssh args %q", rec.Args)
	}
	words := splitPOSIX(t, rec.Args[len(rec.Args)-1])
	if !slices.Equal(words[5:], append([]string{"~/hk"}, args...)) {
		t.Fatalf("remote words %q", words)
	}

	t.Setenv("HK_REMOTE_TEST_EXIT", "0")
	if code, err := RunSSH(rem, []string{"status"}, true, Stdio{In: strings.NewReader("")}); err != nil || code != 0 {
		t.Fatalf("code %d, err %v, want 0", code, err)
	}
	readRecord(t, log, &rec)
	if rec.Args[0] != "-t" {
		t.Fatalf("tty: ssh args %q", rec.Args)
	}
}

func TestRunSSHNoClient(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := RunSSH(Remote{Target: "ssh://srv"}, []string{"status"}, false, Stdio{})
	if !errors.Is(err, ErrNoSSH) {
		t.Fatalf("err %v, want ErrNoSSH", err)
	}
	if _, err := RunSSH(Remote{Target: "https://srv"}, []string{"status"}, false, Stdio{}); err == nil {
		t.Fatal("an https remote over ssh should fail")
	}
}

// shellEnv runs the remote command through sh, as a server does, with a
// fake habakkuk in hkDir (when not empty) and HOME at home.
func shellEnv(t *testing.T, hkDir, home string) string {
	t.Helper()
	testutil.UnixOnly(t, "the fake server runs the command with /bin/sh")
	sshDir := t.TempDir()
	installFake(t, sshDir, "ssh")
	path := sshDir
	if hkDir != "" {
		installFake(t, hkDir, "habakkuk")
		path += string(os.PathListSeparator) + hkDir
	}
	t.Setenv("PATH", path+string(os.PathListSeparator)+"/usr/bin:/bin")
	t.Setenv("HOME", home)
	t.Setenv(fakeEnv, "1")
	t.Setenv("HK_REMOTE_TEST_SSH_LOG", filepath.Join(t.TempDir(), "ssh.json"))
	t.Setenv("HK_REMOTE_TEST_SHELL", "1")
	t.Setenv("HABAKKUK_HOME", "")
	log := filepath.Join(t.TempDir(), "hk.json")
	t.Setenv("HK_REMOTE_TEST_HK_LOG", log)
	t.Setenv("HK_REMOTE_TEST_HK_EXIT", "")
	return log
}

func TestRunSSHRoundTrip(t *testing.T) {
	home := t.TempDir()
	log := shellEnv(t, t.TempDir(), home)
	t.Setenv("HK_REMOTE_TEST_HK_EXIT", "7")
	args := append([]string{"run", "-agent", "ops"}, nasty...)
	code, err := RunSSH(Remote{Target: "ssh://sam@srv", Home: "~/hk home"}, args, false,
		Stdio{In: strings.NewReader("piped input"), Out: os.Stdout, Err: os.Stderr})
	if err != nil || code != 7 {
		t.Fatalf("code %d, err %v, want 7 from the remote command", code, err)
	}
	var rec hkRecord
	readRecord(t, log, &rec)
	if !slices.Equal(rec.Args, args) {
		t.Fatalf("habakkuk got\n%q\nwant\n%q", rec.Args, args)
	}
	if want := filepath.Join(home, "hk home"); rec.Home != want {
		t.Fatalf("HABAKKUK_HOME %q, want %q", rec.Home, want)
	}
	if rec.Stdin != "piped input" {
		t.Fatalf("stdin %q", rec.Stdin)
	}

	// Without -home the server's environment decides.
	t.Setenv("HK_REMOTE_TEST_HK_EXIT", "0")
	t.Setenv("HABAKKUK_HOME", "/from/the/server")
	if code, err := RunSSH(Remote{Target: "ssh://srv"}, []string{"status"}, false, Stdio{In: strings.NewReader("")}); err != nil || code != 0 {
		t.Fatalf("code %d, err %v", code, err)
	}
	readRecord(t, log, &rec)
	if rec.Home != "/from/the/server" || !slices.Equal(rec.Args, []string{"status"}) {
		t.Fatalf("got %+v", rec)
	}
}

func TestRunSSHLocalBinFallback(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	log := shellEnv(t, "", home)
	installFake(t, bin, "habakkuk")
	code, err := RunSSH(Remote{Target: "ssh://srv"}, []string{"version"}, false, Stdio{In: strings.NewReader("")})
	if err != nil || code != 0 {
		t.Fatalf("code %d, err %v", code, err)
	}
	var rec hkRecord
	readRecord(t, log, &rec)
	if !slices.Equal(rec.Args, []string{"version"}) {
		t.Fatalf("args %q", rec.Args)
	}
}

func TestRunSSHMissingBinary(t *testing.T) {
	shellEnv(t, "", t.TempDir())
	var errOut bytes.Buffer
	code, err := RunSSH(Remote{Target: "ssh://srv"}, []string{"status"}, false, Stdio{In: strings.NewReader(""), Err: &errOut})
	if err != nil || code != 127 {
		t.Fatalf("code %d, err %v, want 127", code, err)
	}
	if !strings.Contains(errOut.String(), "-bin PATH") {
		t.Fatalf("stderr %q should say how to set the path", errOut.String())
	}
}
