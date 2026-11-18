// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/norca/structs/config"
	"github.com/shoenig/test/must"
)

func TestTokenRateLimiter(t *testing.T) {
	ci.Parallel(t)

	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	known := map[string]string{"secret-a": "accessor-a", "secret-b": "accessor-b"}
	l, err := newTokenRateLimiter(config.Limits{
		TokenReadRequestsPerSecond:  pointer.Of(2),
		TokenWriteRequestsPerSecond: pointer.Of(1),
	}, func(secret string) string { return known[secret] })
	must.NoError(t, err)
	l.now = func() time.Time { return now }

	// Reads: the burst defaults to the rate.
	for i := 0; i < 2; i++ {
		ok, _ := l.allow("secret-a", "192.0.2.1:1000", true)
		must.True(t, ok)
	}
	ok, wait := l.allow("secret-a", "192.0.2.1:1000", true)
	must.False(t, ok)
	must.Eq(t, 500*time.Millisecond, wait)

	// Writes have their own bucket.
	ok, _ = l.allow("secret-a", "192.0.2.1:1000", false)
	must.True(t, ok)
	ok, wait = l.allow("secret-a", "192.0.2.1:1000", false)
	must.False(t, ok)
	must.Eq(t, time.Second, wait)

	// Another token has its own buckets.
	ok, _ = l.allow("secret-b", "192.0.2.1:1000", false)
	must.True(t, ok)

	// Anonymous requests and unknown tokens of one client address share one
	// bucket, whatever the port and the token are.
	ok, _ = l.allow("", "192.0.2.1:1000", false)
	must.True(t, ok)
	ok, _ = l.allow("random-1", "192.0.2.1:2000", false)
	must.False(t, ok)
	ok, _ = l.allow("random-2", "192.0.2.1:1000", false)
	must.False(t, ok)

	// Another client address has its own bucket, which the first one does
	// not use up.
	ok, _ = l.allow("", "192.0.2.2:1000", false)
	must.True(t, ok)
	ok, _ = l.allow("random-1", "192.0.2.2:1000", false)
	must.False(t, ok)

	// A known token is not limited by the address it comes from.
	ok, _ = l.allow("secret-b", "192.0.2.1:1000", true)
	must.True(t, ok)

	// A refused request does not use up the bucket: after the wait it goes.
	now = now.Add(time.Second)
	ok, _ = l.allow("secret-a", "192.0.2.1:1000", false)
	must.True(t, ok)

	// Idle buckets are removed.
	now = now.Add(tokenRateLimitIdle + 2*time.Minute)
	l.allow("secret-b", "192.0.2.1:1000", true)
	l.mu.Lock()
	_, kept := l.buckets["accessor-a"]
	_, keptClient := l.clients["192.0.2.1"]
	l.mu.Unlock()
	must.False(t, kept)
	must.False(t, keptClient)
}

func TestClientBucketKey(t *testing.T) {
	ci.Parallel(t)

	cases := map[string]string{
		"192.0.2.1:1000":              "192.0.2.1",
		"192.0.2.1:2000":              "192.0.2.1",
		"[::ffff:192.0.2.1]:1000":     "192.0.2.1",
		"[2001:db8:1:2::1]:1000":      "2001:db8:1:2::/64",
		"[2001:db8:1:2:ffff:1:2:3]:9": "2001:db8:1:2::/64",
		"[2001:db8:1:3::1]:1000":      "2001:db8:1:3::/64",
		"[fe80::1%eth0]:1000":         "fe80::/64",
		"[::1]:1000":                  "::/64",
		"":                            "",
		"@":                           "",
		"192.0.2.1":                   "",
		"/run/norca.sock":             "",
		"203.0.113.9, 192.0.2.1:1000": "",
		"example.com:80":              "",
	}
	for in, want := range cases {
		must.Eq(t, want, clientBucketKey(in), must.Sprint(in))
	}
}

// Requests without a known token are limited per client address: an IPv6
// host can't take more buckets with the addresses of its /64, and addresses
// that can't be read share one bucket.
func TestTokenRateLimiter_ClientAddress(t *testing.T) {
	ci.Parallel(t)

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	l, err := newTokenRateLimiter(config.Limits{TokenReadRequestsPerSecond: pointer.Of(2)},
		func(string) string { return anonymousBucket })
	must.NoError(t, err)
	l.now = func() time.Time { return now }

	take := func(token, addr string) bool {
		ok, _ := l.allow(token, addr, true)
		return ok
	}

	// one client uses up its bucket with nonsense tokens ...
	must.True(t, take("nonsense-1", "198.51.100.7:1"))
	must.True(t, take("nonsense-2", "198.51.100.7:2"))
	must.False(t, take("", "198.51.100.7:3"))
	// ... and another address is not affected
	must.True(t, take("", "198.51.100.8:1"))

	// IPv6: one bucket per /64
	must.True(t, take("", "[2001:db8:1:2::1]:1"))
	must.True(t, take("", "[2001:db8:1:2::2]:1"))
	must.False(t, take("", "[2001:db8:1:2:aaaa:bbbb:cccc:dddd]:1"))
	must.True(t, take("", "[2001:db8:1:3::1]:1"))

	// no usable address: the shared bucket
	must.True(t, take("", ""))
	must.True(t, take("", "@"))
	must.False(t, take("", "/run/norca.sock"))

	l.mu.Lock()
	must.MapLen(t, 4, l.clients)
	must.MapLen(t, 0, l.buckets)
	l.mu.Unlock()
}

// The buckets of client addresses and of tokens are capped: beyond the cap a
// new address or token is limited by the shared bucket, never unlimited, and
// gets its own bucket again once idle ones are dropped.
func TestTokenRateLimiter_Cap(t *testing.T) {
	ci.Parallel(t)

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	l, err := newTokenRateLimiter(config.Limits{TokenReadRequestsPerSecond: pointer.Of(1)},
		func(secret string) string { return strings.TrimPrefix(secret, "secret-") })
	must.NoError(t, err)
	must.Eq(t, tokenRateLimitMaxBuckets, l.maxBuckets)
	l.now = func() time.Time { return now }
	l.maxBuckets = 2

	client := func(addr string) bool {
		ok, _ := l.allow("", addr, true)
		return ok
	}
	sizes := func() (int, int) {
		l.mu.Lock()
		defer l.mu.Unlock()
		return len(l.clients), len(l.buckets)
	}

	// two addresses fill the map
	must.True(t, client("192.0.2.1:1"))
	must.True(t, client("192.0.2.2:1"))
	// the next ones share one bucket, and are limited by it
	must.True(t, client("192.0.2.3:1"))
	must.False(t, client("192.0.2.4:1"))
	must.False(t, client("192.0.2.3:1"))
	for i := 0; i < 100; i++ {
		must.False(t, client(fmt.Sprintf("198.51.100.%d:1", i)))
	}
	clients, _ := sizes()
	must.Eq(t, 2, clients)
	// an address with a bucket keeps it
	must.False(t, client("192.0.2.1:1"))
	now = now.Add(time.Second)
	must.True(t, client("192.0.2.1:1"))
	must.True(t, client("192.0.2.2:1"))

	// tokens have their own cap and fall back to the same shared bucket
	token := func(accessor string) bool {
		ok, _ := l.allow("secret-"+accessor, "192.0.2.1:1", true)
		return ok
	}
	must.True(t, token("a"))
	must.True(t, token("b"))
	must.True(t, token("c")) // the shared bucket, refilled by the second
	must.False(t, token("d"))
	must.False(t, client("192.0.2.9:1"))
	must.False(t, token("a"))
	clients, tokens := sizes()
	must.Eq(t, 2, clients)
	must.Eq(t, 2, tokens)

	// idle buckets are dropped, and new addresses and tokens get their own
	now = now.Add(tokenRateLimitIdle + 2*time.Minute)
	must.True(t, client("192.0.2.3:1"))
	must.True(t, client("192.0.2.4:1"))
	must.True(t, token("c"))
	must.True(t, token("d"))
	l.mu.Lock()
	must.MapContainsKey(t, l.clients, "192.0.2.4")
	must.MapNotContainsKey(t, l.clients, "192.0.2.1")
	must.MapContainsKey(t, l.buckets, "d")
	l.mu.Unlock()
}

// An idle bucket is dropped only once it would be full again: a client that
// empties a bucket which takes longer than the idle time to refill does not
// get a full one back by waiting out the idle time (review finding SEC-23).
func TestTokenRateLimiter_IdleDropKeepsEmptyBucket(t *testing.T) {
	ci.Parallel(t)

	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	// 1200 requests at one per second: 20 minutes to refill
	l, err := newTokenRateLimiter(config.Limits{
		TokenReadRequestsPerSecond: pointer.Of(1),
		TokenReadBurst:             pointer.Of(1200),
	}, func(secret string) string { return strings.TrimPrefix(secret, "secret-") })
	must.NoError(t, err)
	l.now = func() time.Time { return now }

	// takeAll returns how many requests go at once
	takeAll := func(secret, addr string) int {
		n := 0
		for {
			ok, _ := l.allow(secret, addr, true)
			if !ok {
				return n
			}
			n++
		}
	}
	must.Eq(t, 1200, takeAll("", "192.0.2.1:1"))
	must.Eq(t, 1200, takeAll("secret-a", "192.0.2.1:1"))

	// after twelve idle minutes, 720 requests have come back, not 1200
	now = now.Add(tokenRateLimitIdle + 2*time.Minute)
	must.Eq(t, 720, takeAll("", "192.0.2.1:1"))
	must.Eq(t, 720, takeAll("secret-a", "192.0.2.1:1"))

	// once full again, an idle bucket is dropped
	now = now.Add(21 * time.Minute)
	l.allow("", "192.0.2.2:1", true)
	l.mu.Lock()
	must.MapNotContainsKey(t, l.clients, "192.0.2.1")
	must.MapNotContainsKey(t, l.buckets, "a")
	l.mu.Unlock()
}

// The client address is the one of the connection: headers a client sets do
// not choose the bucket.
func TestHTTP_TokenRateLimit_ClientAddress(t *testing.T) {
	ci.Parallel(t)

	httpTest(t, nil, func(s *TestAgent) {
		l, err := newTokenRateLimiter(config.Limits{TokenReadRequestsPerSecond: pointer.Of(2)},
			func(string) string { return anonymousBucket })
		must.NoError(t, err)
		s.Server.rateLimiter = l

		get := func(remoteAddr, forwardedFor string) int {
			req := httptest.NewRequest(http.MethodGet, "/v1/regions", nil)
			req.RemoteAddr = remoteAddr
			req.Header.Set("X-Nomad-Token", "nonsense")
			if forwardedFor != "" {
				req.Header.Set("X-Forwarded-For", forwardedFor)
				req.Header.Set("X-Real-IP", forwardedFor)
				req.Header.Set("Forwarded", "for="+forwardedFor)
			}
			resp := httptest.NewRecorder()
			s.Server.mux.ServeHTTP(resp, req)
			return resp.Code
		}

		must.Eq(t, http.StatusOK, get("203.0.113.5:4000", ""))
		must.Eq(t, http.StatusOK, get("203.0.113.5:4001", "198.51.100.1"))
		must.Eq(t, http.StatusTooManyRequests, get("203.0.113.5:4002", "198.51.100.2"))
		must.Eq(t, http.StatusTooManyRequests, get("203.0.113.5:4003", ""))

		// another client is served
		must.Eq(t, http.StatusOK, get("203.0.113.6:4000", "203.0.113.5"))
	})
}

func TestTokenRateLimiter_Config(t *testing.T) {
	ci.Parallel(t)

	l, err := newTokenRateLimiter(config.DefaultLimits(), nil)
	must.NoError(t, err)
	must.Nil(t, l)

	_, err = newTokenRateLimiter(config.Limits{TokenWriteBurst: pointer.Of(-1)}, nil)
	must.ErrorContains(t, err, "token_write_burst must be >= 0")

	// Only writes limited: reads pass.
	l, err = newTokenRateLimiter(config.Limits{
		TokenWriteRequestsPerSecond: pointer.Of(1),
		TokenWriteBurst:             pointer.Of(3),
	}, nil)
	must.NoError(t, err)
	for i := 0; i < 100; i++ {
		ok, _ := l.allow("", "192.0.2.1:1000", true)
		must.True(t, ok)
	}
	for i := 0; i < 3; i++ {
		ok, _ := l.allow("", "192.0.2.1:1000", false)
		must.True(t, ok)
	}
	ok, _ := l.allow("", "192.0.2.1:1000", false)
	must.False(t, ok)
}

// TestHTTP_TokenRateLimit checks the 429 answer with Retry-After of the
// HTTP API.
func TestHTTP_TokenRateLimit(t *testing.T) {
	ci.Parallel(t)

	httpTest(t, nil, func(s *TestAgent) {
		l, err := newTokenRateLimiter(config.Limits{TokenWriteRequestsPerSecond: pointer.Of(1)}, nil)
		must.NoError(t, err)
		s.Server.rateLimiter = l
		handler := s.Server.wrap(func(http.ResponseWriter, *http.Request) (interface{}, error) {
			return map[string]string{"ok": "yes"}, nil
		})

		resp := httptest.NewRecorder()
		handler(resp, httptest.NewRequest(http.MethodPut, "/v1/jobs", nil))
		must.Eq(t, http.StatusOK, resp.Code)

		resp = httptest.NewRecorder()
		handler(resp, httptest.NewRequest(http.MethodPut, "/v1/jobs", nil))
		must.Eq(t, http.StatusTooManyRequests, resp.Code)
		must.Eq(t, "1", resp.Header().Get("Retry-After"))
		must.StrContains(t, resp.Body.String(), "rate limit of the write requests")

		// Reads are not limited here.
		resp = httptest.NewRecorder()
		handler(resp, httptest.NewRequest(http.MethodGet, "/v1/jobs", nil))
		must.Eq(t, http.StatusOK, resp.Code)
	})
}

// The health and status endpoints are not limited: requests with unknown
// tokens, which use up the shared anonymous bucket, do not turn the probes
// of load balancers and Consul into 429.
func TestHTTP_TokenRateLimit_ProbesExempt(t *testing.T) {
	ci.Parallel(t)

	httpTest(t, nil, func(s *TestAgent) {
		l, err := newTokenRateLimiter(config.Limits{TokenReadRequestsPerSecond: pointer.Of(5)},
			func(string) string { return anonymousBucket })
		must.NoError(t, err)
		s.Server.rateLimiter = l

		get := func(path, token string) int {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			if token != "" {
				req.Header.Set("X-Nomad-Token", token)
			}
			resp := httptest.NewRecorder()
			s.Server.mux.ServeHTTP(resp, req)
			return resp.Code
		}

		// anyone empties the anonymous bucket
		for i := 0; i < 5; i++ {
			must.Eq(t, http.StatusOK, get("/v1/regions", "nonsense"))
		}
		must.Eq(t, http.StatusTooManyRequests, get("/v1/regions", ""))

		for _, path := range []string{
			"/v1/agent/health",
			"/v1/agent/health?type=server",
			"/v1/status/leader",
			"/v1/status/peers",
		} {
			for i := 0; i < 10; i++ {
				must.Eq(t, http.StatusOK, get(path, ""), must.Sprint(path))
				must.Eq(t, http.StatusOK, get(path, "nonsense"), must.Sprint(path))
			}
		}

		// and they take nothing from the bucket
		must.True(t, rateLimitExempt("/v1/agent/health"))
		must.False(t, rateLimitExempt("/v1/agent/self"))
		must.False(t, rateLimitExempt("/v1/jobs"))
	})
}
