// Package hub installs packages (skills and plugins) from outside sources:
// a GitHub repository or a folder in one, a URL to a single file or an
// archive, a local path, or a name in a registry index. Every install is
// fetched into a staging folder inside the destination, scanned for
// dangerous scripts, hashed, and only then renamed into place. A lock file
// records where each package came from, the ref asked for, what it
// resolved to and the hash of the installed files, so an update fetches
// the same source again and a replaced version is kept as a backup.
package hub

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// Kind is where a package comes from.
type Kind string

const (
	KindGitHub   Kind = "github"   // a repository, or a folder in one, at a ref
	KindURL      Kind = "url"      // a single file, or a .zip / .tar.gz archive
	KindLocal    Kind = "local"    // a folder or file on this machine
	KindRegistry Kind = "registry" // a name looked up in the configured indexes
)

// Source is a parsed package source.
type Source struct {
	Kind Kind
	// GitHub
	Owner, Repo, Path, Ref string
	// URL
	URL string
	// Local: an absolute path to a folder or file
	Dir string
	// Registry: the package name, and the registry to look in ("" for all)
	Name, Registry string
}

var (
	reGitHubPart = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
	reRegName    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	reSHA        = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// ParseSource understands:
//
//	github:owner/repo[/path][@ref]
//	https://github.com/owner/repo[/tree/REF[/path]]
//	https://example.com/SKILL.md, .../pkg.zip, .../pkg.tar.gz
//	./dir, /abs/dir, ~/dir, file:PATH (a folder, file or archive)
//	registry:NAME, registry:REGISTRY/NAME, or a bare NAME
func ParseSource(s string) (Source, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return Source{}, errors.New("empty source")
	case strings.HasPrefix(s, "github:"):
		return parseGitHub(strings.TrimPrefix(s, "github:"))
	case strings.HasPrefix(s, "https://github.com/"):
		return parseGitHubURL(strings.TrimPrefix(s, "https://github.com/"))
	case strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://"):
		u, err := url.Parse(s)
		if err != nil || u.Host == "" {
			return Source{}, fmt.Errorf("bad URL %q", s)
		}
		if u.Scheme == "http" && !loopbackHost(u.Hostname()) {
			return Source{}, fmt.Errorf("%s: only https URLs are allowed (http only for localhost)", s)
		}
		return Source{Kind: KindURL, URL: s}, nil
	case strings.HasPrefix(s, "registry:"):
		rest := strings.TrimPrefix(s, "registry:")
		reg, name, ok := strings.Cut(rest, "/")
		if !ok {
			reg, name = "", rest
		}
		if !reRegName.MatchString(name) || (reg != "" && !reRegName.MatchString(reg)) {
			return Source{}, fmt.Errorf("bad registry source %q: want registry:NAME or registry:REGISTRY/NAME", s)
		}
		return Source{Kind: KindRegistry, Name: name, Registry: reg}, nil
	case strings.HasPrefix(s, "file:"):
		return localSource(strings.TrimPrefix(s, "file:"))
	case strings.HasPrefix(s, "/"), strings.HasPrefix(s, "./"), strings.HasPrefix(s, "../"), strings.HasPrefix(s, "~/"), s == ".":
		return localSource(s)
	}
	if _, err := os.Stat(s); err == nil {
		return localSource(s)
	}
	if reRegName.MatchString(s) {
		return Source{Kind: KindRegistry, Name: s}, nil
	}
	return Source{}, fmt.Errorf("unknown source %q: use github:owner/repo[/path][@ref], an https URL, a local path or a registry name", s)
}

func parseGitHub(s string) (Source, error) {
	spec, ref, _ := strings.Cut(s, "@")
	parts := strings.Split(strings.Trim(spec, "/"), "/")
	if len(parts) < 2 || !reGitHubPart.MatchString(parts[0]) || !reGitHubPart.MatchString(parts[1]) {
		return Source{}, fmt.Errorf("bad GitHub source %q: want github:owner/repo[/path][@ref]", "github:"+s)
	}
	src := Source{Kind: KindGitHub, Owner: parts[0], Repo: strings.TrimSuffix(parts[1], ".git"), Ref: ref}
	if len(parts) > 2 {
		p, err := cleanRel(strings.Join(parts[2:], "/"))
		if err != nil {
			return Source{}, err
		}
		src.Path = p
	}
	if strings.ContainsAny(ref, " \t\n~^:?*[\\") || strings.Contains(ref, "..") {
		return Source{}, fmt.Errorf("bad ref %q", ref)
	}
	return src, nil
}

// parseGitHubURL maps a browser URL (owner/repo, or owner/repo/tree/REF/path)
// to a GitHub source. A ref with a slash in it needs the github: form.
func parseGitHubURL(s string) (Source, error) {
	s, _, _ = strings.Cut(s, "#")
	s, _, _ = strings.Cut(s, "?")
	parts := strings.Split(strings.Trim(s, "/"), "/")
	if len(parts) < 2 {
		return Source{}, fmt.Errorf("bad GitHub URL %q", s)
	}
	spec := parts[0] + "/" + parts[1]
	ref := ""
	if len(parts) >= 4 && (parts[2] == "tree" || parts[2] == "blob") {
		ref = parts[3]
		if len(parts) > 4 {
			rest := strings.Join(parts[4:], "/")
			if parts[2] == "blob" { // a file: install its folder
				rest = path.Dir(rest)
			}
			if rest != "." {
				spec += "/" + rest
			}
		}
	} else if len(parts) > 2 {
		return Source{}, fmt.Errorf("bad GitHub URL %q: want owner/repo or owner/repo/tree/REF/path", s)
	}
	if ref != "" {
		spec += "@" + ref
	}
	return parseGitHub(spec)
}

func localSource(p string) (Source, error) {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return Source{}, err
		}
		p = filepath.Join(home, rest)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return Source{}, err
	}
	if _, err := os.Stat(abs); err != nil {
		return Source{}, err
	}
	return Source{Kind: KindLocal, Dir: abs}, nil
}

// cleanRel checks a slash path inside a package or repository.
func cleanRel(p string) (string, error) {
	c := path.Clean("/" + p)[1:]
	if c != strings.Trim(p, "/") || strings.Contains(p, "\\") {
		return "", fmt.Errorf("bad path %q", p)
	}
	return c, nil
}

// String renders the source in the form ParseSource reads back.
func (s Source) String() string {
	switch s.Kind {
	case KindGitHub:
		out := "github:" + s.Owner + "/" + s.Repo
		if s.Path != "" {
			out += "/" + s.Path
		}
		if s.Ref != "" {
			out += "@" + s.Ref
		}
		return out
	case KindURL:
		return s.URL
	case KindLocal:
		return s.Dir
	case KindRegistry:
		if s.Registry != "" {
			return "registry:" + s.Registry + "/" + s.Name
		}
		return "registry:" + s.Name
	}
	return ""
}

// Sub is the source of a folder inside this one (rel is a slash path), so
// that one package of a multi-package source can be fetched again on its
// own. Archives cannot address a folder, so URL sources stay as they are and
// the folder is recorded beside them.
func (s Source) Sub(rel string) Source {
	if rel == "" || rel == "." {
		return s
	}
	switch s.Kind {
	case KindGitHub:
		s.Path = path.Join(s.Path, rel)
	case KindLocal:
		s.Dir = filepath.Join(s.Dir, filepath.FromSlash(rel))
	}
	return s
}

// NameHint is a package name to fall back on when the package names none.
func (s Source) NameHint() string {
	var base string
	switch s.Kind {
	case KindGitHub:
		base = s.Repo
		if s.Path != "" {
			base = path.Base(s.Path)
		}
	case KindURL:
		u, err := url.Parse(s.URL)
		if err == nil {
			p := strings.TrimSuffix(u.Path, "/")
			base = path.Base(p)
			for _, ext := range []string{".tar.gz", ".tgz", ".zip", ".md", ".yaml", ".yml"} {
				base = strings.TrimSuffix(base, ext)
			}
			if strings.EqualFold(base, "SKILL") || strings.EqualFold(base, "habakkuk-plugin") {
				base = path.Base(path.Dir(p))
			}
		}
	case KindLocal:
		base = filepath.Base(s.Dir)
		if !isDir(s.Dir) {
			base = filepath.Base(filepath.Dir(s.Dir))
		}
	case KindRegistry:
		base = s.Name
	}
	return Slug(base)
}

// Slug lowercases a name and keeps letters, digits and hyphens.
func Slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if len(out) > 64 {
		out = strings.TrimRight(out[:64], "-")
	}
	return out
}

func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func loopbackHost(h string) bool {
	return h == "localhost" || h == "127.0.0.1" || h == "::1" || strings.HasSuffix(h, ".localhost")
}
