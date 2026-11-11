package config

import (
	"fmt"
	"net/url"
	"regexp"
	"time"
)

// Skills configures which skills agents are offered, the skills hub
// (registries to search and install from) and the curator that tidies
// agent-written skills.
type Skills struct {
	// Bundled says whether the skills shipped inside the program are
	// offered (default true). False turns all of them off at once.
	Bundled *bool `yaml:"bundled"`
	// Disabled are skill names turned off for every agent, wherever the
	// skill comes from (bundled, shared, a plugin or an agent's own).
	// Nothing is removed. `hk skills disable NAME` does the same without
	// editing this file.
	Disabled []string `yaml:"disabled"`
	// DefaultRegistry is the index URL of the registry named "default",
	// searched after Registries. Empty means DefaultRegistryURL (the
	// public directory); "off" turns it off.
	DefaultRegistry string `yaml:"default_registry"`
	// Registries are your own indexes, searched in order before the
	// default, so a name both list comes from yours. Plugins are looked up
	// in the same indexes.
	Registries []*SkillRegistry `yaml:"registries"`
	// Curate is how often the curator reviews agent-written skills:
	// off (default), daily, weekly or monthly.
	Curate string `yaml:"curate"`
	// ArchiveAfterDays archives an agent-written skill nobody has used for
	// this many days (default 90).
	ArchiveAfterDays int `yaml:"archive_after_days"`
}

// SkillRegistry is one index of skills and plugins (see docs/tools/plugins.md
// for the format).
type SkillRegistry struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
}

// DefaultRegistryURL is the registry Habakkuk ships with: the index of the
// public skills directory at habakkuk.ai/skills. It is contacted only by
// `hk skills` and `hk plugins` search, install and update.
// skills.default_registry: "off" in config.yaml removes it, another URL
// there replaces it, and a build may change it with
// -ldflags "-X github.com/habakkuk-ai/habakkuk/internal/config.DefaultRegistryURL=https://...".
var DefaultRegistryURL = "https://habakkuk.ai/skills/index.json"

// BundledSkills reports whether the skills shipped inside the program are
// offered to agents.
func (c *Config) BundledSkills() bool {
	return c.Skills == nil || c.Skills.Bundled == nil || *c.Skills.Bundled
}

// DisabledSkills are the skill names turned off in the config file.
func (c *Config) DisabledSkills() []string {
	if c.Skills == nil {
		return nil
	}
	return c.Skills.Disabled
}

var (
	reRegistryName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	reSkillName    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
)

// SkillRegistries is the configured registries followed by the default one
// (when there is one). The order is the order a bare name is looked up in:
// a registry the owner added wins over the public directory, so a package
// there can never be shadowed by a public one of the same name.
func (c *Config) SkillRegistries() []SkillRegistry {
	var out []SkillRegistry
	if c.Skills != nil {
		for _, r := range c.Skills.Registries {
			if r != nil {
				out = append(out, *r)
			}
		}
	}
	def := DefaultRegistryURL
	if c.Skills != nil && c.Skills.DefaultRegistry != "" {
		def = c.Skills.DefaultRegistry
	}
	if def != "" && def != "off" {
		out = append(out, SkillRegistry{Name: "default", URL: def})
	}
	return out
}

// CurateEvery is how often the skill curator runs; 0 is never.
func (c *Config) CurateEvery() time.Duration {
	if c.Skills == nil {
		return 0
	}
	switch c.Skills.Curate {
	case "daily":
		return 24 * time.Hour
	case "weekly":
		return 7 * 24 * time.Hour
	case "monthly":
		return 30 * 24 * time.Hour
	}
	return 0
}

// ArchiveAfter is how long an agent-written skill may go unused before the
// curator archives it.
func (c *Config) ArchiveAfter() time.Duration {
	days := 90
	if c.Skills != nil && c.Skills.ArchiveAfterDays > 0 {
		days = c.Skills.ArchiveAfterDays
	}
	return time.Duration(days) * 24 * time.Hour
}

func (c *Config) validateSkills() []error {
	s := c.Skills
	if s == nil {
		return nil
	}
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf("skills: "+format, args...)) }
	switch s.Curate {
	case "", "off", "daily", "weekly", "monthly":
	default:
		bad("curate %q must be off, daily, weekly or monthly", s.Curate)
	}
	if s.ArchiveAfterDays < 0 {
		bad("archive_after_days must not be negative")
	}
	for _, name := range s.Disabled {
		if !reSkillName.MatchString(name) {
			bad("disabled: %q is not a skill name (lowercase letters, digits and hyphens)", name)
		}
	}
	if s.DefaultRegistry != "" && s.DefaultRegistry != "off" && !registryURL(s.DefaultRegistry) {
		bad("default_registry %q must be an https URL or off", s.DefaultRegistry)
	}
	seen := map[string]bool{"default": s.DefaultRegistry != "off" && (s.DefaultRegistry != "" || DefaultRegistryURL != "")}
	for i, r := range s.Registries {
		switch {
		case r == nil:
			bad("registries[%d] is empty", i)
		case !reRegistryName.MatchString(r.Name):
			bad("registries[%d]: name %q must be lowercase letters, digits, '.', '_' or '-'", i, r.Name)
		case seen[r.Name]:
			bad("registries[%d]: name %q is used twice", i, r.Name)
		case !registryURL(r.URL):
			bad("registry %s: url %q must be https", r.Name, r.URL)
		default:
			seen[r.Name] = true
		}
	}
	return errs
}

// registryURL accepts https URLs, and http on loopback (a local index).
func registryURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return false
	}
	h := u.Hostname()
	return u.Scheme == "https" || (u.Scheme == "http" && (h == "localhost" || h == "127.0.0.1" || h == "::1"))
}
