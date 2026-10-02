// Package ratelimit provides the gateway's two abuse controls: a per-client
// token bucket for ordinary traffic, and an escalating lockout for repeated
// authentication failures.
//
// Both are keyed by an opaque string (the client address, or a device id) so the
// package stays independent of HTTP. State is in-memory by design: the gateway is
// a single process behind one tunnel, and losing counters on restart is an
// acceptable trade for having no shared store to operate.
package ratelimit

import (
	"math"
	"sync"
	"time"
)

// Config parameterises a Limiter.
type Config struct {
	// PerSecond is the sustained refill rate.
	PerSecond float64
	// Burst is the bucket depth: how many requests may arrive at once.
	Burst int
	// MaxFailures is how many authentication failures are tolerated before the
	// first lockout.
	MaxFailures int
	// LockoutBase is the first lockout duration. Each further failure doubles it.
	LockoutBase time.Duration
	// MaxLockout caps the escalation.
	MaxLockout time.Duration
	// IdleEviction is how long an untouched key is retained before its state is
	// dropped by Sweep.
	IdleEviction time.Duration
}

// Limiter is safe for concurrent use.
type Limiter struct {
	cfg Config
	now func() time.Time

	mu    sync.Mutex
	state map[string]*entry
}

type entry struct {
	tokens   float64
	refill   time.Time
	failures int
	locked   time.Time // zero when not locked out
	seen     time.Time
}

// New builds a Limiter. A nil now defaults to time.Now.
func New(cfg Config, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	if cfg.IdleEviction <= 0 {
		cfg.IdleEviction = time.Hour
	}
	return &Limiter{cfg: cfg, now: now, state: map[string]*entry{}}
}

// Allow consumes one token for key, reporting whether the request may proceed.
// It returns the time to wait when the bucket is empty, so the caller can emit a
// Retry-After header.
func (l *Limiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	e := l.entryLocked(key, now)

	if now.Before(e.locked) {
		return false, e.locked.Sub(now)
	}

	// Refill proportionally to elapsed time, capped at the burst depth.
	elapsed := now.Sub(e.refill).Seconds()
	e.refill = now
	e.tokens = min(float64(l.cfg.Burst), e.tokens+elapsed*l.cfg.PerSecond)

	if e.tokens < 1 {
		// Time until one whole token accumulates.
		//
		// Rounded *up*, plus a nanosecond of slack. Truncating told a client to
		// retry at an instant that never quite arrived: the next call found the
		// bucket still empty, recomputed a sub-nanosecond deficit, and reported a
		// zero wait — so a well-behaved client honouring Retry-After would spin
		// as fast as it could, forever, and never be served.
		seconds := (1 - e.tokens) / l.cfg.PerSecond
		wait := time.Duration(math.Ceil(seconds*float64(time.Second))) + time.Nanosecond
		if wait <= 0 {
			// Reachable only for an implausibly large rate; one nanosecond is
			// enough to make progress and cannot loop.
			wait = time.Nanosecond
		}
		return false, wait
	}
	e.tokens--
	return true, 0
}

// Fail records an authentication failure for key and applies an escalating
// lockout once the tolerance is exhausted.
func (l *Limiter) Fail(key string) {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	e := l.entryLocked(key, now)
	e.failures++

	if e.failures <= l.cfg.MaxFailures {
		return
	}
	// Double per failure beyond the tolerance: base, 2*base, 4*base, ...
	shift := e.failures - l.cfg.MaxFailures - 1
	lockout := l.cfg.MaxLockout
	if shift < 32 { // beyond 32 doublings the shift itself would be meaningless
		if scaled := l.cfg.LockoutBase * time.Duration(1<<uint(shift)); scaled > 0 && scaled < l.cfg.MaxLockout {
			lockout = scaled
		}
	}
	e.locked = now.Add(lockout)
}

// Succeed clears the failure history for key, so a legitimate operator who
// mistyped a code is not punished for the rest of the lockout window.
func (l *Limiter) Succeed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if e, ok := l.state[key]; ok {
		e.failures = 0
		e.locked = time.Time{}
	}
}

// LockedFor reports the remaining lockout, or zero when key is not locked out.
func (l *Limiter) LockedFor(key string) time.Duration {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	e, ok := l.state[key]
	if !ok || !now.Before(e.locked) {
		return 0
	}
	return e.locked.Sub(now)
}

// Sweep drops state for keys untouched for longer than IdleEviction. It is meant
// to be called periodically so that a long-running gateway does not accumulate
// an entry per source address ever seen.
func (l *Limiter) Sweep() {
	cutoff := l.now().Add(-l.cfg.IdleEviction)

	l.mu.Lock()
	defer l.mu.Unlock()

	for key, e := range l.state {
		// Never evict a key that is still locked out, or the lockout would be
		// trivially defeated by waiting for the sweep.
		if e.seen.Before(cutoff) && !l.now().Before(e.locked) {
			delete(l.state, key)
		}
	}
}

// entryLocked fetches or creates the entry for key. The caller must hold l.mu.
func (l *Limiter) entryLocked(key string, now time.Time) *entry {
	e, ok := l.state[key]
	if !ok {
		e = &entry{tokens: float64(l.cfg.Burst), refill: now}
		l.state[key] = e
	}
	e.seen = now
	return e
}
