package remote

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// The test binary doubles as a fake ssh client and a fake habakkuk: copied
// into a temporary directory under those names, it records what it was
// given instead of running the tests. Being a Go program, the fake ssh
// works on Windows too; only the step that hands the command to a POSIX
// shell (as a real server does) needs Unix.
const fakeEnv = "HK_REMOTE_TEST_FAKE"

func TestMain(m *testing.M) {
	if os.Getenv(fakeEnv) == "1" {
		name := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
		switch name {
		case "ssh":
			os.Exit(fakeSSH())
		case "habakkuk":
			os.Exit(fakeHabakkuk())
		}
	}
	os.Exit(m.Run())
}

type sshRecord struct {
	Args []string `json:"args"`
}

type hkRecord struct {
	Args  []string `json:"args"`
	Home  string   `json:"home"`
	Stdin string   `json:"stdin"`
}

func record(path string, v any) {
	data, _ := json.Marshal(v)
	_ = os.WriteFile(path, data, 0o600)
}

// fakeSSH records its arguments. With HK_REMOTE_TEST_SHELL=1 it runs the
// remote command with sh -c, like sshd runs it with the login shell;
// otherwise it exits with HK_REMOTE_TEST_EXIT.
func fakeSSH() int {
	record(os.Getenv("HK_REMOTE_TEST_SSH_LOG"), sshRecord{Args: os.Args[1:]})
	if os.Getenv("HK_REMOTE_TEST_SHELL") == "1" {
		cmd := exec.Command("sh", "-c", os.Args[len(os.Args)-1])
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				return ee.ExitCode()
			}
			return 255
		}
		return 0
	}
	code, _ := strconv.Atoi(os.Getenv("HK_REMOTE_TEST_EXIT"))
	return code
}

func fakeHabakkuk() int {
	in, _ := io.ReadAll(os.Stdin)
	record(os.Getenv("HK_REMOTE_TEST_HK_LOG"), hkRecord{Args: os.Args[1:], Home: os.Getenv("HABAKKUK_HOME"), Stdin: string(in)})
	code, _ := strconv.Atoi(os.Getenv("HK_REMOTE_TEST_HK_EXIT"))
	return code
}

// installFake puts a copy of the test binary named name (plus .exe on
// Windows) in dir.
func installFake(t *testing.T, dir, name string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dst := filepath.Join(dir, name)
	if err := os.Link(self, dst); err != nil {
		data, err := os.ReadFile(self)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

func readRecord(t *testing.T, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the fake was not run: %v", err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatal(err)
	}
}
