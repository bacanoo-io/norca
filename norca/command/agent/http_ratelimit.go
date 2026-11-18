// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"fmt"
	"math"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"

	metrics "github.com/hashicorp/go-metrics"
	"golang.org/x/time/rate"

	"github.com/bacanoo-io/norca/norca/norca/structs/config"
)

// The request rate limits per ACL token of the HTTP API of a server (Norca,
// design raft-observers-and-hot-standby.md, phase 4). Off by default.

// anonymousBucket is what resolve returns for a request without a known ACL
// token: anonymous requests, unknown or expired tokens, workload identities,
// and every request while ACLs are off. Such a request is limited by its
// client address, so random tokens do not get a limit each, and one client
// can't use up the requests of the others.
const anonymousBucket = ""

// tokenRateLimitIdle is how long the buckets of a token or of a client
// address that sends no request are kept at least; they are dropped only once
// they are full again.
const tokenRateLimitIdle = 10 * time.Minute

// tokenRateLimitMaxBuckets is the most tokens, and the most client addresses,
// that have buckets of their own at a time. A new token or address beyond it
// is limited by the shared bucket until idle ones are dropped: the memory of
// the limiter is bounded, and no request goes unlimited.
const tokenRateLimitMaxBuckets = 16384

// tokenRateLimiter keeps a read and a write token bucket per ACL token, and
// per client address for the requests without a known token.
type tokenRateLimiter struct {
	readRate, writeRate   rate.Limit
	readBurst, writeBurst int

	// resolve returns the accessor ID of the ACL token with the secret, or
	// anonymousBucket.
	resolve func(secret string) string

	now func() time.Time

	// maxBuckets is the cap of each of the two maps.
	maxBuckets int

	mu sync.Mutex
	// buckets are those of the ACL tokens, by accessor ID.
	buckets map[string]*tokenBuckets
	// clients are those of the client addresses, by clientBucketKey.
	clients map[string]*tokenBuckets
	// shared limits the requests that have no bucket of their own: those
	// without a usable client address, and those of new tokens and
	// addresses while their map is full.
	shared    *tokenBuckets
	lastSweep time.Time
}

type tokenBuckets struct {
	read, write *rate.Limiter
	seen        time.Time
}

// fullAt reports whether both buckets are full at now, so that dropping them
// gives nothing back: the next request starts with new, full buckets. An idle
// bucket that refills more slowly than tokenRateLimitIdle is kept until then,
// or a client could empty it and get it back full by waiting out the idle
// time.
func (b *tokenBuckets) fullAt(now time.Time) bool {
	full := func(lim *rate.Limiter) bool {
		return lim.TokensAt(now) >= float64(lim.Burst())
	}
	return full(b.read) && full(b.write)
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
		resolve:    resolve,
		now:        time.Now,
		maxBuckets: tokenRateLimitMaxBuckets,
		buckets:    make(map[string]*tokenBuckets),
		clients:    make(map[string]*tokenBuckets),
	}, nil
}

// clientBucketKey returns the key of the bucket of the client at a remote
// address ("ip:port" of the connection, never a header a client can set): an
// IPv4 address, or the /64 of an IPv6 address, as a host has a whole /64 to
// send from. It returns "" for an address it can't read.
func clientBucketKey(remoteAddr string) string {
	ap, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return ""
	}
	addr := ap.Addr().Unmap().WithZone("")
	if addr.Is4() {
		return addr.String()
	}
	prefix, err := addr.Prefix(64)
	if err != nil {
		return ""
	}
	return prefix.String()
}

// newBuckets returns a full pair of buckets.
func (l *tokenRateLimiter) newBuckets() *tokenBuckets {
	return &tokenBuckets{
		read:  rate.NewLimiter(l.readRate, l.readBurst),
		write: rate.NewLimiter(l.writeRate, l.writeBurst),
	}
}

// bucketsLocked returns the buckets of key in m, new ones while m has room,
// else the shared ones.
func (l *tokenRateLimiter) bucketsLocked(m map[string]*tokenBuckets, key string) *tokenBuckets {
	if key != "" {
		if b := m[key]; b != nil {
			return b
		}
		if len(m) < l.maxBuckets {
			b := l.newBuckets()
			m[key] = b
			return b
		}
	}
	if l.shared == nil {
		l.shared = l.newBuckets()
	}
	return l.shared
}

// isReadRequest reports whether a request counts against the read limit.
func isReadRequest(req *http.Request) bool {
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// allow takes a request from the bucket of its token, or without a known
// token from the bucket of its client address (the remote address of the
// connection). It returns false and the time to wait when the bucket is
// empty.
func (l *tokenRateLimiter) allow(secret, remoteAddr string, read bool) (bool, time.Duration) {
	accessor := anonymousBucket
	if secret != "" && l.resolve != nil {
		accessor = l.resolve(secret)
	}
	now := l.now()

	l.mu.Lock()
	if now.Sub(l.lastSweep) > time.Minute {
		for _, m := range []map[string]*tokenBuckets{l.buckets, l.clients} {
			for k, b := range m {
				if now.Sub(b.seen) > tokenRateLimitIdle && b.fullAt(now) {
					delete(m, k)
				}
			}
		}
		l.lastSweep = now
	}
	var b *tokenBuckets
	if accessor != anonymousBucket {
		b = l.bucketsLocked(l.buckets, accessor)
	} else {
		b = l.bucketsLocked(l.clients, clientBucketKey(remoteAddr))
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

// rateLimitExempt reports whether the requests of a path are never limited:
// the health and status endpoints, which load balancers and Consul probe
// without a token. Behind a proxy or a NAT they share the address, and so
// the bucket, of other clients, who could use up their requests and have the
// server marked unhealthy.
func rateLimitExempt(path string) bool {
	switch path {
	case "/v1/agent/health", "/v1/status/leader", "/v1/status/peers":
		return true
	}
	return false
}

// rateLimited answers 429 with Retry-After when the token of the request, or
// without a known token its client address, is over its limit, and reports
// whether it did.
func (s *HTTPServer) rateLimited(resp http.ResponseWriter, req *http.Request) bool {
	if s.rateLimiter == nil || rateLimitExempt(req.URL.Path) {
		return false
	}
	var secret string
	s.parseToken(req, &secret)
	read := isReadRequest(req)
	ok, wait := s.rateLimiter.allow(secret, req.RemoteAddr, read)
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
	fmt.Fprintf(resp, "rate limit of the %s requests of this token or client address exceeded, retry after %s\n",
		kind, wait.Round(time.Millisecond))
	return true
}
