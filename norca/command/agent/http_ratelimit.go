// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	metrics "github.com/hashicorp/go-metrics"
	"golang.org/x/time/rate"

	"github.com/bacanoo-io/norca/norca/norca/structs/config"
)

// The request rate limits per ACL token of the HTTP API of a server (Norca,
// design raft-observers-and-hot-standby.md, phase 4). Off by default.

// anonymousBucket is the key of the requests without a known ACL token:
// anonymous requests, unknown or expired tokens and workload identities
// share it, so random tokens do not get a limit each.
const anonymousBucket = ""

// tokenRateLimitIdle is how long the buckets of a token that sends no
// request are kept.
const tokenRateLimitIdle = 10 * time.Minute

// tokenRateLimiter keeps a read and a write token bucket per ACL token.
type tokenRateLimiter struct {
	readRate, writeRate   rate.Limit
	readBurst, writeBurst int

	// resolve returns the accessor ID of the ACL token with the secret, or
	// anonymousBucket.
	resolve func(secret string) string

	now func() time.Time

	mu        sync.Mutex
	buckets   map[string]*tokenBuckets
	lastSweep time.Time
}

type tokenBuckets struct {
	read, write *rate.Limiter
	seen        time.Time
}

// newTokenRateLimiter returns the limiter of the limits, nil when both limits
// are off.
func newTokenRateLimiter(l config.Limits, resolve func(string) string) (*tokenRateLimiter, error) {
	get := func(name string, v *int) (int, error) {
		if v == nil {
			return 0, nil
		}
		if *v < 0 {
			return 0, fmt.Errorf("%s must be >= 0", name)
		}
		return *v, nil
	}
	readRate, err := get("token_read_requests_per_second", l.TokenReadRequestsPerSecond)
	if err != nil {
		return nil, err
	}
	readBurst, err := get("token_read_burst", l.TokenReadBurst)
	if err != nil {
		return nil, err
	}
	writeRate, err := get("token_write_requests_per_second", l.TokenWriteRequestsPerSecond)
	if err != nil {
		return nil, err
	}
	writeBurst, err := get("token_write_burst", l.TokenWriteBurst)
	if err != nil {
		return nil, err
	}
	if readRate == 0 && writeRate == 0 {
		return nil, nil
	}
	limit := func(perSecond int) rate.Limit {
		if perSecond == 0 {
			return rate.Inf
		}
		return rate.Limit(perSecond)
	}
	if readBurst == 0 {
		readBurst = max(readRate, 1)
	}
	if writeBurst == 0 {
		writeBurst = max(writeRate, 1)
	}
	return &tokenRateLimiter{
		readRate: limit(readRate), writeRate: limit(writeRate),
		readBurst: readBurst, writeBurst: writeBurst,
		resolve: resolve,
		now:     time.Now,
		buckets: make(map[string]*tokenBuckets),
	}, nil
}

// isReadRequest reports whether a request counts against the read limit.
func isReadRequest(req *http.Request) bool {
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// allow takes a request of the token from its bucket. It returns false and
// the time to wait when the bucket is empty.
func (l *tokenRateLimiter) allow(secret string, read bool) (bool, time.Duration) {
	key := anonymousBucket
	if secret != "" && l.resolve != nil {
		key = l.resolve(secret)
	}
	now := l.now()

	l.mu.Lock()
	if now.Sub(l.lastSweep) > time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.seen) > tokenRateLimitIdle {
				delete(l.buckets, k)
			}
		}
		l.lastSweep = now
	}
	b := l.buckets[key]
	if b == nil {
		b = &tokenBuckets{
			read:  rate.NewLimiter(l.readRate, l.readBurst),
			write: rate.NewLimiter(l.writeRate, l.writeBurst),
		}
		l.buckets[key] = b
	}
	b.seen = now
	l.mu.Unlock()

	lim := b.write
	if read {
		lim = b.read
	}
	r := lim.ReserveN(now, 1)
	if !r.OK() {
		return false, time.Second
	}
	if d := r.DelayFrom(now); d > 0 {
		r.CancelAt(now)
		return false, d
	}
	return true, 0
}

// rateLimited answers 429 with Retry-After when the token of the request is
// over its limit, and reports whether it did.
func (s *HTTPServer) rateLimited(resp http.ResponseWriter, req *http.Request) bool {
	if s.rateLimiter == nil {
		return false
	}
	var secret string
	s.parseToken(req, &secret)
	read := isReadRequest(req)
	ok, wait := s.rateLimiter.allow(secret, read)
	if ok {
		return false
	}
	kind := "write"
	if read {
		kind = "read"
	}
	metrics.IncrCounterWithLabels([]string{"nomad", "http", "rate_limited"}, 1,
		[]metrics.Label{{Name: "kind", Value: kind}})
	resp.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
	resp.WriteHeader(http.StatusTooManyRequests)
	fmt.Fprintf(resp, "rate limit of the %s requests of this token exceeded, retry after %s\n",
		kind, wait.Round(time.Millisecond))
	return true
}
