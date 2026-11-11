// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"net/http"
	"net/http/httptest"
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
		ok, _ := l.allow("secret-a", true)
		must.True(t, ok)
	}
	ok, wait := l.allow("secret-a", true)
	must.False(t, ok)
	must.Eq(t, 500*time.Millisecond, wait)

	// Writes have their own bucket.
	ok, _ = l.allow("secret-a", false)
	must.True(t, ok)
	ok, wait = l.allow("secret-a", false)
	must.False(t, ok)
	must.Eq(t, time.Second, wait)

	// Another token has its own buckets.
	ok, _ = l.allow("secret-b", false)
	must.True(t, ok)

	// Anonymous requests and unknown tokens share one bucket.
	ok, _ = l.allow("", false)
	must.True(t, ok)
	ok, _ = l.allow("random-1", false)
	must.False(t, ok)

	// A refused request does not use up the bucket: after the wait it goes.
	now = now.Add(time.Second)
	ok, _ = l.allow("secret-a", false)
	must.True(t, ok)

	// Idle buckets are removed.
	now = now.Add(tokenRateLimitIdle + 2*time.Minute)
	l.allow("secret-b", true)
	l.mu.Lock()
	_, kept := l.buckets["accessor-a"]
	l.mu.Unlock()
	must.False(t, kept)
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
		ok, _ := l.allow("", true)
		must.True(t, ok)
	}
	for i := 0; i < 3; i++ {
		ok, _ := l.allow("", false)
		must.True(t, ok)
	}
	ok, _ := l.allow("", false)
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
