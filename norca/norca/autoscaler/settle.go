// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"sync"
	"time"
)

// settleTracker remembers since when a target has not been settled (see
// targetState.isSettled), for the settle timeout of the scale downs. It lives
// in the leader's memory: after a leader change the wait starts again, which
// only holds a scale down longer.
type settleTracker struct {
	l     sync.Mutex
	since map[targetKey]time.Time
}

func newSettleTracker() *settleTracker {
	return &settleTracker{since: map[targetKey]time.Time{}}
}

// observe records what an evaluation saw of a target and returns for how long
// the target has not been settled; zero for a settled one.
func (s *settleTracker) observe(key targetKey, isSettled bool, now time.Time) time.Duration {
	s.l.Lock()
	defer s.l.Unlock()

	if isSettled {
		delete(s.since, key)
		return 0
	}
	since, ok := s.since[key]
	if !ok {
		s.since[key] = now
		return 0
	}
	return now.Sub(since)
}

// forget drops a target whose policy is gone.
func (s *settleTracker) forget(key targetKey) {
	s.l.Lock()
	defer s.l.Unlock()
	delete(s.since, key)
}
