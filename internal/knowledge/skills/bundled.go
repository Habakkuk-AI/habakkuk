package skills

import (
	"embed"
	"errors"
	"io/fs"
	"path"
	"slices"
	"strings"
)

// The skills that ship inside the binary: bundled/<name>/SKILL.md, and any
// supporting files beside it. They are read from the binary and never
// written to disk, so an update replaces them and nothing can edit them.
// Every other place a skill can live wins over a bundled one of the same
// name, so saving your own copy (skill_save, or a folder under
// $HABAKKUK_HOME/skills) is how you change one.
//
//go:embed bundled
var bundledFS embed.FS

// Bundled is the folder of skills shipped with the program, for
// Library.Bundled.
func Bundled() fs.FS {
	sub, err := fs.Sub(bundledFS, "bundled")
	if err != nil {
		panic(err) // the folder is part of the build
	}
	return sub
}

// loadBundled reads one bundled skill.
func loadBundled(fsys fs.FS, name string) (Skill, string, error) {
	if fsys == nil {
		return Skill{}, "", fs.ErrNotExist
	}
	data, err := fs.ReadFile(fsys, path.Join(name, FileName))
	if err != nil {
		return Skill{}, "", err
	}
	s, body := parse(name, data)
	s.Shared, s.Bundled = true, true
	if s.fsys, err = fs.Sub(fsys, name); err != nil {
		return Skill{}, "", err
	}
	return s, body, nil
}

// scanBundled adds every bundled skill to into.
func scanBundled(fsys fs.FS, into map[string]Skill) error {
	if fsys == nil {
		return nil
	}
	entries, err := fs.ReadDir(fsys, ".")
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() || ValidName(e.Name()) != nil {
			continue
		}
		if s, _, err := loadBundled(fsys, e.Name()); err == nil {
			into[s.Name] = s
		}
	}
	return nil
}

// bundledFiles lists a bundled skill's supporting files, sorted.
func bundledFiles(fsys fs.FS) ([]string, error) {
	var files []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == "." {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || p == FileName {
			return nil
		}
		if len(files) == maxListedFiles {
			return fs.SkipAll
		}
		files = append(files, p)
		return nil
	})
	slices.Sort(files)
	return files, err
}
