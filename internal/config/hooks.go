package config

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Hook runs a command on an event. The command gets the event as JSON on
// stdin and HABAKKUK_EVENT in its environment. A tool_before hook that exits
// non-zero blocks the call, with its stdout as the reason.
type Hook struct {
	Event   string        `yaml:"event"`   // one of HookEvents
	Command string        `yaml:"command"` // run with /bin/sh -c
	Timeout time.Duration `yaml:"timeout"` // default 10s
	// Match is a regular expression the event's subject must match: the tool
	// name (tool_before, tool_after), the agent (turn_start, turn_end,
	// session_new), the channel account (message_in, message_out) or the
	// schedule name (schedule_run). Empty matches everything.
	Match string `yaml:"match"`
}

// HookEvents are the events a hook can name.
var HookEvents = []string{"turn_start", "turn_end", "tool_before", "tool_after",
	"message_in", "message_out", "session_new", "schedule_run"}

// Progress modes for a channel: how a long turn shows that it is working.
const (
	ProgressDraft = "draft" // one "working" message, edited with tool progress, replaced by the reply
	ProgressOff   = "off"   // typing indicator or one late notice only
)

// ProgressMode is the channel's progress setting with its default.
func (ch *Channel) ProgressMode() string {
	if ch == nil || ch.Progress == "" {
		return ProgressDraft
	}
	return ch.Progress
}

// validateExtras checks hooks and per-channel progress settings.
func (c *Config) validateExtras() []error {
	var errs []error
	for i, h := range c.Hooks {
		bad := func(format string, args ...any) {
			errs = append(errs, fmt.Errorf("hooks[%d]: "+format, append([]any{i}, args...)...))
		}
		if h == nil {
			bad("empty entry")
			continue
		}
		known := false
		for _, e := range HookEvents {
			known = known || e == h.Event
		}
		if !known {
			bad("event %q must be one of %s", h.Event, strings.Join(HookEvents, ", "))
		}
		if strings.TrimSpace(h.Command) == "" {
			bad("command is required")
		}
		if h.Timeout < 0 {
			bad("timeout must not be negative")
		}
		if _, err := regexp.Compile(h.Match); err != nil {
			bad("match: %v", err)
		}
	}
	for name, ch := range c.Channels {
		if ch == nil {
			continue
		}
		switch ch.Progress {
		case "", ProgressDraft, ProgressOff:
		default:
			errs = append(errs, fmt.Errorf("channel %s: progress %q must be draft or off", name, ch.Progress))
		}
	}
	return errs
}
