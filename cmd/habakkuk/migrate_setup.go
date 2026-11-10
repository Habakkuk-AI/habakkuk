package main

import (
	"bufio"
	"fmt"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/migrate"
)

// offerImport runs on a new home before the wizard's steps: when another
// agent's home is on this computer, one question offers to import it first
// (with -y, only when -migrate is given). The wizard then goes on editing
// the config the import wrote.
func (s *setup) offerImport() error {
	o := migrate.Options{Getenv: s.getenv, UserHome: s.userHome}
	found := migrate.Detect(o)
	if len(found) == 0 || (!s.interactive && !s.opts.migrate) {
		return nil
	}
	from := found[0]
	label, dir := migrate.Label(from), migrate.DefaultSource(from, o)
	if s.interactive {
		ok, err := s.yes(fmt.Sprintf("Found a %s setup in %s. Import its agents, memory, skills, schedules and keys first?", label, dir), true)
		if err != nil || !ok {
			if err == nil {
				s.say("  Skipped; hk migrate imports it later.")
			}
			return err
		}
	}
	m := &migration{home: s.home, in: bufio.NewReader(strings.NewReader("")), out: s.out, getenv: s.getenv, inSetup: true,
		fl: migrateFlags{from: from, agent: s.opts.agent, secrets: "copy", yes: true, userHome: s.userHome}}
	if err := m.run(s.ctx); err != nil {
		return fmt.Errorf("importing from %s: %w", label, err)
	}
	if len(found) > 1 {
		s.say("\nA %s setup is here too: hk migrate --from %s imports it.", migrate.Label(found[1]), found[1])
	}
	s.say("")
	return s.load()
}
