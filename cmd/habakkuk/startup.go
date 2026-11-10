package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
)

// startupCap is how long serve reports itself as starting at most: a
// channel that never reports back must not leave it "starting" forever.
// The channel list in hk status shows such a channel's own state.
const startupCap = 2 * time.Minute

// serveProgress keeps serve.json for hk status from the first moment
// of hk serve, saying what a starting process still waits for: the
// config and databases, the workers, then each channel's first connection.
type serveProgress struct {
	path string

	mu        sync.Mutex
	hb        heartbeat
	phase     string          // what startup is doing now; "" once the channels are starting
	pending   map[string]bool // channels still on their first connection attempt
	connected bool            // a channel has connected
	ready     bool
	log       *slog.Logger // nil until the app is open
	now       func() time.Time
}

func newServeProgress(home string, now func() time.Time) *serveProgress {
	p := &serveProgress{path: filepath.Join(home, heartbeatFile), now: now, pending: map[string]bool{},
		phase: "loading config and databases",
		hb:    heartbeat{PID: os.Getpid(), Version: version, Started: now()}}
	p.mu.Lock()
	p.write()
	p.mu.Unlock()
	return p
}

// update changes the state under the lock and rewrites serve.json.
func (p *serveProgress) update(f func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f()
	p.write()
}

func (p *serveProgress) setLog(l *slog.Logger) { p.update(func() { p.log = l }) }
func (p *serveProgress) setListen(addr string) { p.update(func() { p.hb.Listen = addr }) }
func (p *serveProgress) setPhase(s string)     { p.update(func() { p.phase = s }) }

// channelsStarting says which channels startup now waits for, and ends the
// startup phases before them.
func (p *serveProgress) channelsStarting(names []string) {
	p.update(func() {
		for _, n := range names {
			p.pending[n] = true
		}
		p.phase = ""
	})
}

// channelState is the gateway's OnChannelState. A channel's first attempt
// ends when it connects or fails; the first connection is logged with how
// long startup took.
func (p *serveProgress) channelState(account, state string) {
	if state == "connecting" {
		return
	}
	p.update(func() {
		delete(p.pending, account)
		if state == "connected" && !p.connected {
			p.connected = true
			if p.log != nil {
				p.log.Info("first channel connected", "channel", account, "after", p.since())
			}
		}
	})
}

func (p *serveProgress) since() time.Duration {
	return p.now().Sub(p.hb.Started).Round(time.Millisecond)
}

// waiting says what startup still waits for, or "" once ready. The lock
// is held.
func (p *serveProgress) waiting() string {
	if p.ready || p.now().Sub(p.hb.Started) > startupCap {
		return ""
	}
	if p.phase != "" {
		return p.phase
	}
	if len(p.pending) > 0 {
		return "channels to connect: " + strings.Join(slices.Sorted(maps.Keys(p.pending)), ", ")
	}
	return ""
}

// write rewrites serve.json. The lock is held.
func (p *serveProgress) write() {
	p.hb.Beat = p.now()
	p.hb.Starting = p.waiting()
	if p.hb.Starting == "" && !p.ready {
		p.ready = true
		if p.log != nil {
			p.log.Info("serve ready", "after", p.since())
		}
	}
	data, _ := json.Marshal(p.hb)
	if err := store.WriteFileAtomic(p.path, data, 0o600); err != nil && p.log != nil {
		p.log.Warn("write heartbeat failed", "err", err)
	}
}

// run keeps serve.json fresh until the returned func runs, which removes it.
func (p *serveProgress) run(ctx context.Context) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(heartbeatEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				p.update(func() {})
			}
		}
	}()
	return func() {
		cancel()
		<-done
		os.Remove(p.path)
	}
}
