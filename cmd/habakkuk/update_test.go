package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/app"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
)

// fakeReleases serves a GitHub-like releases API: /releases/latest is the
// newest non-prerelease, /releases lists them all. Each release carries
// archives for linux/amd64 (.tar.gz) and windows/amd64 (.zip) holding
// "binary <tag>", SHA256SUMS and, when key is set, SHA256SUMS.sig.
type fakeReleases struct {
	srv      *httptest.Server
	releases []release
	files    map[string][]byte
}

type fakeRel struct {
	tag        string
	prerelease bool
	badSum     bool               // SHA256SUMS lists a wrong hash
	key        ed25519.PrivateKey // sign SHA256SUMS
	badSig     bool               // sign something else
}

func newFakeReleases(t *testing.T, rels ...fakeRel) *fakeReleases {
	t.Helper()
	f := &fakeReleases{files: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	for _, r := range rels {
		tgz := fakeRelease(t, "binary "+r.tag)
		zipped := fakeZip(t, "habakkuk.exe", "binary "+r.tag)
		sum := func(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
		tgzSum := sum(tgz)
		if r.badSum {
			tgzSum = strings.Repeat("0", 64)
		}
		sums := []byte(tgzSum + "  habakkuk_linux_amd64.tar.gz\n" + sum(zipped) + "  habakkuk_windows_amd64.zip\n")
		base := "/dl/" + r.tag + "/"
		f.files[base+"habakkuk_linux_amd64.tar.gz"] = tgz
		f.files[base+"habakkuk_windows_amd64.zip"] = zipped
		f.files[base+"SHA256SUMS"] = sums
		rel := release{Tag: r.tag, Prerelease: r.prerelease, Assets: []asset{
			{Name: "habakkuk_linux_amd64.tar.gz", URL: f.srv.URL + base + "habakkuk_linux_amd64.tar.gz"},
			{Name: "habakkuk_windows_amd64.zip", URL: f.srv.URL + base + "habakkuk_windows_amd64.zip"},
			{Name: "SHA256SUMS", URL: f.srv.URL + base + "SHA256SUMS"},
		}}
		if r.key != nil {
			signed := sums
			if r.badSig {
				signed = []byte("something else")
			}
			f.files[base+"SHA256SUMS.sig"] = []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(r.key, signed)) + "\n")
			rel.Assets = append(rel.Assets, asset{Name: "SHA256SUMS.sig", URL: f.srv.URL + base + "SHA256SUMS.sig"})
		}
		f.releases = append(f.releases, rel)
	}
	return f
}

func (f *fakeReleases) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/releases/latest":
		var best *release
		for i := range f.releases {
			if !f.releases[i].Prerelease && (best == nil || compareVersions(f.releases[i].Tag, best.Tag) > 0) {
				best = &f.releases[i]
			}
		}
		if best == nil {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(best)
	case "/releases":
		_ = json.NewEncoder(w).Encode(f.releases)
	default:
		if b, ok := f.files[r.URL.Path]; ok {
			w.Write(b)
			return
		}
		http.NotFound(w, r)
	}
}

func fakeZip(t *testing.T, name, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte(body))
	lw, _ := zw.Create("LICENSE")
	lw.Write([]byte("MIT"))
	zw.Close()
	return buf.Bytes()
}

// upgradeHome is a home with a config and a control.db holding one job.
func upgradeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(config.Template), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, s := range app.Schemas() {
		if s.File != "control.db" && s.File != "memory.db" {
			continue
		}
		db, err := store.Open(ctx, filepath.Join(home, s.File), s.Migrations)
		if err != nil {
			t.Fatal(err)
		}
		db.Close()
	}
	return home
}

type checkCall struct{ exe, home, body string }

func newTestUpdater(f *fakeReleases, home, exe, current string, out *bytes.Buffer) (*updater, *[]checkCall) {
	var calls []checkCall
	u := &updater{API: f.srv.URL, HTTP: f.srv.Client(), OS: "linux", Arch: "amd64", Exe: exe, Current: current,
		Out: out, Home: home, Now: func() time.Time { return time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC) },
		Backup:  fullBackup,
		Restore: func(archive, home string) error { _, err := restoreBackup(archive, home, true); return err },
		Check: func(_ context.Context, exe, home string) error {
			b, _ := os.ReadFile(exe)
			calls = append(calls, checkCall{exe, home, string(b)})
			return nil
		},
		Running: func(string) (int, bool) { return 0, false },
	}
	return u, &calls
}

func writeExe(t *testing.T, body string) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "habakkuk")
	if err := os.WriteFile(exe, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func userVersion(t *testing.T, path string) int {
	t.Helper()
	v, err := store.SchemaVersion(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// An update checks the new binary against the home, backs up, swaps; a
// rollback brings back the old binary, databases and config.
func TestUpdateThenRollback(t *testing.T) {
	ctx := context.Background()
	f := newFakeReleases(t, fakeRel{tag: "v0.4.0"})
	home := upgradeHome(t)
	exe := writeExe(t, "binary v0.3.0")
	control := filepath.Join(home, "control.db")
	before := userVersion(t, control)
	var out bytes.Buffer
	u, calls := newTestUpdater(f, home, exe, "v0.3.0", &out)

	if err := u.update(ctx, updateOptions{}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if got := mustRead(t, exe); got != "binary v0.4.0" {
		t.Fatalf("exe = %q", got)
	}
	if len(*calls) != 1 || (*calls)[0].body != "binary v0.4.0" || (*calls)[0].home != home {
		t.Fatalf("upgrade check calls %+v", *calls)
	}
	if _, err := os.Stat((*calls)[0].exe); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the staged binary was left behind")
	}
	st, err := u.loadState()
	if err != nil || st.Previous == nil || st.Previous.Version != "v0.3.0" || st.Previous.ReplacedBy != "v0.4.0" {
		t.Fatalf("state %+v %v", st, err)
	}
	if got := mustRead(t, st.Previous.Binary); got != "binary v0.3.0" {
		t.Fatalf("previous binary = %q", got)
	}
	names, err := backupNames(st.Previous.Backup)
	if err != nil || !names["config.yaml"] || !names["control.db"] || !names["memory.db"] {
		t.Fatalf("backup holds %v (%v)", names, err)
	}
	if !strings.Contains(out.String(), "checksum: ok") || !strings.Contains(out.String(), "--rollback") {
		t.Fatalf("output:\n%s", out.String())
	}

	// The new version migrates the database past what the old one knows,
	// edits the config and creates a database the old one never had.
	db, err := store.Open(ctx, control, nil)
	if err != nil {
		t.Fatal(err)
	}
	db.Exec("PRAGMA user_version = 999")
	db.Close()
	os.WriteFile(filepath.Join(home, "config.yaml"), []byte(config.Template+"\n# edited by v0.4.0\n"), 0o600)
	newDB, _ := store.Open(ctx, filepath.Join(home, "nodes.db"), []store.Migration{{Version: 1, SQL: "CREATE TABLE x (y)"}})
	newDB.Close()

	out.Reset()
	if err := u.rollback(ctx, false); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if got := mustRead(t, exe); got != "binary v0.3.0" {
		t.Fatalf("after rollback exe = %q", got)
	}
	if v := userVersion(t, control); v != before {
		t.Fatalf("control.db schema %d after rollback, want %d", v, before)
	}
	if strings.Contains(mustRead(t, filepath.Join(home, "config.yaml")), "edited by v0.4.0") {
		t.Fatal("config not restored")
	}
	if _, err := os.Stat(filepath.Join(home, "nodes.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a database created after the update was left in place")
	}
	if !strings.Contains(out.String(), "moved aside nodes.db") {
		t.Fatalf("output:\n%s", out.String())
	}
	safety, _ := filepath.Glob(filepath.Join(home, "upgrades", "before-rollback-*.tar.gz"))
	if len(safety) != 1 {
		t.Fatalf("no safety backup before the rollback: %v", safety)
	}
	if names, _ := backupNames(safety[0]); !names["config.yaml"] {
		t.Fatalf("safety backup holds %v", names)
	}
	if st, _ := u.loadState(); st.Previous != nil {
		t.Fatal("rollback state not cleared")
	}
	if err := u.rollback(ctx, false); err == nil || !strings.Contains(err.Error(), "nothing to roll back") {
		t.Fatalf("second rollback: %v", err)
	}
}

func TestRollbackRefusesWhileServing(t *testing.T) {
	f := newFakeReleases(t, fakeRel{tag: "v0.4.0"})
	home := upgradeHome(t)
	exe := writeExe(t, "binary v0.3.0")
	var out bytes.Buffer
	u, _ := newTestUpdater(f, home, exe, "v0.3.0", &out)
	if err := u.update(context.Background(), updateOptions{}); err != nil {
		t.Fatal(err)
	}
	u.Running = func(string) (int, bool) { return 4242, true }
	if err := u.rollback(context.Background(), false); err == nil || !strings.Contains(err.Error(), "pid 4242") {
		t.Fatalf("err = %v", err)
	}
	if got := mustRead(t, exe); got != "binary v0.4.0" {
		t.Fatal("a refused rollback changed the binary")
	}
}

// Nothing is backed up, checked or replaced when the archive does not
// match SHA256SUMS.
func TestUpdateChecksumFailure(t *testing.T) {
	f := newFakeReleases(t, fakeRel{tag: "v0.4.0", badSum: true})
	home := upgradeHome(t)
	exe := writeExe(t, "binary v0.3.0")
	var out bytes.Buffer
	u, calls := newTestUpdater(f, home, exe, "v0.3.0", &out)
	err := u.update(context.Background(), updateOptions{})
	if err == nil || !strings.Contains(err.Error(), "does not match SHA256SUMS") {
		t.Fatalf("err = %v", err)
	}
	if mustRead(t, exe) != "binary v0.3.0" || len(*calls) != 0 {
		t.Fatal("a bad download got further than the checksum")
	}
	if backups, _ := filepath.Glob(filepath.Join(home, "upgrades", "backup-*")); len(backups) != 0 {
		t.Fatalf("backups made: %v", backups)
	}
}

// A new binary that rejects the home is never installed.
func TestUpdateAbortsWhenUpgradeCheckFails(t *testing.T) {
	f := newFakeReleases(t, fakeRel{tag: "v0.4.0"})
	home := upgradeHome(t)
	exe := writeExe(t, "binary v0.3.0")
	var out bytes.Buffer
	u, _ := newTestUpdater(f, home, exe, "v0.3.0", &out)
	u.Check = func(context.Context, string, string) error { return errors.New("control.db: FAIL") }
	err := u.update(context.Background(), updateOptions{})
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") || !strings.Contains(err.Error(), "control.db: FAIL") {
		t.Fatalf("err = %v", err)
	}
	if mustRead(t, exe) != "binary v0.3.0" {
		t.Fatal("binary replaced after a failed check")
	}
	if left, _ := filepath.Glob(filepath.Join(home, "upgrades", "habakkuk-*")); len(left) != 0 {
		t.Fatalf("staged binaries left: %v", left)
	}
	if st, _ := u.loadState(); st.Previous != nil {
		t.Fatal("rollback state written for an update that did not happen")
	}
}

func TestUpdateSignatures(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		rel  fakeRel
		key  ed25519.PublicKey
		want string // error substring, "" for success
	}{
		{"valid", fakeRel{tag: "v0.4.0", key: priv}, pub, ""},
		{"tampered", fakeRel{tag: "v0.4.0", key: priv, badSig: true}, pub, "does not match the release signing key"},
		{"unsigned with a key", fakeRel{tag: "v0.4.0"}, pub, "not signed"},
		{"signed without a key", fakeRel{tag: "v0.4.0", key: priv}, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeReleases(t, tc.rel)
			exe := writeExe(t, "binary v0.3.0")
			var out bytes.Buffer
			u, _ := newTestUpdater(f, upgradeHome(t), exe, "v0.3.0", &out)
			u.PublicKey = tc.key
			err := u.update(ctx, updateOptions{})
			switch {
			case tc.want == "" && err != nil:
				t.Fatal(err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("err = %v, want %q", err, tc.want)
			case tc.want != "" && mustRead(t, exe) != "binary v0.3.0":
				t.Fatal("binary replaced")
			}
		})
	}
	if _, err := parsePublicKey(base64.StdEncoding.EncodeToString(pub)); err != nil {
		t.Fatal(err)
	}
	if _, err := parsePublicKey("bm90IGEga2V5"); err == nil {
		t.Fatal("accepted a short key")
	}
}

// The release workflow signs with openssl pkeyutl -rawin and publishes the
// key as `openssl pkey -pubout -outform DER | tail -c 32 | base64`; these
// values came from exactly those commands.
func TestVerifySignatureFromOpenSSL(t *testing.T) {
	key, err := parsePublicKey("hxntVWuV3Rg/TYuhJS2I21kyZeZm0k6cVC4S424DIOg=")
	if err != nil {
		t.Fatal(err)
	}
	sums := []byte("abc  habakkuk_linux_amd64.tar.gz\n")
	sig := []byte("bnl4Dho/xaVnTrgpOjslNXd6l9pYng6vRnBC8oZtyiKiDBUiuCX1pZVAfO0abJQ/wkISM1WGPo03WuSXfnEwBQ==\n")
	if err := verifySignature(key, sums, sig); err != nil {
		t.Fatal(err)
	}
	if err := verifySignature(key, []byte("abd  habakkuk_linux_amd64.tar.gz\n"), sig); err == nil {
		t.Fatal("a changed SHA256SUMS verified")
	}
}

func TestUpdateChannels(t *testing.T) {
	f := newFakeReleases(t, fakeRel{tag: "v0.4.0"}, fakeRel{tag: "v0.5.0-beta.1", prerelease: true}, fakeRel{tag: "v0.3.9"})
	ctx := context.Background()
	exe := writeExe(t, "binary v0.3.0")
	home := upgradeHome(t)
	var out bytes.Buffer
	u, _ := newTestUpdater(f, home, exe, "v0.3.0", &out)
	t.Setenv("HABAKKUK_UPDATE_CHANNEL", "")

	if err := u.update(ctx, updateOptions{checkOnly: true}); err != nil || !strings.Contains(out.String(), "stable channel: v0.3.0 -> v0.4.0") {
		t.Fatalf("stable: %v %s", err, out.String())
	}
	out.Reset()
	if err := u.update(ctx, updateOptions{channel: "beta"}); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, exe); got != "binary v0.5.0-beta.1" {
		t.Fatalf("beta installed %q", got)
	}
	// The channel is remembered.
	u.Current = "v0.5.0-beta.1"
	out.Reset()
	if err := u.update(ctx, updateOptions{checkOnly: true}); err != nil || !strings.Contains(out.String(), "up to date (beta channel") {
		t.Fatalf("remembered channel: %v %s", err, out.String())
	}
	if err := u.update(ctx, updateOptions{channel: "nightly"}); err == nil {
		t.Fatal("accepted an unknown channel")
	}
}

// Before the first release GitHub answers 404 for /releases/latest (and for
// a repository with pre-releases only): -check says so and succeeds, a real
// update fails in plain words, and neither touches the binary.
func TestUpdateWithNoRelease(t *testing.T) {
	ctx := context.Background()
	t.Setenv("HABAKKUK_UPDATE_CHANNEL", "")
	for _, tc := range []struct {
		name    string
		f       *fakeReleases
		channel string
	}{
		{"nothing published", newFakeReleases(t), ""},
		{"pre-releases only", newFakeReleases(t, fakeRel{tag: "v0.4.0-rc.1", prerelease: true}), ""},
		{"empty beta channel", newFakeReleases(t), "beta"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exe := writeExe(t, "binary v0.3.0")
			var out bytes.Buffer
			u, calls := newTestUpdater(tc.f, upgradeHome(t), exe, "v0.3.0", &out)
			if err := u.update(ctx, updateOptions{checkOnly: true, channel: tc.channel}); err != nil {
				t.Fatalf("-check: %v", err)
			}
			if got := out.String(); !strings.Contains(got, "no release on the") || strings.Contains(got, "404") {
				t.Fatalf("-check printed %q", got)
			}
			err := u.update(ctx, updateOptions{channel: tc.channel})
			if !errors.Is(err, errNoRelease) || strings.Contains(err.Error(), "404") {
				t.Fatalf("update: %v", err)
			}
			if mustRead(t, exe) != "binary v0.3.0" || len(*calls) != 0 {
				t.Fatal("an update with no release touched the binary")
			}
		})
	}
}

// Other failures to reach the API still name the status and the address.
func TestUpdateReportsOtherHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusForbidden)
	}))
	defer srv.Close()
	var out bytes.Buffer
	u := &updater{API: srv.URL, HTTP: srv.Client(), OS: "linux", Arch: "amd64", Current: "v0.3.0", Out: &out, Home: t.TempDir()}
	err := u.update(context.Background(), updateOptions{checkOnly: true})
	if err == nil || errors.Is(err, errNoRelease) || !strings.Contains(err.Error(), "HTTP 403 from "+srv.URL) {
		t.Fatalf("got %v", err)
	}
}

// The Windows build ships a .zip holding habakkuk.exe.
func TestUpdateWindowsZip(t *testing.T) {
	f := newFakeReleases(t, fakeRel{tag: "v0.4.0"})
	exe := writeExe(t, "binary v0.3.0")
	var out bytes.Buffer
	u, _ := newTestUpdater(f, upgradeHome(t), exe, "v0.3.0", &out)
	u.OS = "windows"
	if err := u.update(context.Background(), updateOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, exe); got != "binary v0.4.0" {
		t.Fatalf("exe = %q", got)
	}
	if _, err := extractZipBinary(fakeZip(t, "other.exe", "x"), "habakkuk.exe"); err == nil {
		t.Fatal("a zip without habakkuk.exe was accepted")
	}
}

// swapAside is how Windows replaces a running binary; it must leave either
// the old or the new file at exe, never neither.
func TestSwapAside(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "habakkuk.exe")
	os.WriteFile(exe, []byte("old"), 0o755)
	if err := swapAside(exe, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if mustRead(t, exe) != "new" || mustRead(t, exe+".old") != "old" {
		t.Fatal("swap did not move the old binary aside")
	}
	// A second swap replaces the stale .old.
	if err := swapAside(exe, []byte("newer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if mustRead(t, exe) != "newer" || mustRead(t, exe+".old") != "new" {
		t.Fatal("second swap")
	}
	removeReplaced(exe)
	if _, err := os.Stat(exe + ".old"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(".old not cleaned up")
	}
	if mustRead(t, exe) != "newer" {
		t.Fatal("cleanup touched the binary")
	}
	// A missing exe (first install) still works.
	fresh := filepath.Join(dir, "fresh.exe")
	if err := swapAside(fresh, []byte("x"), 0o755); err != nil || mustRead(t, fresh) != "x" {
		t.Fatalf("fresh install: %v", err)
	}
}

// The new binary refuses a home whose databases are newer than it knows.
func TestUpgradeCheck(t *testing.T) {
	ctx := context.Background()
	home := upgradeHome(t)
	var out bytes.Buffer
	if err := upgradeCheck(ctx, home, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "upgrade check passed") || !strings.Contains(out.String(), "control.db: ok") {
		t.Fatalf("output:\n%s", out.String())
	}

	db, _ := store.Open(ctx, filepath.Join(home, "control.db"), nil)
	db.Exec("PRAGMA user_version = 999")
	db.Close()
	out.Reset()
	err := upgradeCheck(ctx, home, &out)
	if !errors.Is(err, errUpgradeCheck) || !strings.Contains(out.String(), "control.db: FAIL schema version 999") {
		t.Fatalf("err %v\n%s", err, out.String())
	}
	// It wrote nothing: the database is still at 999, not touched.
	if v := userVersion(t, filepath.Join(home, "control.db")); v != 999 {
		t.Fatalf("version %d", v)
	}

	// An older binary refuses to start on that home, with directions.
	_, err = app.Open(ctx, home, nil)
	var tooNew *store.SchemaTooNewError
	if !errors.As(err, &tooNew) || !strings.Contains(err.Error(), "hk update --rollback") {
		t.Fatalf("app.Open: %v", err)
	}

	// A fresh home passes.
	out.Reset()
	if err := upgradeCheck(ctx, t.TempDir(), &out); err != nil || !strings.Contains(out.String(), "fresh install") {
		t.Fatalf("fresh: %v %s", err, out.String())
	}
}

func TestConfigMigrateCmd(t *testing.T) {
	home := t.TempDir()
	old := strings.Replace(config.Template, "version: 1\n", "", 1)
	os.WriteFile(filepath.Join(home, "config.yaml"), []byte(old), 0o600)
	var out bytes.Buffer
	if err := configCmd(home, []string{"migrate"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "from version 0 to 1") {
		t.Fatalf("output %s", out.String())
	}
	baks, _ := filepath.Glob(filepath.Join(home, "config.yaml.*.bak"))
	if len(baks) != 1 || mustRead(t, baks[0]) != old {
		t.Fatalf("backups %v", baks)
	}
	out.Reset()
	if err := configCmd(home, []string{"migrate"}, &out); err != nil || !strings.Contains(out.String(), "already version 1") {
		t.Fatalf("second run: %v %s", err, out.String())
	}
}

func TestVersionCmd(t *testing.T) {
	home := upgradeHome(t)
	var out bytes.Buffer
	if err := versionCmd(context.Background(), home, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"habakkuk " + version, "commit:", "built:", "control.db", "nodes.db", "not created",
		fmt.Sprintf("format version %d", config.CurrentVersion)} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("version output lacks %q:\n%s", want, out.String())
		}
	}
}
