package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"gopkg.in/yaml.v3"
)

// Config files carry a schema version (the top-level version: key). When a
// release changes the format it bumps CurrentVersion and appends a step to
// configMigrations. Loading an older file migrates it in memory, so an
// upgrade never stops Habakkuk from starting; `habakkuk config migrate`
// writes the migrated file after keeping a timestamped copy of the old one.
//
// Keys this binary does not know are reported and ignored rather than
// refused: a config edited for a newer release (or kept across a rollback)
// still loads. `habakkuk doctor` lists them so typos do not go unnoticed.

// configStep upgrades a config document from one version to the next. It
// edits the YAML node tree in place (keeping comments) and returns notes
// for the user, one per change.
type configStep struct {
	from int
	note string
	fn   func(root *yaml.Node) ([]string, error)
}

// configMigrations holds one step per version, from 0 up to CurrentVersion.
var configMigrations = []configStep{
	{from: 0, note: "add the version key (configs written before it existed)", fn: func(*yaml.Node) ([]string, error) {
		return nil, nil // setVersion below records the new version
	}},
}

// LoadReport says what loading a config had to do: the version it was
// written in, the migration steps applied in memory, and unknown keys that
// were ignored.
type LoadReport struct {
	FileVersion int      // the version: key in the file (0 when missing)
	Migrated    []string // migration notes; empty when the file is current
	Unknown     []string // unknown keys, as `"key" in Section (line N)`
}

// NeedsMigration reports whether the file on disk is older than this binary.
func (r *LoadReport) NeedsMigration() bool { return r != nil && len(r.Migrated) > 0 }

// Summary is a one-line description for logs, or "".
func (r *LoadReport) Summary() string {
	if r == nil || len(r.Migrated) == 0 {
		return ""
	}
	return fmt.Sprintf("config.yaml is version %d; migrated to version %d in memory (%s). Run `habakkuk config migrate` to save it.",
		r.FileVersion, CurrentVersion, strings.Join(r.Migrated, "; "))
}

// ErrConfigTooNew is returned for a config written by a newer Habakkuk.
var ErrConfigTooNew = errors.New("config written by a newer Habakkuk")

// decodeConfig parses data into cfg, migrating an older format in memory.
// Unknown keys fail unless lenient, which reports and ignores them.
func decodeConfig(data []byte, cfg *Config, lenient bool) (*LoadReport, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	rep := &LoadReport{}
	if root := docRoot(&doc); root != nil {
		v, err := nodeVersion(root)
		if err != nil {
			return nil, err
		}
		rep.FileVersion = v
		if v > CurrentVersion {
			return nil, fmt.Errorf("%w: it is version %d, and this habakkuk understands up to version %d. "+
				"Update habakkuk (habakkuk update), or restore the config saved before the upgrade "+
				"(habakkuk update --rollback, or config.yaml.<time>.bak)", ErrConfigTooNew, v, CurrentVersion)
		}
		if v < CurrentVersion {
			notes, err := migrateNode(root, v)
			if err != nil {
				return nil, err
			}
			rep.Migrated = notes
			if data, err = yaml.Marshal(&doc); err != nil {
				return nil, err
			}
		}
	}
	err := strictDecode(data, cfg)
	var te *yaml.TypeError
	if err == nil || !lenient || !errors.As(err, &te) {
		if err != nil {
			return nil, err
		}
		return rep, nil
	}

	var other []string
	for _, msg := range te.Errors {
		// Line numbers point into the migrated text, so they are only
		// shown for a file that needed no migration.
		if u, ok := unknownKey(msg, len(rep.Migrated) == 0); ok {
			rep.Unknown = append(rep.Unknown, u)
		} else {
			other = append(other, msg)
		}
	}
	if len(other) > 0 {
		return nil, &yaml.TypeError{Errors: other}
	}
	// Decode again without KnownFields: the strict pass stops filling in a
	// struct at its first unknown key.
	*cfg = Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	return rep, nil
}

func strictDecode(data []byte, cfg *Config) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	return dec.Decode(cfg)
}

var reUnknownField = regexp.MustCompile(`^(line \d+): field (\S+) not found in type \S+\.(\S+)$`)

// unknownKey turns yaml.v3's "line 4: field foo not found in type
// config.HTTP" into `"foo" in HTTP (line 4)`.
func unknownKey(msg string, withLine bool) (string, bool) {
	m := reUnknownField.FindStringSubmatch(msg)
	if m == nil {
		return "", false
	}
	s := strconv.Quote(m[2])
	if m[3] != "Config" {
		s += " in " + m[3]
	}
	if withLine {
		s += " (" + m[1] + ")"
	}
	return s, true
}

func docRoot(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 && doc.Content[0].Kind == yaml.MappingNode {
		return doc.Content[0]
	}
	return nil
}

// nodeVersion reads the top-level version key; 0 when missing.
func nodeVersion(root *yaml.Node) (int, error) {
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "version" {
			v, err := strconv.Atoi(root.Content[i+1].Value)
			if err != nil {
				return 0, fmt.Errorf("line %d: version must be a whole number, not %q", root.Content[i+1].Line, root.Content[i+1].Value)
			}
			return v, nil
		}
	}
	return 0, nil
}

// FileVersion reads the version key of a config file without loading it
// (0 when missing).
func FileVersion(data []byte) (int, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return 0, err
	}
	root := docRoot(&doc)
	if root == nil {
		return 0, errors.New("not a YAML mapping")
	}
	return nodeVersion(root)
}

// setVersion sets (or adds, as the first key) version: v.
func setVersion(root *yaml.Node, v int) {
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "version" {
			root.Content[i+1].Value = strconv.Itoa(v)
			root.Content[i+1].Tag = "!!int"
			return
		}
	}
	key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "version"}
	val := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(v)}
	root.Content = append([]*yaml.Node{key, val}, root.Content...)
}

// migrateNode applies every step from version from up to CurrentVersion.
func migrateNode(root *yaml.Node, from int) ([]string, error) {
	return applySteps(root, from, CurrentVersion, configMigrations)
}

func applySteps(root *yaml.Node, from, to int, steps []configStep) ([]string, error) {
	var notes []string
	for v := from; v < to; v++ {
		var step *configStep
		for i := range steps {
			if steps[i].from == v {
				step = &steps[i]
			}
		}
		if step == nil {
			return nil, fmt.Errorf("no migration from config version %d", v)
		}
		more, err := step.fn(root)
		if err != nil {
			return nil, fmt.Errorf("migrating config from version %d: %w", v, err)
		}
		notes = append(notes, fmt.Sprintf("v%d->v%d: %s", v, v+1, step.note))
		for _, n := range more {
			notes = append(notes, "  "+n)
		}
		setVersion(root, v+1)
	}
	return notes, nil
}

// MigrateResult describes what MigrateFile did.
type MigrateResult struct {
	From, To int
	Notes    []string
	Backup   string   // the copy of the old file, "" when nothing changed
	Unknown  []string // keys this binary ignores (kept in the file)
}

// MigrateFile rewrites home/config.yaml in the current format. The old file
// is first copied to config.yaml.<UTC time>.bak; the new one must load
// before it replaces the old. Comments are kept; the YAML may be
// re-indented. With dryRun nothing is written.
func MigrateFile(home string, now time.Time, dryRun bool) (*MigrateResult, error) {
	path := filepath.Join(home, "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	root := docRoot(&doc)
	if root == nil {
		return nil, fmt.Errorf("%s is not a YAML mapping", path)
	}
	from, err := nodeVersion(root)
	if err != nil {
		return nil, err
	}
	res := &MigrateResult{From: from, To: CurrentVersion}
	switch {
	case from > CurrentVersion:
		return nil, fmt.Errorf("%w: %s is version %d; this habakkuk understands up to %d", ErrConfigTooNew, path, from, CurrentVersion)
	case from == CurrentVersion:
		return res, nil
	}
	if res.Notes, err = migrateNode(root, from); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	cfg, err := parseHome(out, home, true)
	if err != nil {
		return nil, fmt.Errorf("the migrated config would not load, so %s was left alone: %w", path, err)
	}
	res.Unknown = cfg.Report.Unknown

	if dryRun {
		return res, nil
	}
	res.Backup = fmt.Sprintf("%s.%s.bak", path, now.UTC().Format("20060102-150405"))
	if err := store.WriteFileAtomic(res.Backup, data, 0o600); err != nil {
		return nil, err
	}
	if err := store.WriteFileAtomic(path, out, 0o600); err != nil {
		return nil, err
	}
	return res, nil
}
