package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// Capability names a kind of call that can hurt the machine Habakkuk runs
// on: the shell, background processes, file writes outside the workspace,
// the computer (screen, mouse, keyboard) and the browser on non-public
// addresses. A turn's Policy decides who may use them.
type Capability string

const (
	CapExec     Capability = "exec"     // exec and tools of exec risk (MCP servers marked exec)
	CapProcess  Capability = "process"  // input to and killing background processes
	CapWrite    Capability = "write"    // file writes outside the workspace
	CapComputer Capability = "computer" // the computer toolsets
	CapBrowser  Capability = "browser"  // the browser on loopback or private addresses
)

// Capabilities lists every Capability, in the order reports show them.
var Capabilities = []Capability{CapExec, CapProcess, CapWrite, CapComputer, CapBrowser}

// ValidCapability reports whether s names a Capability.
func ValidCapability(s string) bool {
	for _, c := range Capabilities {
		if string(c) == s {
			return true
		}
	}
	return false
}

// Capable is implemented by tools that know which Capability one of their
// calls uses (the browser checks the address it is sent to, for example).
// It returns "" when the call uses none.
type Capable interface {
	Capability(ctx context.Context, env *Env, input json.RawMessage) Capability
}

// Policy is what a turn may do with the capabilities, decided by where the
// turn came from. It is checked before the permission mode, so full mode
// does not lift it.
//
// The zero Policy (and a nil one) is a turn started on this machine (the
// CLI, the TUI, the dashboard): the permission mode decides, except that a
// tainted turn needs a human for the shell and for writes outside the
// workspace.
type Policy struct {
	// Channel is the messaging account the turn came from; "" for a turn
	// started on this machine. Capabilities not in Allowed need approval.
	Channel string
	// Guest is set when the message came from someone who is not the
	// owner (let in by allow_from "*", not by id or pairing). A guest's
	// turn gets none of the capabilities, whoever would approve.
	Guest bool
	// Allowed are the capabilities the owner let channel turns use without
	// approval (agents.<name>.trust: full, or channel_allow).
	Allowed map[Capability]bool
	// AllowTaintedExec lets a tainted turn use the shell and write outside
	// the workspace as the mode says (agents.<name>.allow_tainted_exec).
	AllowTaintedExec bool
	// TrustedWrite are folders (absolute) outside the workspace where the
	// file tools may write notes without approval, in a channel turn and in
	// a tainted one (agents.<name>.trusted_write_paths; trusted.go).
	// Protected are folders that are never trusted, whatever TrustedWrite
	// says: the agent homes, config, hooks, skills and the folders
	// scheduled scripts run from.
	TrustedWrite []string
	Protected    []string
	// Place is where the turn stands for approval rules (allow.go):
	// PlaceLocal for this machine (the CLI, the TUI, the dashboard), a
	// ChatPlace for the owner's chat, and "" for anything else (a guest, an
	// account the config no longer has, an inbound A2A task, a webhook),
	// where no rule answers. A nil Policy is PlaceLocal. Which turns may use
	// rules at all is agent.limitRules's to say.
	Place string
}

// PlaceLocal is the Place of a turn started on this machine.
const PlaceLocal = "local"

// ChatPlace is the Place of a turn from the owner in one chat.
func ChatPlace(account, conversation string) string {
	return "chat:" + account + ":" + conversation
}

// capability is the Capability a call uses, or "".
func (r *Registry) capability(ctx context.Context, env *Env, t Tool, call llm.Block) Capability {
	if c, ok := t.(Capable); ok {
		if cp := c.Capability(ctx, env, call.Input); cp != "" {
			return cp
		}
	}
	// The computer's actions are exec risk, but they are their own
	// capability: screenshots taint the turn, and the computer has its own
	// guard (computer.confirm_apps), so taint does not stop every click.
	for _, ts := range r.toolsetsOf(call.ToolName) {
		if ts == "computer" || strings.HasPrefix(ts, "computer-") {
			return CapComputer
		}
	}
	switch {
	case strings.HasPrefix(call.ToolName, "process_") && t.Risk() >= RiskWrite:
		return CapProcess
	case t.Risk() == RiskExec:
		return CapExec
	case t.Risk() == RiskWrite:
		if _, ok := t.(OwnStateTool); ok {
			return ""
		}
		if pt, ok := t.(PathTool); ok {
			if p, err := pt.TargetPath(call.Input); err == nil && !env.InWorkspace(env.Resolve(p)) {
				return CapWrite
			}
		}
		if pt, ok := t.(PathsTool); ok {
			if ps, err := pt.TargetPaths(call.Input); err == nil && !allIn(env, ps) {
				return CapWrite
			}
		}
	}
	return ""
}

// toolsetsOf lists the toolsets a tool is registered in.
func (r *Registry) toolsetsOf(name string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for ts, names := range r.toolsets {
		for _, n := range names {
			if n == name {
				out = append(out, ts)
				break
			}
		}
	}
	return out
}

// gate applies the turn's Policy to a call that uses cp. handled is false
// when the policy has nothing to say and the permission mode decides; when
// it is true, err (nil or not) is the decision and note goes on the result.
func gate(ctx context.Context, env *Env, t Tool, cp Capability, risk Risk, call llm.Block, confirm string) (note string, handled bool, err error) {
	p := env.Policy
	if p == nil {
		p = &Policy{}
	}
	if p.Guest {
		return "", true, fmt.Errorf("%w: %s (%s) is not available to messages from people other than the owner; tell the user the owner has to do this", ErrDenied, call.ToolName, cp)
	}
	if env.Mode == ModeReadOnly && risk != RiskRead {
		return "", false, nil // the mode denies it anyway
	}
	risky := cp == CapExec || cp == CapProcess || cp == CapWrite
	if risky && env.Taint.Marked() && !p.AllowTaintedExec {
		why := fmt.Sprintf("this turn has read outside content (a web page, search result, email or tool output), so %s needs your approval", cp)
		if confirm != "" {
			why = confirm + "; " + why
		}
		return "", true, confirmByHuman(ctx, env, call, why, CauseTainted)
	}
	if p.Channel != "" && !p.Allowed[cp] {
		if confirm != "" {
			return "", true, confirmByHuman(ctx, env, call, confirm, CauseConfirm)
		}
		// The owner said "Always allow" for calls like this one (allow.go).
		if rule, ok := coveredByRule(env, t, call); ok {
			return rule.Note(), true, nil
		}
		note, err := Ask(ctx, env, ApprovalRequest{Tool: call.ToolName, Cause: CauseChannel,
			Summary: fmt.Sprintf("[%s via channel %s] %s", cp, p.Channel, summarize(call.Input)), Input: call.Input,
			Rule: ProposeRule(env, t, call)})
		return note, true, err
	}
	return "", false, nil
}
