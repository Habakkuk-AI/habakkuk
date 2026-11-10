package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	plat "github.com/habakkuk-ai/habakkuk/internal/core/platform"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
)

// An update is meant to be boring and reversible:
//
//  1. find the release on the channel (stable or beta), download this
//     platform's archive and verify it against SHA256SUMS (and its
//     signature when the release is signed);
//  2. stage the new binary under $HABAKKUK_HOME/upgrades and run its
//     `doctor --upgrade-check` against the current home, which loads the
//     config and reads every database's schema version without writing
//     anything; if the new binary objects, nothing has changed;
//  3. back up config, secrets and every database (habakkuk backup with
//     -env, plus memory, usage and nodes), and keep a copy of the current
//     binary;
//  4. swap the binary in one step (installBinary).
//
// `habakkuk update --rollback` puts the previous binary back and restores
// the databases and config from the backup taken in step 3, after backing
// up the current state once more so a rollback loses nothing either.
//
// Databases are only migrated forward, by the new binary when it first
// opens them, each step in a transaction; an older binary refuses a database
// a newer one has migrated (store.SchemaTooNewError), which is why rollback
// restores the databases along with the binary.

// releaseKey is the base64 Ed25519 public key release SHA256SUMS files are
// signed with, set at build time (-X main.releaseKey=...). Empty: releases
// are checked against SHA256SUMS only.
var releaseKey = ""

func updateCmd(ctx context.Context, home string, args []string) error {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	check := fs.Bool("check", false, "only report whether an update is available")
	force := fs.Bool("force", false, "install the channel's release even if it is not newer (or roll back while serve runs)")
	rollback := fs.Bool("rollback", false, "restore the previous binary, databases and config")
	channel := fs.String("channel", "", "release channel: stable or beta (remembered for later updates)")
	noCheck := fs.Bool("skip-upgrade-check", false, "do not run the new binary's doctor --upgrade-check first")
	fs.Parse(args)
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	key, err := parsePublicKey(envOr("HABAKKUK_UPDATE_PUBKEY", releaseKey))
	if err != nil {
		return err
	}
	u := &updater{
		API:  envOr("HABAKKUK_UPDATE_API", "https://api.github.com/repos/habakkuk-ai/habakkuk"),
		HTTP: &http.Client{Timeout: 5 * time.Minute},
		OS:   runtime.GOOS, Arch: runtime.GOARCH, Exe: exe, Current: version, Out: os.Stdout,
		Home: home, PublicKey: key, Now: time.Now,
		Backup:  fullBackup,
		Restore: func(archive, home string) error { _, err := restoreBackup(archive, home, true); return err },
		Check:   runUpgradeCheck,
		Running: func(home string) (int, bool) { return serveRunning(home) },
	}
	if *noCheck {
		u.Check = nil
	}
	if *rollback {
		return u.rollback(ctx, *force)
	}
	return u.update(ctx, updateOptions{checkOnly: *check, force: *force, channel: *channel})
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

type updater struct {
	API       string // repository API base, e.g. https://api.github.com/repos/OWNER/REPO
	HTTP      *http.Client
	OS, Arch  string
	Exe       string // the binary to replace
	Current   string // the running version
	Home      string // $HABAKKUK_HOME: backups and the previous binary live in upgrades/
	Out       io.Writer
	PublicKey ed25519.PublicKey // set: releases must carry a valid SHA256SUMS.sig
	Now       func() time.Time

	// Backup writes a full backup of home to archive.
	Backup func(ctx context.Context, home, archive string) error
	// Restore unpacks archive over home.
	Restore func(archive, home string) error
	// Check runs the staged binary's upgrade check against home; nil skips it.
	Check func(ctx context.Context, exe, home string) error
	// Running reports a habakkuk serve using home.
	Running func(home string) (pid int, ok bool)
}

type updateOptions struct {
	checkOnly, force bool
	channel          string
}

// upgradeState is upgrades/state.json: the channel and what a rollback
// would restore.
type upgradeState struct {
	Channel  string        `json:"channel,omitempty"`
	Previous *previousInfo `json:"previous,omitempty"`
}

type previousInfo struct {
	Version    string    `json:"version"`     // the version a rollback returns to
	Binary     string    `json:"binary"`      // a copy of that binary
	Backup     string    `json:"backup"`      // backup of home taken before the update ("" when there was none)
	Exe        string    `json:"exe"`         // where the binary was installed
	ReplacedBy string    `json:"replaced_by"` // the version installed over it
	At         time.Time `json:"at"`
}

func (u *updater) dir() string       { return filepath.Join(u.Home, "upgrades") }
func (u *updater) statePath() string { return filepath.Join(u.dir(), "state.json") }

func (u *updater) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

func (u *updater) loadState() (*upgradeState, error) {
	data, err := os.ReadFile(u.statePath())
	if errors.Is(err, os.ErrNotExist) {
		return &upgradeState{}, nil
	}
	if err != nil {
		return nil, err
	}
	var st upgradeState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("%s: %w", u.statePath(), err)
	}
	return &st, nil
}

func (u *updater) saveState(st *upgradeState) error {
	if err := os.MkdirAll(u.dir(), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return store.WriteFileAtomic(u.statePath(), append(data, '\n'), 0o600)
}

// channel picks the release channel: the flag (remembered), then
// $HABAKKUK_UPDATE_CHANNEL, then the remembered one, then stable.
func (u *updater) channel(st *upgradeState, flagValue string) (string, error) {
	ch := flagValue
	if ch == "" {
		ch = os.Getenv("HABAKKUK_UPDATE_CHANNEL")
	}
	if ch == "" {
		ch = st.Channel
	}
	if ch == "" {
		ch = channelStable
	}
	if ch != channelStable && ch != channelBeta {
		return "", fmt.Errorf("unknown channel %q: use stable or beta", ch)
	}
	return ch, nil
}

func (u *updater) update(ctx context.Context, o updateOptions) error {
	st, err := u.loadState()
	if err != nil {
		return err
	}
	ch, err := u.channel(st, o.channel)
	if err != nil {
		return err
	}
	if o.channel != "" && o.channel != st.Channel && !o.checkOnly {
		st.Channel = o.channel
		if err := u.saveState(st); err != nil {
			return err
		}
	}
	rel, err := u.latest(ctx, ch)
	if errors.Is(err, errNoRelease) {
		// Not a failure when only asked to look: there is nothing newer.
		if o.checkOnly {
			fmt.Fprintf(u.Out, "habakkuk %s: no release on the %s channel yet, so there is nothing to update to\n", u.Current, ch)
			return nil
		}
		return fmt.Errorf("%w on the %s channel, so there is nothing to update to", err, ch)
	}
	if err != nil {
		return err
	}
	newer := compareVersions(rel.Tag, u.Current) > 0
	if !newer && !o.force {
		fmt.Fprintf(u.Out, "habakkuk %s is up to date (%s channel: %s)\n", u.Current, ch, rel.Tag)
		return nil
	}
	if o.checkOnly {
		fmt.Fprintf(u.Out, "update available on the %s channel: %s -> %s (run `habakkuk update`)\n", ch, u.Current, rel.Tag)
		return nil
	}
	bin, err := u.download(ctx, rel)
	if err != nil {
		return err
	}
	if err := checkWritable(filepath.Dir(u.Exe)); err != nil {
		return fmt.Errorf("cannot replace %s (%w): run the update as the user that installed it, or reinstall into a folder you own", u.Exe, err)
	}

	// Stage the new binary and let it look at this home before anything
	// changes.
	if err := os.MkdirAll(u.dir(), 0o700); err != nil {
		return err
	}
	staged := filepath.Join(u.dir(), "habakkuk-"+safeName(rel.Tag)+plat.ExeSuffix)
	if err := writeAtomic(staged, bytes.NewReader(bin), 0o755); err != nil {
		return fmt.Errorf("stage the new binary: %w", err)
	}
	defer os.Remove(staged)
	if u.Check != nil {
		fmt.Fprintf(u.Out, "checking %s against %s ...\n", rel.Tag, u.Home)
		if err := u.Check(ctx, staged, u.Home); err != nil {
			return fmt.Errorf("%s failed its upgrade check, so nothing was changed:\n%w", rel.Tag, err)
		}
	}

	// Back up the data and keep the binary being replaced.
	stamp := u.now().UTC().Format("20060102-150405")
	prev := &previousInfo{Version: u.Current, Exe: u.Exe, ReplacedBy: rel.Tag, At: u.now().UTC()}
	if _, err := os.Stat(filepath.Join(u.Home, "config.yaml")); err == nil {
		prev.Backup = filepath.Join(u.dir(), "backup-"+safeName(u.Current)+"-"+stamp+".tar.gz")
		if err := u.Backup(ctx, u.Home, prev.Backup); err != nil {
			return fmt.Errorf("back up before updating (nothing was changed): %w", err)
		}
		fmt.Fprintf(u.Out, "backed up config and databases to %s\n", prev.Backup)
	}
	prev.Binary = filepath.Join(u.dir(), "previous", "habakkuk"+plat.ExeSuffix)
	if err := copyFile(u.Exe, prev.Binary, 0o755); err != nil {
		return fmt.Errorf("keep the current binary (nothing was changed): %w", err)
	}
	st.Previous = prev
	if err := u.saveState(st); err != nil {
		return err
	}

	if err := installBinary(u.Exe, bin); err != nil {
		return fmt.Errorf("install: %w", err)
	}
	u.prune()
	fmt.Fprintf(u.Out, "updated %s from %s to %s.\nRestart running Habakkuk processes (habakkuk serve) to use it; undo with `habakkuk update --rollback`.\n",
		u.Exe, u.Current, rel.Tag)
	return nil
}

// rollback restores the binary, databases and config saved by the last
// update.
func (u *updater) rollback(ctx context.Context, force bool) error {
	st, err := u.loadState()
	if err != nil {
		return err
	}
	p := st.Previous
	if p == nil {
		return errors.New("nothing to roll back to: no update has been installed from this home")
	}
	if _, err := os.Stat(p.Binary); err != nil {
		return fmt.Errorf("the previous binary is gone (%w); reinstall %s from the releases page", err, p.Version)
	}
	if u.Running != nil {
		if pid, ok := u.Running(u.Home); ok && !force {
			return fmt.Errorf("habakkuk serve is running (pid %d): stop it first so its databases can be restored, or pass -force", pid)
		}
	}
	stamp := u.now().UTC().Format("20060102-150405")
	if p.Backup != "" {
		if _, err := os.Stat(p.Backup); err != nil {
			return fmt.Errorf("the backup taken before the update is gone (%w); restore the binary by hand from %s", err, p.Binary)
		}
		safety := filepath.Join(u.dir(), "before-rollback-"+stamp+".tar.gz")
		if err := u.Backup(ctx, u.Home, safety); err != nil {
			return fmt.Errorf("back up the current state before rolling back (nothing was changed): %w", err)
		}
		fmt.Fprintf(u.Out, "backed up the current state to %s\n", safety)
		moved, err := setAsideUnlisted(p.Backup, u.Home, filepath.Join(u.dir(), "rollback-"+stamp))
		if err != nil {
			return err
		}
		for _, m := range moved {
			fmt.Fprintf(u.Out, "moved aside %s (created after the update)\n", m)
		}
		if err := u.Restore(p.Backup, u.Home); err != nil {
			return fmt.Errorf("restore %s: %w (the state before the rollback is in %s)", p.Backup, err, safety)
		}
		fmt.Fprintf(u.Out, "restored config and databases from %s\n", p.Backup)
	}
	data, err := os.ReadFile(p.Binary)
	if err != nil {
		return err
	}
	exe := p.Exe
	if exe == "" {
		exe = u.Exe
	}
	if err := installBinary(exe, data); err != nil {
		return fmt.Errorf("put back the previous binary: %w", err)
	}
	st.Previous = nil
	if err := u.saveState(st); err != nil {
		return err
	}
	fmt.Fprintf(u.Out, "rolled back %s from %s to %s; restart running Habakkuk processes\n", exe, p.ReplacedBy, p.Version)
	return nil
}

// prune keeps the three newest backups of each kind in upgrades/.
func (u *updater) prune() {
	for _, pattern := range []string{"backup-*.tar.gz", "before-rollback-*.tar.gz"} {
		matches, _ := filepath.Glob(filepath.Join(u.dir(), pattern))
		sort.Slice(matches, func(i, j int) bool { return modTime(matches[i]).Before(modTime(matches[j])) })
		for len(matches) > 3 {
			_ = os.Remove(matches[0])
			matches = matches[1:]
		}
	}
}

func modTime(p string) time.Time {
	if st, err := os.Stat(p); err == nil {
		return st.ModTime()
	}
	return time.Time{}
}

// safeName keeps a version usable in a file name.
func safeName(v string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			return r
		}
		return '_'
	}, v)
}

// fullBackup is the backup an update takes: config, secrets, every
// database (memory, usage and nodes included, since the new binary may
// migrate them) and the agents' files, without workspaces.
func fullBackup(ctx context.Context, home, archive string) error {
	_, err := writeBackupOpts(ctx, home, archive, backupOptions{env: true, allDBs: true})
	return err
}

// runUpgradeCheck runs exe doctor --upgrade-check with HABAKKUK_HOME set
// to home.
func runUpgradeCheck(ctx context.Context, exe, home string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "doctor", "--upgrade-check")
	cmd.Env = append(os.Environ(), "HABAKKUK_HOME="+home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 4000 {
			msg = "..." + msg[len(msg)-4000:]
		}
		return fmt.Errorf("%w\n%s\n(pass -skip-upgrade-check to install anyway)", err, msg)
	}
	return nil
}

// serveRunning reports a live habakkuk serve on home from its heartbeat.
func serveRunning(home string) (int, bool) {
	hb, err := readHeartbeat(home)
	if err != nil || hb.PID == os.Getpid() {
		return 0, false
	}
	return hb.PID, plat.Alive(hb.PID)
}

// setAsideUnlisted moves databases that exist in home but not in the backup
// archive (created by the newer version) into dir, so the older binary does
// not find a schema it cannot read.
func setAsideUnlisted(archive, home, dir string) ([]string, error) {
	names, err := backupNames(archive)
	if err != nil {
		return nil, err
	}
	var moved []string
	candidates := backupDBNames()
	creds, _ := filepath.Glob(filepath.Join(home, "channels", "*.db"))
	for _, c := range creds {
		candidates = append(candidates, filepath.ToSlash(filepath.Join("channels", filepath.Base(c))))
	}
	for _, rel := range candidates {
		if names[rel] {
			continue
		}
		src := filepath.Join(home, filepath.FromSlash(rel))
		if _, err := os.Stat(src); err != nil {
			continue
		}
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return moved, err
		}
		for _, side := range []string{"", "-wal", "-shm"} {
			if err := os.Rename(src+side, dst+side); err != nil && !errors.Is(err, os.ErrNotExist) {
				return moved, err
			}
		}
		moved = append(moved, rel)
	}
	return moved, nil
}

// configMigrateCmd writes config.yaml in the current format.
func configCmd(home string, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "migrate" {
		return errors.New("usage: habakkuk config migrate [-dry-run]")
	}
	fs := flag.NewFlagSet("config migrate", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "show what would change without writing")
	fs.Parse(args[1:])
	res, err := config.MigrateFile(home, time.Now(), *dry)
	if err != nil {
		return err
	}
	if res.From == res.To {
		fmt.Fprintf(out, "config.yaml is already version %d; nothing to do\n", res.To)
	} else {
		for _, n := range res.Notes {
			fmt.Fprintln(out, n)
		}
		if *dry {
			fmt.Fprintf(out, "would migrate config.yaml from version %d to %d (dry run, nothing written)\n", res.From, res.To)
		} else {
			fmt.Fprintf(out, "migrated config.yaml from version %d to %d; the old file is %s\n", res.From, res.To, res.Backup)
		}
	}
	for _, k := range res.Unknown {
		fmt.Fprintf(out, "warning: unknown key %s is ignored (left in the file)\n", k)
	}
	return nil
}
