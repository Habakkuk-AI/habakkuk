package jobs

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Handler runs one job. Returning nil completes it; returning an error fails
// it (retried if the error is not Permanent and attempts remain). The context
// is canceled if the lease is lost, so handlers must stop writing promptly.
type Handler func(ctx context.Context, j *Job, owner string) (result string, err error)

// Permanent wraps an error that retrying cannot fix. An error that has a
// Permanent() bool method returning true (such as a refused spending limit)
// is treated the same way, wherever it is wrapped.
type Permanent struct{ Err error }

func (p Permanent) Error() string { return p.Err.Error() }
func (p Permanent) Unwrap() error { return p.Err }

type Pool struct {
	Queue       *Queue
	Handlers    map[string]Handler
	Concurrency int
	Lease       time.Duration
	Poll        time.Duration
	Log         *slog.Logger

	wake chan struct{}
	once sync.Once
}

// Wake tells idle workers to poll now instead of waiting for the next tick.
func (p *Pool) Wake() {
	p.init()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *Pool) init() {
	p.once.Do(func() {
		p.wake = make(chan struct{}, 1)
		if p.Concurrency <= 0 {
			p.Concurrency = 4
		}
		if p.Lease <= 0 {
			p.Lease = 45 * time.Second
		}
		if p.Poll <= 0 {
			p.Poll = time.Second
		}
		if p.Log == nil {
			p.Log = slog.Default()
		}
	})
}

// Run starts the workers and blocks until ctx is canceled and all in-flight
// jobs have stopped.
func (p *Pool) Run(ctx context.Context) {
	p.init()
	if n, err := p.Queue.ReleaseDead(ctx); err != nil {
		p.Log.Warn("release dead leases failed", "err", err)
	} else if n > 0 {
		p.Log.Info("resuming jobs from a crashed process", "count", n)
	}
	kinds := make([]string, 0, len(p.Handlers))
	for k := range p.Handlers {
		kinds = append(kinds, k)
	}
	var wg sync.WaitGroup
	for i := 0; i < p.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			owner := OwnerID()
			for ctx.Err() == nil {
				j, err := p.Queue.Claim(ctx, owner, p.Lease, kinds...)
				if err != nil && ctx.Err() == nil {
					p.Log.Error("claim failed", "err", err)
				}
				if j == nil {
					select {
					case <-ctx.Done():
					case <-p.wake:
					case <-time.After(p.Poll):
					}
					continue
				}
				p.run(ctx, j, owner)
				p.Wake() // a finished job may unblock its serial key for another worker
			}
		}()
	}
	wg.Wait()
}

func (p *Pool) run(parent context.Context, j *Job, owner string) {
	log := p.Log.With("job", j.ID, "kind", j.Kind, "attempt", j.Attempts)
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		t := time.NewTicker(p.Lease / 3)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := p.Queue.Heartbeat(context.WithoutCancel(ctx), j.ID, owner, p.Lease); err != nil {
					log.Warn("heartbeat failed; stopping job", "err", err)
					cancel()
					return
				}
			}
		}
	}()

	result, err := p.safeRun(ctx, j, owner)
	cancel()
	<-hbDone

	// Shutting down: leave the lease to expire so another process resumes it.
	if parent.Err() != nil {
		log.Info("shutdown mid-job; will resume after lease expiry")
		return
	}
	bg := context.Background()
	if err == nil {
		if err := p.Queue.Complete(bg, j.ID, owner, result); err != nil {
			log.Warn("complete failed", "err", err)
		}
		return
	}
	if errors.Is(err, ErrLeaseLost) {
		log.Warn("lease lost")
		return
	}
	retry := !IsPermanent(err)
	backoff := time.Duration(1<<min(j.Attempts, 6)) * time.Second
	log.Error("job failed", "err", err, "retry", retry)
	if ferr := p.Queue.Fail(bg, j.ID, owner, err, retry, backoff); ferr != nil {
		log.Warn("fail record failed", "err", ferr)
	}
}

// IsPermanent reports whether retrying err cannot help.
func IsPermanent(err error) bool {
	if errors.As(err, new(Permanent)) {
		return true
	}
	var p interface{ Permanent() bool }
	return errors.As(err, &p) && p.Permanent()
}

func (p *Pool) safeRun(ctx context.Context, j *Job, owner string) (result string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = Permanent{Err: errors.New("handler panic")}
			p.Log.Error("handler panic", "job", j.ID, "panic", r)
		}
	}()
	h, ok := p.Handlers[j.Kind]
	if !ok {
		return "", Permanent{Err: errors.New("no handler for kind " + j.Kind)}
	}
	return h(ctx, j, owner)
}
