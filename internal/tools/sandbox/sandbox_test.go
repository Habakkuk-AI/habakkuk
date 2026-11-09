package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/platform"
	"github.com/habakkuk-ai/habakkuk/internal/testutil"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

func TestShellQuoting(t *testing.T) {
	testutil.UnixOnly(t, "runs /bin/sh to check its quoting")
	for _, s := range []string{"plain", "it's", `a "b" $c`, "line\nbreak", ""} {
		out, err := exec.Command("sh", "-c", "printf %s "+shq(s)).Output()
		if err != nil || string(out) != s {
			t.Fatalf("shq(%q) printed %q (%v)", s, out, err)
		}
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The wrapper and kill script, run by the local sh, behave as they will in
// a container or on a host: exit status passes through, the pid file goes
// away, and the kill script ends the command's children.
func TestWrapperAndKillScript(t *testing.T) {
	testutil.UnixOnly(t, "runs the wrapper and kill scripts with the local /bin/sh")
	dir := t.TempDir()
	pf := filepath.Join(dir, "x.pid")
	out, err := exec.Command("sh", "-c", wrapper(pf, "echo out; exit 7")).CombinedOutput()
	var exitErr *exec.ExitError
	if !strings.Contains(string(out), "out") || err == nil || !errorsAs(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("out %q err %v", out, err)
	}
	if _, err := os.Stat(pf); err == nil {
		t.Fatal("pid file left behind")
	}

	child := filepath.Join(dir, "child.pid")
	cmd := exec.Command("sh", "-c", wrapper(pf, "sleep 60 & echo $! > "+shq(child)+"; wait"))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var pid int
	waitUntil(t, "child pid", func() bool {
		b, err := os.ReadFile(child)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		_, perr := os.Stat(pf)
		return err == nil && pid > 0 && perr == nil
	})
	if out, err := exec.Command("sh", "-c", killScript(pf)).CombinedOutput(); err != nil {
		t.Fatalf("kill script: %v %s", err, out)
	}
	_ = cmd.Wait()
	// Without setsid (macOS) only the wrapper's bash dies; its sleep is
	// reparented. With setsid the whole group goes.
	if _, err := exec.LookPath("setsid"); err == nil {
		waitUntil(t, "child gone", func() bool { return !platform.Alive(pid) })
	} else {
		_ = platform.Kill(pid)
	}
	if _, err := os.Stat(pf); err == nil {
		t.Fatal("kill script left the pid file")
	}
}

func errorsAs(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	*target = e
	return ok
}

func TestDockerArgs(t *testing.T) {
	d := &Docker{Agent: "my agent", Workspace: "/work/space", Image: "img:1", Network: false, CPUs: "2", Memory: "1g", User: "501:20"}
	if d.Container() != "habakkuk-my_agent" {
		t.Fatalf("name %q", d.Container())
	}
	run := d.RunArgs()
	want := []string{"run", "-d", "--name", "habakkuk-my_agent", "--init", "--label", "habakkuk.agent=my agent",
		"--label", "habakkuk.config=" + d.ConfigHash(), "-v", "/work/space:" + d.containerPath("/work/space"), "-w", d.containerPath("/work/space"), "-e", "HOME=/tmp",
		"--network", "none", "--cpus", "2", "--memory", "1g", "--user", "501:20", "img:1", "sleep", "infinity"}
	if !slices.Equal(run, want) {
		t.Fatalf("run args\n got %q\nwant %q", run, want)
	}
	on := &Docker{Agent: "a", Workspace: "/w", Network: true}
	if r := on.RunArgs(); slices.Contains(r, "--network") || !slices.Contains(r, DefaultImage) || slices.Contains(r, "--user") {
		t.Fatalf("defaults: %q", r)
	}
	if on.ConfigHash() == (&Docker{Agent: "a", Workspace: "/w", Network: false}).ConfigHash() {
		t.Fatal("network change does not change the config hash")
	}
	ex := d.ExecArgs("echo hi", "/work/space/sub", "/tmp/p.pid", true)
	if !slices.Equal(ex[:6], []string{"exec", "-i", "-w", d.containerPath("/work/space/sub"), "habakkuk-my_agent", "sh"}) || ex[6] != "-c" ||
		!strings.Contains(ex[7], "'echo hi'") || !strings.Contains(ex[7], "'/tmp/p.pid'") {
		t.Fatalf("exec args %q", ex)
	}
	if ex := d.ExecArgs("x", "/w", "/p", false); slices.Contains(ex, "-i") {
		t.Fatal("-i without stdin")
	}
	if k := d.KillArgs("/tmp/p.pid"); !slices.Equal(k[:4], []string{"exec", "habakkuk-my_agent", "sh", "-c"}) || !strings.Contains(k[4], "kill -KILL") {
		t.Fatalf("kill args %q", k)
	}
	if d.Remote() {
		t.Fatal("docker shares the workspace; it is not remote")
	}
}

// On Unix the workspace is mounted at its own path; on Windows, whose
// C:\ paths a Linux container cannot use, under /workspace.
func TestDockerContainerPath(t *testing.T) {
	d := &Docker{Workspace: "/work/space"}
	if got := d.containerPathFor("linux", "/work/space/sub"); got != "/work/space/sub" {
		t.Fatalf("unix: %s", got)
	}
	ws := filepath.Join(t.TempDir(), "ws")
	w := &Docker{Workspace: ws}
	for in, want := range map[string]string{
		ws:                          "/workspace",
		filepath.Join(ws, "a", "b"): "/workspace/a/b",
		filepath.Dir(ws):            filepath.Dir(ws), // outside: left alone
	} {
		if got := w.containerPathFor("windows", in); got != want {
			t.Errorf("windows %s: %s, want %s", in, got, want)
		}
	}
}

func TestSSHArgs(t *testing.T) {

	s := &SSH{Host: "build.example", Port: 2222, User: "ci", KeyFile: "/k/id", Workdir: "/srv/work", Workspace: "/home/me/ws", ControlDir: "/c"}
	base := s.BaseArgs(true)
	want := []string{"-T", "-o", "BatchMode=yes", "-o", "ControlMaster=auto", "-o", "ControlPath=/c/%C", "-o", "ControlPersist=10m",
		"-o", "ServerAliveInterval=30", "-o", "ServerAliveCountMax=3", "-n", "-p", "2222", "-i", "/k/id", "-o", "IdentitiesOnly=yes",
		"-l", "ci", "build.example"}
	if platform.IsWindows { // Windows OpenSSH has no ControlMaster
		want = slices.Concat(want[:3], want[9:])
	}

	if !slices.Equal(base, want) {
		t.Fatalf("base\n got %q\nwant %q", base, want)
	}
	if slices.Contains(s.BaseArgs(false), "-n") {
		t.Fatal("-n with stdin")
	}
	cases := map[string]string{"/home/me/ws": "/srv/work", "/home/me/ws/a/b": "/srv/work/a/b", "/opt/x": "/opt/x", "/home/me/wsx": "/home/me/wsx"}
	for local, remote := range cases {
		if got := s.RemoteDir(local); got != remote {
			t.Errorf("RemoteDir(%q) = %q, want %q", local, got, remote)
		}
	}
	args := s.CommandArgs("make test", "/home/me/ws/app", "/tmp/p.pid", false)
	last := args[len(args)-1]
	if args[len(args)-2] != "build.example" || !strings.HasPrefix(last, "cd '/srv/work/app' && ") || !strings.Contains(last, "'make test'") {
		t.Fatalf("command args %q", args)
	}
	if k := s.KillArgs("/tmp/p.pid"); !strings.Contains(k[len(k)-1], "kill -KILL") {
		t.Fatalf("kill args %q", k)
	}
	if !s.Remote() || !strings.Contains(s.Describe(), "ci@build.example") {
		t.Fatal("ssh is remote")
	}
}

// The ssh backend end to end against a host named in HABAKKUK_TEST_SSH_HOST
// (user@host), with HABAKKUK_TEST_SSH_WORKDIR as the remote directory.
func TestSSHEndToEnd(t *testing.T) {
	host := os.Getenv("HABAKKUK_TEST_SSH_HOST")
	if host == "" {
		t.Skip("set HABAKKUK_TEST_SSH_HOST (and HABAKKUK_TEST_SSH_WORKDIR) to test against a real host")
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh is not installed")
	}
	user, h, ok := strings.Cut(host, "@")
	if !ok {
		user, h = "", host
	}
	ctl, _ := os.MkdirTemp("", "fssh")
	defer os.RemoveAll(ctl)
	ws := t.TempDir()
	s := &SSH{Host: h, User: user, Workdir: os.Getenv("HABAKKUK_TEST_SSH_WORKDIR"), Workspace: ws, ControlDir: ctl}
	if s.Workdir == "" {
		s.Workdir = "/tmp"
	}
	defer s.Close()
	out, code, err := tools.RunCommand(context.Background(), s, "pwd; exit 4", ws, 30*time.Second)
	if err != nil || code != 4 || strings.TrimSpace(out) != s.Workdir {
		t.Fatalf("out %q code %d err %v", out, code, err)
	}
	start := time.Now()
	_, _, err = tools.RunCommand(context.Background(), s, "sleep 60", ws, time.Second)
	if err == nil || time.Since(start) > 20*time.Second {
		t.Fatalf("timeout: %v after %s", err, time.Since(start))
	}
}

func dockerRig(t *testing.T) *Docker {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		t.Skip("the docker daemon is not running")
	}
	image := os.Getenv("HABAKKUK_TEST_DOCKER_IMAGE")
	if image == "" {
		image = "debian:bookworm-slim"
	}
	if err := exec.CommandContext(ctx, "docker", "image", "inspect", image).Run(); err != nil {
		t.Skipf("image %s is not present locally (set HABAKKUK_TEST_DOCKER_IMAGE to one that is)", image)
	}
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := &Docker{Agent: "test-" + strconv.Itoa(os.Getpid()), Workspace: ws, Image: image,
		User: strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())}
	t.Cleanup(func() { _ = d.Remove(context.Background()) })
	return d
}

func TestDockerEndToEnd(t *testing.T) {
	d := dockerRig(t)
	ctx := context.Background()
	out, code, err := tools.RunCommand(ctx, d, "pwd; echo hi > made.txt; exit 3", d.Workspace, time.Minute)
	if err != nil || code != 3 || strings.TrimSpace(out) != d.Workspace {
		t.Fatalf("out %q code %d err %v", out, code, err)
	}
	// The bind mount: the file is in the local workspace.
	if b, err := os.ReadFile(filepath.Join(d.Workspace, "made.txt")); err != nil || string(b) != "hi\n" {
		t.Fatalf("made.txt: %q %v", b, err)
	}
	// No network unless asked.
	out, _, _ = tools.RunCommand(ctx, d, "cat /sys/class/net/*/operstate 2>/dev/null | wc -l; ls /sys/class/net", d.Workspace, time.Minute)
	if strings.Contains(out, "eth0") {
		t.Fatalf("network is on: %s", out)
	}
	// A timeout kills the command inside the container, not just the CLI.
	start := time.Now()
	_, _, err = tools.RunCommand(ctx, d, "sleep 300 & sleep 300", d.Workspace, time.Second)
	if err == nil || time.Since(start) > 30*time.Second {
		t.Fatalf("timeout: %v after %s", err, time.Since(start))
	}
	waitUntil(t, "sleeps killed in the container", func() bool { return !inContainer(t, d, "sleep300") })
}

// inContainer reports whether a process whose command line, spaces
// removed, contains s runs in the container.
func inContainer(t *testing.T, d *Docker, s string) bool {
	t.Helper()
	out, _, err := tools.RunCommand(context.Background(), d, `for f in /proc/[0-9]*/cmdline; do tr -d '\000' < $f 2>/dev/null; echo; done`, d.Workspace, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(out, s)
}

func TestDockerBackgroundProcess(t *testing.T) {
	d := dockerRig(t)
	ps, err := tools.NewProcs(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ps.Close()
	env := &tools.Env{Agent: "a", Workspace: d.Workspace}
	p, err := ps.Start(context.Background(), env, d, "read l; echo got $l", d.Workspace, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Write("hello\n", true); err != nil {
		t.Fatal(err)
	}
	if !p.Wait(context.Background(), time.Minute) {
		t.Fatal("process did not exit")
	}
	c, _ := p.Output(0, 1000)
	if strings.TrimSpace(string(c.Data)) != "got hello" {
		t.Fatalf("output %q", c.Data)
	}
	p, err = ps.Start(context.Background(), env, d, "sleep 301; true", d.Workspace, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "sleep running in the container", func() bool { return inContainer(t, d, "sleep301") })
	if !p.Kill(30 * time.Second) {
		t.Fatal("kill did not end the process")
	}
	waitUntil(t, "sleep killed in the container", func() bool { return !inContainer(t, d, "sleep301") })
}

// Changing the settings recreates the container.
func TestDockerRecreatesOnConfigChange(t *testing.T) {
	d := dockerRig(t)
	ctx := context.Background()
	if _, _, err := tools.RunCommand(ctx, d, "touch /tmp/marker", d.Workspace, time.Minute); err != nil {
		t.Fatal(err)
	}
	d.Memory, d.checked = "256m", time.Time{}
	out, _, err := tools.RunCommand(ctx, d, "ls /tmp/marker 2>&1 || true", d.Workspace, time.Minute)
	if err != nil || !strings.Contains(out, "No such file") {
		t.Fatalf("container kept across a config change: %q %v", out, err)
	}
}
