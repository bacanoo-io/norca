// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"math"
	"sync"
	"time"
)

// RateLimiter decides how long a key has to wait before it is processed
// again after a failure.
type RateLimiter[K comparable] interface {
	// When returns the delay before key may be retried, and records the
	// retry.
	When(key K) time.Duration

	// Forget clears the retry history of key, typically after a success.
	Forget(key K)

	// NumRequeues returns how many times key was retried since the last
	// Forget.
	NumRequeues(key K) int
}

// ExponentialBackoff is a per-key exponential backoff: Base * 2^retries,
// capped at Max.
type ExponentialBackoff[K comparable] struct {
	Base time.Duration
	Max  time.Duration

	mu       sync.Mutex
	failures map[K]int
}

// NewExponentialBackoff returns a per-key exponential backoff.
func NewExponentialBackoff[K comparable](base, max time.Duration) *ExponentialBackoff[K] {
	return &ExponentialBackoff[K]{Base: base, Max: max, failures: map[K]int{}}
}

func (r *ExponentialBackoff[K]) When(key K) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.failures[key]
	r.failures[key] = n + 1

	d := float64(r.Base) * math.Pow(2, float64(n))
	if d > float64(r.Max) || math.IsInf(d, 0) {
		return r.Max
	}
	return time.Duration(d)
}

func (r *ExponentialBackoff[K]) Forget(key K) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.failures, key)
}

func (r *ExponentialBackoff[K]) NumRequeues(key K) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failures[key]
}

// TokenBucket limits the overall retry rate of all keys to QPS with bursts
// of Burst.
type TokenBucket[K comparable] struct {
	qps   float64
	burst float64
	now   func() time.Time

	mu     sync.Mutex
	tokens float64
	last   time.Time
}

// NewTokenBucket returns an overall rate limiter.
func NewTokenBucket[K comparable](qps float64, burst int) *TokenBucket[K] {
	return &TokenBucket[K]{qps: qps, burst: float64(burst), tokens: float64(burst), now: time.Now}
}

// When reserves a token and returns how long to wait until it is
// available.
func (r *TokenBucket[K]) When(K) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if !r.last.IsZero() {
		r.tokens = math.Min(r.burst, r.tokens+now.Sub(r.last).Seconds()*r.qps)
	}
	r.last = now
	r.tokens--
	if r.tokens >= 0 || r.qps <= 0 {
		return 0
	}
	return time.Duration(-r.tokens / r.qps * float64(time.Second))
}

func (r *TokenBucket[K]) Forget(K)          {}
func (r *TokenBucket[K]) NumRequeues(K) int { return 0 }

// MaxOf combines rate limiters: the delay is the longest of them.
type MaxOf[K comparable] []RateLimiter[K]

func (m MaxOf[K]) When(key K) time.Duration {
	var d time.Duration
	for _, r := range m {
		d = max(d, r.When(key))
	}
	return d
}

func (m MaxOf[K]) Forget(key K) {
	for _, r := range m {
		r.Forget(key)
	}
}

func (m MaxOf[K]) NumRequeues(key K) int {
	var n int
	for _, r := range m {
		n = max(n, r.NumRequeues(key))
	}
	return n
}

// DefaultRateLimiter returns the default rate limiter of controllers: a
// per-key exponential backoff from 500ms to 5 minutes, and an overall limit
// of 10 retries per second with bursts of 100.
func DefaultRateLimiter[K comparable]() RateLimiter[K] {
	return MaxOf[K]{
		NewExponentialBackoff[K](500*time.Millisecond, 5*time.Minute),
		NewTokenBucket[K](10, 100),
	}
}
