// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package helper

import (
	"time"
)

// Backoff returns the delay to wait before retry number attempt (counting
// from zero) of an operation, using capped exponential backoff: attempt 0
// waits backoffBase, and every further attempt doubles the previous delay
// until backoffLimit is reached. The result never exceeds backoffLimit.
//
// The doubling is done step by step and stops as soon as the cap would be
// crossed, so the result is correct for any attempt value (including
// math.MaxUint64) and can never overflow, wrap around or become negative.
// A non-positive backoffBase yields zero, and a negative backoffLimit is
// treated as zero.
func Backoff(backoffBase time.Duration, backoffLimit time.Duration, attempt uint64) time.Duration {
	if backoffLimit < 0 {
		backoffLimit = 0
	}
	if backoffBase <= 0 {
		return 0
	}
	if backoffBase >= backoffLimit {
		return backoffLimit
	}

	// backoffBase is positive and below the limit, so the loop below runs at
	// most ~63 times before the doubled value reaches the cap.
	backoff := backoffBase
	for i := uint64(0); i < attempt; i++ {
		if backoff > backoffLimit/2 {
			return backoffLimit
		}
		backoff *= 2
	}
	return backoff
}
