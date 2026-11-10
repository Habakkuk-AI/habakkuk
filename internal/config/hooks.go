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
	// ProgressPlain (the default) is one short line in everyday words
	// ("Checking the web"), edited as the work moves on and replaced by the
	// reply. It never shows tool names, commands, paths or ids.
	ProgressPlain = "plain"
	// ProgressDetailed is one "working" message with a line per tool step.
	ProgressDetailed = "detailed"
	// ProgressDraft is the old name of ProgressDetailed, still accepted so
	// configs written before plain existed keep their behaviour.
	ProgressDraft = "draft"
	ProgressOff   = "off" // typing indicator or one late notice only
)

// ProgressModes are the values progress takes, as documented.
var ProgressModes = []string{ProgressPlain, ProgressDetailed, ProgressOff}

// NormalizeProgress maps a progress setting to plain, detailed or off: ""
// is the default (plain) and draft is detailed. ok is false for anything
// else.
func NormalizeProgress(v string) (mode string, ok bool) {
	switch v {
	case "", ProgressPlain:
		return ProgressPlain, true
	case ProgressDetailed, ProgressDraft:
		return ProgressDetailed, true
	case ProgressOff:
		return ProgressOff, true
	}
	return "", false
}

// ProgressMode is the channel's progress setting with its default: plain,
// detailed or off.
func (ch *Channel) ProgressMode() string {
	if ch == nil {
		return ProgressPlain
	}
	if mode, ok := NormalizeProgress(ch.Progress); ok {
		return mode
	}
	return ProgressPlain
}

// How much an approval prompt in a chat shows of the call it asks about.
const (
	// ApprovalsSummary (the default) says what the agent wants to do in
	// everyday words; the exact call is one "details" reply away.
	ApprovalsSummary = "summary"
	// ApprovalsFull also appends the exact call in a code block.
	ApprovalsFull = "full"
)

// ApprovalsDetails are the values approvals_detail takes.
var ApprovalsDetails = []string{ApprovalsSummary, ApprovalsFull}

// NormalizeApprovalsDetail maps an approvals_detail setting to summary or
// full; "" is the default (summary). ok is false for anything else.
func NormalizeApprovalsDetail(v string) (mode string, ok bool) {
	switch v {
	case "", ApprovalsSummary:
		return ApprovalsSummary, true
	case ApprovalsFull:
		return ApprovalsFull, true
	}
	return "", false
}

// ApprovalsDetailMode is the channel's approvals_detail setting with its
// default: summary or full.
func (ch *Channel) ApprovalsDetailMode() string {
	if ch == nil {
		return ApprovalsSummary
	}
	if mode, ok := NormalizeApprovalsDetail(ch.ApprovalsDetail); ok {
		return mode
	}
	return ApprovalsSummary
}

// validateExtras checks hooks and the per-channel progress and
// approvals_detail settings.
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
		if _, ok := NormalizeProgress(ch.Progress); !ok {
			errs = append(errs, fmt.Errorf("channel %s: progress %q must be plain, detailed or off", name, ch.Progress))
		}
		if _, ok := NormalizeApprovalsDetail(ch.ApprovalsDetail); !ok {
			errs = append(errs, fmt.Errorf("channel %s: approvals_detail %q must be summary or full", name, ch.ApprovalsDetail))
		}
	}
	return errs
}
