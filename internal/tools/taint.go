package tools

import (
	"strings"
	"sync/atomic"
)

// Taint records that external content entered a turn: output of a tool that
// reaches the network (web pages, search results, MCP servers, the
// browser), hosted web search, or an inbound message from a channel whose
// senders are not vetted (email). Tools that persist what the model writes,
// such as memory_save, mark what they store as untrusted when it is set, so
// text planted in a page cannot quietly become a trusted memory.
//
// A Taint lives for one run of a turn; a nil *Taint is never tainted. A
// turn that is interrupted and resumed (a restart, a crash) gets a new one,
// set again from the turn's own transcript (Registry.MayTaint and the
// agent package's restoreTaint), so a restart never clears it.
type Taint struct{ set atomic.Bool }

// NewTaint returns a Taint, already set when tainted is true.
func NewTaint(tainted bool) *Taint {
	t := &Taint{}
	t.set.Store(tainted)
	return t
}

// Mark sets the taint.
func (t *Taint) Mark() {
	if t != nil {
		t.set.Store(true)
	}
}

// Marked reports whether external content has entered the turn.
func (t *Taint) Marked() bool { return t != nil && t.set.Load() }

// ExternalContent is implemented by tools whose output comes from outside
// even though their risk is below network (none of the built-ins today).
// Tools of network risk taint the turn without it.
type ExternalContent interface {
	ExternalContent()
}

// taints reports whether a tool's output counts as external content.
func taints(t Tool) bool {
	if _, ok := t.(ExternalContent); ok {
		return true
	}
	return t.Risk() == RiskNetwork
}

// MayTaint reports whether a call to the named tool can bring outside
// content into a turn. A resumed turn uses it to set its taint again from
// the calls its earlier run recorded, so it errs on the side of yes: a tool
// that is not registered now (an MCP server still connecting after the
// restart) counts, and so does every computer tool, whose screenshots
// taint the turn themselves.
func (r *Registry) MayTaint(name string) bool {
	r.mu.RLock()
	t, ok := r.tools[name]
	r.mu.RUnlock()
	if !ok || taints(t) {
		return true
	}
	for _, ts := range r.toolsetsOf(name) {
		if ts == "computer" || strings.HasPrefix(ts, "computer-") {
			return true
		}
	}
	return false
}
