package main

import (
	"bytes"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/remote"
)

func TestParseGlobals(t *testing.T) {
	for _, c := range []struct {
		args string
		want globals
		rest string
		err  bool
	}{
		{"chat", globals{}, "chat", false},
		{"-r srv chat -agent ops", globals{remote: "srv"}, "chat -agent ops", false},
		{"--remote srv -q status", globals{remote: "srv", quiet: true}, "status", false},
		{"--remote=srv --yes-remote update", globals{remote: "srv", yesRemote: true}, "update", false},
		{"-r=srv --quiet run -r x", globals{remote: "srv", quiet: true}, "run -r x", false},
		{"-q", globals{quiet: true}, "", false},
		{"-h", globals{}, "-h", false},
		{"--help", globals{}, "--help", false},
		{"-r", globals{}, "", true},
		{"-r -q status", globals{}, "", true},
		{"-r= status", globals{}, "", true},
		{"-q=1 status", globals{}, "", true},
	} {
		g, rest, err := parseGlobals(strings.Fields(c.args))
		if (err != nil) != c.err {
			t.Errorf("%q: err %v", c.args, err)
			continue
		}
		if c.err {
			continue
		}
		if g != c.want || strings.Join(rest, " ") != c.rest {
			t.Errorf("%q: got %+v %q, want %+v %q", c.args, g, rest, c.want, c.rest)
		}
	}
}

// useRegistry points the CLI at a registry in a temporary directory.
func useRegistry(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "habakkuk", remote.FileName)
	old := remotesPath
	remotesPath = func() (string, error) { return path, nil }
	t.Cleanup(func() { remotesPath = old })
	return path
}

func TestRemoteCmd(t *testing.T) {
	path := useRegistry(t)
	run := func(args ...string) (string, error) {
		var out, errOut bytes.Buffer
		err := remoteCmd(args, &out, &errOut)
		return out.String() + errOut.String(), err
	}
	if out, err := run(); err != nil || !strings.Contains(out, "No remotes") {
		t.Fatalf("empty list: %q %v", out, err)
	}
	if out, err := run("add", "srv", "ssh://ay@srv.example.com", "-home", "/var/lib/habakkuk", "-identity", "/k/id"); err != nil || !strings.Contains(out, "hk -r srv status") {
		t.Fatalf("add: %q %v", out, err)
	}
	if _, err := run("add", "-token-env", "HK_TOKEN", "web", "https://hk.example.com", "-default"); err != nil {
		t.Fatalf("add with flags first: %v", err)
	}
	if _, err := run("add", "web", "https://other.example.com"); err == nil {
		t.Fatal("adding an existing name without -force should fail")
	}
	if _, err := run("add", "web", "https://hk.example.com", "-token-env", "HK_TOKEN", "-force", "-default"); err != nil {
		t.Fatalf("add -force: %v", err)
	}
	for _, bad := range [][]string{{"add", "srv"}, {"add", "x", "ssh://x", "extra"}, {"rm"}, {"default", "a", "b"}, {"nope"}} {
		if _, err := run(bad...); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
	out, err := run("list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"srv", "ssh://ay@srv.example.com", "home=/var/lib/habakkuk", "identity=/k/id", "* ", "web", "token-env=HK_TOKEN"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
	reg, err := remote.Load(path)
	if err != nil || reg.Default != "web" || len(reg.Remotes) != 2 {
		t.Fatalf("saved registry: %+v %v", reg, err)
	}
	if out, _ := run("default"); strings.TrimSpace(out) != "web" {
		t.Fatalf("default: %q", out)
	}
	if _, err := run("default", "srv"); err != nil {
		t.Fatal(err)
	}
	if _, err := run("rm", "srv"); err != nil {
		t.Fatal(err)
	}
	if out, _ := run("default"); !strings.Contains(out, "No default") {
		t.Fatalf("removing the default remote clears it: %q", out)
	}
	if _, err := run("default", "none"); err != nil {
		t.Fatal(err)
	}
	if out, _ := run("help"); !strings.Contains(out, "hk remote add NAME TARGET") {
		t.Fatalf("help: %q", out)
	}
}

func TestDispatchRemote(t *testing.T) {
	path := useRegistry(t)
	reg := &remote.Registry{Remotes: map[string]remote.Remote{}}
	if err := reg.Add("srv", remote.Remote{Target: "ssh://ay@srv"}, false); err != nil {
		t.Fatal(err)
	}
	dispatch := func(env string, args ...string) ([]string, int, bool, string) {
		var out, errOut bytes.Buffer
		getenv := func(k string) string {
			if k == "HABAKKUK_REMOTE" {
				return env
			}
			return ""
		}
		rest, code, done := dispatchRemote(args, remote.Stdio{In: strings.NewReader(""), Out: &out, Err: &errOut}, getenv)
		return rest, code, done, out.String() + errOut.String()
	}

	// No registry file and nothing asked for: local, global flags removed.
	if rest, _, done, _ := dispatch("", "-q", "status"); done || !slices.Equal(rest, []string{"status"}) {
		t.Fatalf("local: %q done=%v", rest, done)
	}
	if _, code, done, out := dispatch("", "-r", "srv", "status"); !done || code != 2 || !strings.Contains(out, "unknown remote") {
		t.Fatalf("unknown remote: %d %v %q", code, done, out)
	}
	if err := reg.Save(path); err != nil {
		t.Fatal(err)
	}
	// hk remote runs here even with a remote selected.
	if _, code, done, out := dispatch("srv", "-r", "srv", "remote", "list"); !done || code != 0 || !strings.Contains(out, "ssh://ay@srv") {
		t.Fatalf("remote list: %d %v %q", code, done, out)
	}
	// Commands that change the server's install need --yes-remote; the
	// refusal comes before ssh is run.
	for _, args := range [][]string{{"-r", "srv", "update"}, {"-r", "srv", "serve"}, {"-r", "srv", "setup", "service"}} {
		_, code, done, out := dispatch("", args...)
		if !done || code != 2 || !strings.Contains(out, "hk -r srv --yes-remote "+strings.Join(args[2:], " ")) {
			t.Errorf("%q: %d %v %q", args, code, done, out)
		}
	}
	if _, code, done, out := dispatch("srv", "update"); !done || code != 2 || !strings.Contains(out, "--yes-remote") {
		t.Fatalf("HABAKKUK_REMOTE selects the remote: %d %v %q", code, done, out)
	}
	// local overrides the variable and the default.
	reg.Default = "srv"
	if err := reg.Save(path); err != nil {
		t.Fatal(err)
	}
	if rest, _, done, _ := dispatch("srv", "-r", "local", "update"); done || !slices.Equal(rest, []string{"update"}) {
		t.Fatalf("-r local: %q done=%v", rest, done)
	}
	if rest, _, done, _ := dispatch("local", "update"); done || !slices.Equal(rest, []string{"update"}) {
		t.Fatalf("HABAKKUK_REMOTE=local: %q done=%v", rest, done)
	}
	if _, code, done, _ := dispatch("", "update"); !done || code != 2 {
		t.Fatalf("the default remote is used: %d %v", code, done)
	}
	// No command prints the local usage.
	if rest, _, done, _ := dispatch("srv", "-r", "srv"); done || len(rest) != 0 {
		t.Fatalf("no command: %q done=%v", rest, done)
	}
	if _, code, done, out := dispatch("", "-r"); !done || code != 2 || !strings.Contains(out, "needs a remote name") {
		t.Fatalf("-r without a name: %d %v %q", code, done, out)
	}
}
