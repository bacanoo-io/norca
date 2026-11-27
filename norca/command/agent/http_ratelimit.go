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
// can't use up the requests of the others. An IPv6 client is also limited by
// its /48, see ipv6PrefixShare.
const anonymousBucket = ""

// ipv6PrefixShare is how many clients' worth of requests without a known
// token the /64s of one IPv6 /48 get together: the requests of a /64 take
// from its own buckets and from those of its /48, which have this many times
// the rates and bursts of a client.
//
// A /48 is a common assignment to one site and holds 65536 /64s, each a
// client of its own: without this level its owner gets 65536 times the limit
// of a client, and can keep the map of client addresses from refilling. 16
// leaves room for a site whose hosts send from several /64s, while one /48
// can keep at most about 16 client entries from refilling (each takes
// requests at the client rate), so holding the map of client addresses
// takes about tokenRateLimitMaxBuckets/16 = 1024 /48s. IPv4 clients have no
// such level: an address is already scarce.
const ipv6PrefixShare = 16

// tokenRateLimitIdle is how long the buckets of a token or of a client
// address that sends no request are kept before the periodic sweep drops
// them, and it drops them only once they are full again. A full map drops a
// full entry earlier for a new key, see bucketSet.evictLocked.
const tokenRateLimitIdle = 10 * time.Minute

// tokenRateLimitMaxBuckets is the most tokens, the most client addresses,
// and the most IPv6 /48s, that have buckets of their own at a time. A new
// token, address or /48 beyond it takes the place of an entry whose buckets
// are full, and only when it finds none it is limited by the shared bucket
// of its map: the memory of the limiter is bounded, and no request goes
// unlimited.
const tokenRateLimitMaxBuckets = 16384

// tokenRateLimitEvictScan is the most entries of a full map that a new key
// looks at for one whose buckets are full, so that the work of a request is
// bounded however the map is filled. See bucketSet.evictLocked.
const tokenRateLimitEvictScan = 32

// tokenRateLimiter keeps a read and a write token bucket per ACL token, and
// per client address and IPv6 /48 for the requests without a known token.
type tokenRateLimiter struct {
	// resolve returns the accessor ID of the ACL token with the secret, or
	// anonymousBucket.
	resolve func(secret string) string

	now func() time.Time

	// maxBuckets is the cap of each of the maps.
	maxBuckets int

	mu sync.Mutex
	// buckets are those of the ACL tokens, by accessor ID.
	buckets *bucketSet
	// clients are those of the client addresses, by the client key of
	// clientBucketKeys. Its shared bucket is the one of buckets: it limits
	// the requests that have no bucket of their own, those without a usable
	// client address, and those of new tokens and addresses while their map
	// is full and has no entry to evict.
	clients *bucketSet
	// prefixes are those of the IPv6 /48s, by the prefix key of
	// clientBucketKeys, with ipv6PrefixShare times the rates and bursts, and
	// a shared bucket of their own.
	prefixes  *bucketSet
	lastSweep time.Time
}

type tokenBuckets struct {
	read, write *rate.Limiter
	seen        time.Time

	// key, newer and older place an entry of a bucketSet in the map and in
	// the list; the shared buckets have none.
	key          string
	newer, older *tokenBuckets
}

// bucketSet is a bounded map of buckets, with the list of its entries from
// the most to the least recently used.
type bucketSet struct {
	readRate, writeRate   rate.Limit
	readBurst, writeBurst int

	m              map[string]*tokenBuckets
	newest, oldest *tokenBuckets

	// shared limits the requests whose key finds no room in m.
	shared *tokenBuckets
}

// newBucketSet returns an empty set with buckets of the rates and bursts.
// shared is the shared bucket of the set, nil for one of its own.
func newBucketSet(readRate, writeRate rate.Limit, readBurst, writeBurst int, shared *tokenBuckets) *bucketSet {
	s := &bucketSet{
		readRate: readRate, writeRate: writeRate,
		readBurst: readBurst, writeBurst: writeBurst,
		m: make(map[string]*tokenBuckets),
	}
	if shared == nil {
		shared = s.newBuckets()
	}
	s.shared = shared
	return s
}

// newBuckets returns a full pair of buckets of the rates of the set.
func (s *bucketSet) newBuckets() *tokenBuckets {
	return &tokenBuckets{
		read:  rate.NewLimiter(s.readRate, s.readBurst),
		write: rate.NewLimiter(s.writeRate, s.writeBurst),
	}
}

// pushNewestLocked puts b, which is in no list, at the newest end.
func (s *bucketSet) pushNewestLocked(b *tokenBuckets) {
	b.older, b.newer = s.newest, nil
	if s.newest != nil {
		s.newest.newer = b
	} else {
		s.oldest = b
	}
	s.newest = b
}

// unlinkLocked takes b out of the list.
func (s *bucketSet) unlinkLocked(b *tokenBuckets) {
	if b.newer != nil {
		b.newer.older = b.older
	} else {
		s.newest = b.older
	}
	if b.older != nil {
		b.older.newer = b.newer
	} else {
		s.oldest = b.newer
	}
	b.newer, b.older = nil, nil
}

// removeLocked drops the entry b.
func (s *bucketSet) removeLocked(b *tokenBuckets) {
	s.unlinkLocked(b)
	delete(s.m, b.key)
}

// getLocked returns the buckets of key: the ones it has, else new ones while
// the map has fewer than capacity entries or an entry to evict, else the
// shared ones. An empty key gets the shared ones.
func (s *bucketSet) getLocked(key string, now time.Time, capacity int) *tokenBuckets {
	if key == "" {
		return s.shared
	}
	if b := s.m[key]; b != nil {
		s.unlinkLocked(b)
		s.pushNewestLocked(b)
		return b
	}
	if len(s.m) >= capacity && !s.evictLocked(now) {
		return s.shared
	}
	b := s.newBuckets()
	b.key = key
	s.m[key] = b
	s.pushNewestLocked(b)
	return b
}

// evictLocked drops the least recently used entry whose buckets are full at
// now, of the oldest tokenRateLimitEvictScan entries, and reports whether it
// found one. Dropping full buckets gives the client nothing back: it would
// find them full if they were kept.
//
// The entries it finds not full it moves to the newest end, so that the
// next scan looks at others: a few emptied entries at the oldest end do not
// hide the full ones behind them. A client can keep new keys out of buckets
// of their own only by keeping nearly every entry of the map from refilling,
// which takes requests at the limited rate for each of them, and a scan that
// finds no full entry costs at most tokenRateLimitEvictScan checks.
func (s *bucketSet) evictLocked(now time.Time) bool {
	for i := min(tokenRateLimitEvictScan, len(s.m)); i > 0; i-- {
		b := s.oldest
		if b.fullAt(now) {
			s.removeLocked(b)
			return true
		}
		s.unlinkLocked(b)
		s.pushNewestLocked(b)
	}
	return false
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
	buckets := newBucketSet(limit(readRate), limit(writeRate), readBurst, writeBurst, nil)
	return &tokenRateLimiter{
		resolve:    resolve,
		now:        time.Now,
		maxBuckets: tokenRateLimitMaxBuckets,
		buckets:    buckets,
		clients:    newBucketSet(limit(readRate), limit(writeRate), readBurst, writeBurst, buckets.shared),
		prefixes: newBucketSet(prefixRate(limit(readRate)), prefixRate(limit(writeRate)),
			prefixBurst(readBurst), prefixBurst(writeBurst), nil),
	}, nil
}

// prefixRate returns the rate of the buckets of an IPv6 /48.
func prefixRate(r rate.Limit) rate.Limit {
	if r == rate.Inf {
		return rate.Inf
	}
	return r * ipv6PrefixShare
}

// prefixBurst returns the burst of the buckets of an IPv6 /48.
func prefixBurst(b int) int {
	if b > math.MaxInt/ipv6PrefixShare {
		return math.MaxInt
	}
	return b * ipv6PrefixShare
}

// clientBucketKeys returns the keys of the buckets of the client at a remote
// address ("ip:port" of the connection, never a header a client can set).
// The client key is an IPv4 address, or the /64 of an IPv6 address, as a
// host has a whole /64 to send from; the prefix key is the /48 of an IPv6
// address, and "" for IPv4. Both are "" for an address it can't read.
func clientBucketKeys(remoteAddr string) (client, prefix string) {
	ap, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return "", ""
	}
	addr := ap.Addr().Unmap().WithZone("")
	if addr.Is4() {
		return addr.String(), ""
	}
	p64, err := addr.Prefix(64)
	if err != nil {
		return "", ""
	}
	p48, err := addr.Prefix(48)
	if err != nil {
		return "", ""
	}
	return p64.String(), p48.String()
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
// connection) and, for IPv6, also from the bucket of its /48. It returns
// false and the time to wait when a bucket is empty; a request the /48
// refuses is given back to the bucket of the client.
func (l *tokenRateLimiter) allow(secret, remoteAddr string, read bool) (bool, time.Duration) {
	accessor := anonymousBucket
	if secret != "" && l.resolve != nil {
		accessor = l.resolve(secret)
	}
	now := l.now()

	l.mu.Lock()
	if now.Sub(l.lastSweep) > time.Minute {
		for _, s := range []*bucketSet{l.buckets, l.clients, l.prefixes} {
			for _, b := range s.m {
				if now.Sub(b.seen) > tokenRateLimitIdle && b.fullAt(now) {
					s.removeLocked(b)
				}
			}
		}
		l.lastSweep = now
	}
	// b is the bucket of the token or of the client, p the one of the /48
	var b, p *tokenBuckets
	if accessor != anonymousBucket {
		b = l.buckets.getLocked(accessor, now, l.maxBuckets)
	} else {
		client, prefix := clientBucketKeys(remoteAddr)
		b = l.clients.getLocked(client, now, l.maxBuckets)
		if prefix != "" {
			p = l.prefixes.getLocked(prefix, now, l.maxBuckets)
			p.seen = now
		}
	}
	b.seen = now
	l.mu.Unlock()

	r, wait := b.take(now, read)
	if r == nil {
		return false, wait
	}
	if p != nil {
		if pr, wait := p.take(now, read); pr == nil {
			// give the request back to the bucket of the client
			r.CancelAt(now)
			return false, wait
		}
	}
	return true, 0
}

// take takes a request from the read or the write bucket at now. It returns
// the reservation, or nil and the time to wait when the bucket is empty.
func (b *tokenBuckets) take(now time.Time, read bool) (*rate.Reservation, time.Duration) {
	lim := b.write
	if read {
		lim = b.read
	}
	r := lim.ReserveN(now, 1)
	if !r.OK() {
		return nil, time.Second
	}
	if d := r.DelayFrom(now); d > 0 {
		r.CancelAt(now)
		return nil, d
	}
	return r, 0
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
	metrics.IncrCounterWithLabels([]string{"http", "rate_limited"}, 1,
		[]metrics.Label{{Name: "kind", Value: kind}})
	resp.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
	resp.WriteHeader(http.StatusTooManyRequests)
	fmt.Fprintf(resp, "rate limit of the %s requests of this token or client address exceeded, retry after %s\n",
		kind, wait.Round(time.Millisecond))
	return true
}
