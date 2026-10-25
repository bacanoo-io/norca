package ratelimit

import (
	"testing"
	"time"
)

// clock is a controllable time source for deterministic tests.
type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func newTestLimiter(rate, burst float64) (*Limiter, *clock) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	l := New(rate, burst)
	l.now = c.now
	return l, c
}

func TestBurstThenThrottle(t *testing.T) {
	l, _ := newTestLimiter(1, 3) // 1/s, burst 3

	// A fresh key spends its full burst, then is denied.
	for i := 0; i < 3; i++ {
		if !l.Allow("1.2.3.4") {
			t.Fatalf("request %d should be allowed (within burst)", i+1)
		}
	}
	if l.Allow("1.2.3.4") {
		t.Fatal("4th request should be denied (burst exhausted)")
	}
}

func TestRefillOverTime(t *testing.T) {
	l, c := newTestLimiter(2, 2) // 2/s, burst 2
	l.Allow("k")
	l.Allow("k") // drain
	if l.Allow("k") {
		t.Fatal("should be denied while empty")
	}
	c.add(500 * time.Millisecond) // +1 token at 2/s
	if !l.Allow("k") {
		t.Fatal("should be allowed after a partial refill")
	}
	if l.Allow("k") {
		t.Fatal("only one token should have refilled")
	}
}

func TestKeysAreIndependent(t *testing.T) {
	l, _ := newTestLimiter(1, 1)
	if !l.Allow("a") || !l.Allow("b") {
		t.Fatal("distinct keys must not share a bucket")
	}
	if l.Allow("a") {
		t.Fatal("key a should now be throttled")
	}
}

func TestSweepRemovesIdleFullBuckets(t *testing.T) {
	l, c := newTestLimiter(1, 2)
	l.Allow("busy") // partially drained (1 token left), recently used
	c.add(10 * time.Minute)
	l.Allow("fresh") // full burst, just touched
	// "busy" has refilled to full and is old → swept; "fresh" is recent → kept.
	if n := l.Sweep(time.Minute); n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}
	if _, ok := l.buckets["busy"]; ok {
		t.Fatal("idle full bucket should have been swept")
	}
	if _, ok := l.buckets["fresh"]; !ok {
		t.Fatal("recently-used bucket should be kept")
	}
}
