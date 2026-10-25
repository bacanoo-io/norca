// Package ratelimit is a small, dependency-free per-key token-bucket rate
// limiter. uBixVault uses it to throttle API clients (keyed by source IP) so a
// single client cannot brute-force unseal shares or tokens.
//
// Each key gets a bucket that refills at a fixed rate up to a burst ceiling;
// each allowed request costs one token. Buckets are created lazily and can be
// swept once idle so memory stays bounded under many distinct keys.
package ratelimit

import (
	"math"
	"sync"
	"time"
)

// Limiter is a set of per-key token buckets. The zero value is not usable; call
// [New]. It is safe for concurrent use.
type Limiter struct {
	rate  float64 // tokens added per second
	burst float64 // maximum tokens a bucket holds

	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time // injectable clock for tests
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New returns a limiter allowing rate requests/second per key with the given
// burst (the most a single key can spend at once). Both must be positive.
func New(rate, burst float64) *Limiter {
	return &Limiter{
		rate:    rate,
		burst:   burst,
		buckets: make(map[string]*bucket),
		now:     time.Now,
	}
}

// Rate returns the configured refill rate in tokens per second.
func (l *Limiter) Rate() float64 { return l.rate }

// Burst returns the configured burst ceiling (the most a single key may spend
// at once).
func (l *Limiter) Burst() float64 { return l.burst }

// Allow reports whether a request for key may proceed, consuming one token when
// it can.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b := l.buckets[key]
	if b == nil {
		// A new key starts with a full burst so first-seen clients aren't penalized.
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	} else {
		elapsed := now.Sub(b.last).Seconds()
		if elapsed > 0 {
			b.tokens = math.Min(l.burst, b.tokens+elapsed*l.rate)
			b.last = now
		}
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// Sweep removes buckets idle for at least idle that have refilled to a full
// burst — removing such a bucket is equivalent to a fresh one, so no throttling
// state is lost. This bounds memory under a large or hostile key space. It
// returns the number removed.
func (l *Limiter) Sweep(idle time.Duration) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	cutoff := now.Add(-idle)
	removed := 0
	for k, b := range l.buckets {
		if !b.last.Before(cutoff) {
			continue // used recently
		}
		// Effective tokens after refill; if full, this bucket carries no denial
		// state and can be dropped safely.
		eff := math.Min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
		if eff >= l.burst {
			delete(l.buckets, k)
			removed++
		}
	}
	return removed
}
