// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package sdk

import "math"

// CountFromFloat converts the result of a strategy to a count, rounding toward
// zero. A value out of the range of an int64 gives the nearest end of the
// range, and NaN gives 0: a plain conversion is not defined for them in Go
// and gives math.MinInt64 on amd64, which turned an infinite metric into a
// scale down to the minimum (review finding ASP-1).
func CountFromFloat(f float64) int64 {
	switch {
	case math.IsNaN(f):
		return 0
	case f >= math.MaxInt64:
		return math.MaxInt64
	case f <= math.MinInt64:
		return math.MinInt64
	}
	return int64(f)
}

// AddCount returns a+b, held at the ends of the range of an int64 instead of
// wrapping around (a max_scale_up of math.MaxInt64 must not turn into a
// negative count).
func AddCount(a, b int64) int64 {
	switch {
	case b > 0 && a > math.MaxInt64-b:
		return math.MaxInt64
	case b < 0 && a < math.MinInt64-b:
		return math.MinInt64
	}
	return a + b
}

// SubCount returns a-b, held at the ends of the range of an int64.
func SubCount(a, b int64) int64 {
	if b == math.MinInt64 {
		if a >= 0 {
			return math.MaxInt64
		}
		return a - b
	}
	return AddCount(a, -b)
}
