package platform

import (
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

func lookIn(found ...string) func(string) (string, error) {
	return func(name string) (string, error) {
		for _, f := range found {
			if f == name {
				return `C:\bin\` + name + ".exe", nil
			}
		}
		return "", exec.ErrNotFound
	}
}

func decodePS(t *testing.T, enc string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatal(err)
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return string(utf16.Decode(u))
}

func TestShellForWindows(t *testing.T) {
	script := `Get-ChildItem "C:\Program Files" | Select -First 1; echo 'it''s'`

	s := shellFor("windows", "", script, true, lookIn())
	if s.kind != "powershell" || !strings.HasSuffix(strings.ToLower(s.path), "powershell.exe") {
		t.Fatalf("default shell = %s %s, want Windows PowerShell", s.kind, s.path)
	}
	if n := len(s.args); n < 2 || s.args[n-2] != "-EncodedCommand" {
		t.Fatalf("args %q: want -EncodedCommand", s.args)
	}
	body := decodePS(t, s.args[len(s.args)-1])
	if !strings.Contains(body, script) || !strings.HasPrefix(body, psPrelude) || !strings.HasSuffix(body, psEpilogue) {
		t.Fatalf("encoded body %q does not wrap the script", body)
	}

	if s := shellFor("windows", "", "x", true, lookIn("pwsh")); s.kind != "pwsh" || s.path != `C:\bin\pwsh.exe` {
		t.Fatalf("with pwsh installed: %s %s", s.kind, s.path)
	}
	c := shellFor("windows", "cmd", "dir /b & echo done", false, lookIn())
	if c.kind != "cmd" || !strings.Contains(c.cmdLine, `/d /s /c "dir /b & echo done"`) {
		t.Fatalf("cmd: %+v", c)
	}
	if b := shellFor("windows", "bash", "ls", true, lookIn("bash")); b.kind != "bash" || b.args[0] != "-c" {
		t.Fatalf("bash override: %+v", b)
	}
	if b := shellFor("windows", "bash", "ls", true, lookIn()); b.kind != "powershell" {
		t.Fatalf("missing bash should fall back to PowerShell: %+v", b)
	}
	long := strings.Repeat("x", maxEncoded)
	if l := shellFor("windows", "", long, true, lookIn()); l.args[len(l.args)-2] != "-Command" {
		t.Fatalf("an over-long script should use -Command: %q", l.args[len(l.args)-2])
	}
}

func TestShellForUnix(t *testing.T) {
	if s := shellFor("linux", "", "echo hi", false, lookIn("bash")); s.kind != "sh" || s.path != "/bin/sh" || s.args[0] != "-c" || s.args[1] != "echo hi" {
		t.Fatalf("user shell: %+v", s)
	}
	if s := shellFor("darwin", "", "echo hi", true, lookIn("bash")); s.kind != "bash" || s.path != `C:\bin\bash.exe` {
		t.Fatalf("script shell: %+v", s)
	}
	if s := shellFor("linux", "", "echo hi", true, lookIn()); s.kind != "sh" || s.path != "/bin/sh" {
		t.Fatalf("no bash: %+v", s)
	}
}

func TestShellRunsHere(t *testing.T) {
	cmd := ScriptCommand("echo habakkuk-ok")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !strings.Contains(string(out), "habakkuk-ok") {
		t.Fatalf("output %q", out)
	}
	err = ShellCommand("exit 3").Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 3 {
		t.Fatalf("exit 3 gave %v", err)
	}
}

func TestCommandInDir(t *testing.T) {
	for kind, want := range map[string]string{
		"sh":         `cd '/it'\''s' && ./x.sh`,
		"powershell": `Set-Location -LiteralPath '/it''s'; ./x.sh`,
		"cmd":        `cd /d "/it's" && ./x.sh`,
	} {
		if got := commandInDir(kind, "/it's", "./x.sh"); got != want {
			t.Errorf("%s: %s, want %s", kind, got, want)
		}
	}
}

func TestDataDir(t *testing.T) {
	t.Setenv("HABAKKUK_HOME", filepath.Join("x", "home"))
	if got := DataDir(); got != filepath.Join("x", "home") {
		t.Fatalf("override ignored: %s", got)
	}
	t.Setenv("HABAKKUK_HOME", "")
	got := DataDir()
	if IsWindows {
		if filepath.Base(got) != "Habakkuk" {
			t.Fatalf("Windows data dir %s, want ...\\Habakkuk", got)
		}
	} else if filepath.Base(got) != ".habakkuk" {
		t.Fatalf("data dir %s, want ~/.habakkuk", got)
	}
}

func TestLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	a, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := Lock(a); err != nil {
		t.Fatal(err)
	}
	if ok, err := TryLock(b); err != nil || ok {
		t.Fatalf("second lock: ok=%v err=%v, want held", ok, err)
	}
	if err := Unlock(a); err != nil {
		t.Fatal(err)
	}
	if ok, err := TryLock(b); err != nil || !ok {
		t.Fatalf("after unlock: ok=%v err=%v", ok, err)
	}
}

func TestAliveAndDisk(t *testing.T) {
	if !Alive(os.Getpid()) {
		t.Fatal("this process is not alive?")
	}
	if Alive(0) || Alive(-1) {
		t.Fatal("pid 0 or -1 reported alive")
	}
	free, total, err := DiskUsage(t.TempDir())
	if err != nil || free == 0 || total < free {
		t.Fatalf("DiskUsage = %d, %d, %v", free, total, err)
	}
}
