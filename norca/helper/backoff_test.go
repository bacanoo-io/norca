// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package helper

import (
	"fmt"
	"math"
	"math/big"
	"testing"
	"time"
)

// exactBackoff is the specification of Backoff written with arbitrary
// precision integers: base·2^attempt, capped at limit, where a non-positive
// base gives zero and a negative limit counts as zero. Being exact, it
// cannot share an overflow bug with the implementation it checks.
func exactBackoff(base, limit time.Duration, attempt uint64) time.Duration {
	if limit < 0 {
		limit = 0
	}
	if base <= 0 {
		return 0
	}
	// With base >= 1, base·2^64 exceeds every Duration, so larger attempts
	// are capped the same way as attempt 64.
	shift := uint(min(attempt, 64))
	delay := new(big.Int).Lsh(big.NewInt(int64(base)), shift)
	if delay.Cmp(big.NewInt(int64(limit))) > 0 {
		return limit
	}
	return time.Duration(delay.Int64())
}

// TestBackoffAgainstExactArithmetic compares Backoff with exactBackoff on
// every combination of awkward bases, limits and attempt numbers: signs,
// zero, odd values, the largest Duration and attempts around the 63/64-bit
// boundary up to the largest uint64.
func TestBackoffAgainstExactArithmetic(t *testing.T) {
	bases := []time.Duration{
		-time.Second, 0, 1, 3, time.Millisecond, 50 * time.Millisecond,
		time.Second, time.Minute, time.Hour,
	}
	limits := []time.Duration{
		-time.Second, 0, 7, 8 * time.Second, time.Minute, time.Hour,
		math.MaxInt64,
	}
	attempts := []uint64{
		0, 1, 2, 3, 5, 6, 34, 61, 62, 63, 64, 65, 1 << 40, math.MaxUint64,
	}

	for _, base := range bases {
		for _, limit := range limits {
			for _, attempt := range attempts {
				want := exactBackoff(base, limit, attempt)
				if got := Backoff(base, limit, attempt); got != want {
					t.Errorf("Backoff(%d, %d, %d) = %d, want %d",
						base, limit, attempt, got, want)
				}
			}
		}
	}
}

// TestBackoffDoublesUntilCapped walks a long series of attempts and checks
// the step rule directly: every delay is twice the one before, until it is
// pinned at the limit for good.
func TestBackoffDoublesUntilCapped(t *testing.T) {
	series := []struct{ base, limit time.Duration }{
		{time.Millisecond, time.Second},
		{time.Millisecond, time.Hour},
		{3, 7},
		{1, math.MaxInt64},
	}
	for _, s := range series {
		prev := Backoff(s.base, s.limit, 0)
		if prev != s.base {
			t.Fatalf("base %d limit %d: first delay %d, want the base", s.base, s.limit, prev)
		}
		for attempt := uint64(1); attempt <= 200; attempt++ {
			next := Backoff(s.base, s.limit, attempt)
			want := s.limit
			if prev <= s.limit/2 {
				want = 2 * prev
			}
			if next != want {
				t.Fatalf("base %d limit %d attempt %d: delay %d after %d, want %d",
					s.base, s.limit, attempt, next, prev, want)
			}
			prev = next
		}
		if prev != s.limit {
			t.Fatalf("base %d limit %d: series ends at %d, not at the limit", s.base, s.limit, prev)
		}
	}
}

// FuzzBackoff checks Backoff against exactBackoff on arbitrary inputs.
func FuzzBackoff(f *testing.F) {
	f.Add(int64(time.Second), int64(time.Minute), uint64(6))
	f.Add(int64(1), int64(math.MaxInt64), uint64(62))
	f.Add(int64(-1), int64(-1), uint64(math.MaxUint64))
	f.Fuzz(func(t *testing.T, base, limit int64, attempt uint64) {
		b, l := time.Duration(base), time.Duration(limit)
		if got, want := Backoff(b, l, attempt), exactBackoff(b, l, attempt); got != want {
			t.Fatalf("Backoff(%d, %d, %d) = %d, want %d", base, limit, attempt, got, want)
		}
	})
}

func ExampleBackoff() {
	for attempt := uint64(0); attempt < 5; attempt++ {
		fmt.Println(Backoff(time.Second, 5*time.Second, attempt))
	}
	// Output:
	// 1s
	// 2s
	// 4s
	// 5s
	// 5s
}
