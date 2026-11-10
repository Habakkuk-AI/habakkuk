package channel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"
)

// Supervisor keeps one adapter running. It restarts the adapter when Run
// returns, backing off exponentially, and it restarts an adapter that is
// still "running" but has stopped beating: a socket that hangs half-open
// gets torn down instead of silently dropping messages for days.
type Supervisor struct {
	Store        *Store
	Log          *slog.Logger
	MinBackoff   time.Duration // default 1s
	MaxBackoff   time.Duration // default 5m
	HealthyAfter time.Duration // a run this long resets the backoff; default 1m
	Check        time.Duration // watchdog interval; default 5s
	Grace        time.Duration // how long a canceled adapter gets to return; default 30s
	// OnState, when set, hears every state change (connecting, connected,
	// backoff, stopped) of every account.
	OnState func(account, state string)
}

func (s *Supervisor) defaults() {
	if s.MinBackoff == 0 {
		s.MinBackoff = time.Second
	}
	if s.MaxBackoff == 0 {
		s.MaxBackoff = 5 * time.Minute
	}
	if s.HealthyAfter == 0 {
		s.HealthyAfter = time.Minute
	}
	if s.Check == 0 {
		s.Check = 5 * time.Second
	}
	if s.Grace == 0 {
		s.Grace = 30 * time.Second
	}
	if s.Log == nil {
		s.Log = slog.Default()
	}
}

// Run supervises the adapter until ctx ends.
func (s *Supervisor) Run(ctx context.Context, account string, a Adapter, deliver func(context.Context, Message)) {
	s.defaults()
	log := s.Log.With("channel", account)
	backoff := s.MinBackoff
	st := Status{Account: account}
	for restarts := 0; ; restarts++ {
		st.Restarts = restarts
		in := &inbox{deliver: deliver, account: account}
		in.lastBeat.Store(time.Now().UnixNano())
		s.setStatus(ctx, &st, "connecting", "", "")

		runCtx, cancel := context.WithCancel(ctx)
		in.onConnect = func(detail string) {
			in.connectedAt.Store(time.Now().UnixNano())
			s.setStatus(ctx, &st, "connected", detail, "")
			log.Info("channel connected", "detail", detail)
		}
		done := make(chan error, 1)
		go func() { done <- safeRun(runCtx, a, in) }()

		err, stale := s.watch(ctx, a, in, &st, done, cancel)
		in.closed.Store(true) // an abandoned adapter can no longer deliver
		cancel()
		if ctx.Err() != nil {
			s.setStatus(context.WithoutCancel(ctx), &st, "stopped", "", "")
			return
		}
		if stale {
			err = fmt.Errorf("no heartbeat for %s; restarted", a.StaleAfter())
		}
		if err == nil {
			err = errors.New("connection closed")
		}

		connected := in.connectedAt.Load()
		switch {
		case errors.As(err, new(Fatal)):
			backoff = s.MaxBackoff
		case connected != 0 && time.Since(time.Unix(0, connected)) >= s.HealthyAfter:
			backoff = s.MinBackoff
		}
		log.Warn("channel down; reconnecting", "err", err, "in", backoff)
		s.setStatus(ctx, &st, "backoff", "", err.Error())
		select {
		case <-ctx.Done():
			s.setStatus(context.WithoutCancel(ctx), &st, "stopped", "", "")
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, s.MaxBackoff)
	}
}

// watch waits for the adapter to return, canceling it if its heartbeat goes
// stale. It records beats to the status table at most every 30s.
func (s *Supervisor) watch(ctx context.Context, a Adapter, in *inbox, st *Status, done <-chan error, cancel func()) (error, bool) {
	t := time.NewTicker(s.Check)
	defer t.Stop()
	var saved int64
	for {
		select {
		case err := <-done:
			return err, false
		case <-ctx.Done():
			cancel()
			s.await(done)
			return nil, false
		case <-t.C:
			beat := in.lastBeat.Load()
			if time.Since(time.Unix(0, beat)) > a.StaleAfter() {
				s.Log.Warn("channel stopped beating; restarting it", "channel", st.Account)
				cancel()
				s.await(done)
				return nil, true
			}
			if beat-saved > int64(30*time.Second) && in.connectedAt.Load() != 0 {
				saved = beat
				st.LastBeat = time.Unix(0, beat)
				if s.Store != nil {
					_ = s.Store.SetStatus(ctx, *st)
				}
			}
		}
	}
}

func (s *Supervisor) await(done <-chan error) {
	select {
	case err := <-done:
		if err != nil {
			s.Log.Debug("channel adapter stopped", "err", err)
		}
	case <-time.After(s.Grace):
		s.Log.Error("channel adapter ignored cancellation; abandoning it")
	}
}

func (s *Supervisor) setStatus(ctx context.Context, st *Status, state, detail, lastErr string) {
	st.State = state
	if detail != "" || state != "connected" {
		st.Detail = detail
	}
	if lastErr != "" {
		st.LastError = lastErr
	}
	st.Since = time.Now()
	if s.Store != nil {
		if err := s.Store.SetStatus(ctx, *st); err != nil && ctx.Err() == nil {
			s.Log.Warn("record channel status failed", "err", err)
		}
	}
	if s.OnState != nil {
		s.OnState(st.Account, state)
	}
}

func safeRun(ctx context.Context, a Adapter, in Inbox) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("adapter panic: %v", r)
		}
	}()
	return a.Run(ctx, in)
}

type inbox struct {
	account     string
	deliver     func(context.Context, Message)
	onConnect   func(string)
	lastBeat    atomic.Int64
	connectedAt atomic.Int64
	closed      atomic.Bool
}

func (i *inbox) Deliver(ctx context.Context, m Message) {
	if i.closed.Load() {
		return
	}
	i.Beat()
	m.Account = i.account
	i.deliver(ctx, m)
}

func (i *inbox) Beat() { i.lastBeat.Store(time.Now().UnixNano()) }

func (i *inbox) Connected(detail string) {
	i.Beat()
	if !i.closed.Load() && i.onConnect != nil {
		i.onConnect(detail)
	}
}
