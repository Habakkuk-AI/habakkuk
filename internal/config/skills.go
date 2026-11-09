package config

import (
	"fmt"
	"net/url"
	"regexp"
	"time"
)

// Skills configures the skills hub (registries to search and install
// from) and the curator that tidies agent-written skills.
type Skills struct {
	// DefaultRegistry is the index URL searched first, named "default".
	// Empty means DefaultRegistryURL; "off" turns it off.
	DefaultRegistry string `yaml:"default_registry"`
	// Registries are more indexes, searched in order after the default.
	// Plugins are looked up in the same indexes.
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

// DefaultRegistryURL is the registry Habakkuk ships with. It is empty; a
// build may set one with
// -ldflags "-X github.com/habakkuk-ai/habakkuk/internal/config.DefaultRegistryURL=https://...".
var DefaultRegistryURL = ""

var reRegistryName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// SkillRegistries is the default registry (when there is one) followed by
// the configured ones.
func (c *Config) SkillRegistries() []SkillRegistry {
	var out []SkillRegistry
	def := DefaultRegistryURL
	if c.Skills != nil && c.Skills.DefaultRegistry != "" {
		def = c.Skills.DefaultRegistry
	}
	if def != "" && def != "off" {
		out = append(out, SkillRegistry{Name: "default", URL: def})
	}
	if c.Skills != nil {
		for _, r := range c.Skills.Registries {
			if r != nil {
				out = append(out, *r)
			}
		}
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
