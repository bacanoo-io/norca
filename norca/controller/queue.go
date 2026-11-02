// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"sync"
	"time"
)

// Queue is a work queue of keys with the semantics controllers need:
//
//   - a key is queued at most once, however many times it is added before it
//     is processed;
//   - a key is never processed by two workers at the same time: if it is
//     added while being processed, it is queued again when Done is called;
//   - keys can be added with a delay (AddAfter) or with the delay of the
//     rate limiter (AddRateLimited).
type Queue[K comparable] struct {
	limiter RateLimiter[K]
	now     func() time.Time

	mu           sync.Mutex
	cond         *sync.Cond
	queue        []K
	dirty        map[K]struct{}
	processing   map[K]struct{}
	waiting      map[K]time.Time
	shuttingDown bool

	wake chan struct{}
	stop chan struct{}
}

// NewQueue returns a queue that uses limiter for AddRateLimited. A nil
// limiter means DefaultRateLimiter.
func NewQueue[K comparable](limiter RateLimiter[K]) *Queue[K] {
	if limiter == nil {
		limiter = DefaultRateLimiter[K]()
	}
	q := &Queue[K]{
		limiter:    limiter,
		now:        time.Now,
		dirty:      map[K]struct{}{},
		processing: map[K]struct{}{},
		waiting:    map[K]time.Time{},
		wake:       make(chan struct{}, 1),
		stop:       make(chan struct{}),
	}
	q.cond = sync.NewCond(&q.mu)
	go q.delayLoop()
	return q
}

// Add queues key for processing.
func (q *Queue[K]) Add(key K) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.addLocked(key)
}

func (q *Queue[K]) addLocked(key K) {
	if q.shuttingDown {
		return
	}
	if _, ok := q.dirty[key]; ok {
		return
	}
	q.dirty[key] = struct{}{}
	if _, ok := q.processing[key]; ok {
		return
	}
	q.queue = append(q.queue, key)
	q.cond.Signal()
}

// AddAfter queues key after the delay. If the key is already waiting, the
// earlier time wins.
func (q *Queue[K]) AddAfter(key K, delay time.Duration) {
	if delay <= 0 {
		q.Add(key)
		return
	}
	q.mu.Lock()
	if q.shuttingDown {
		q.mu.Unlock()
		return
	}
	at := q.now().Add(delay)
	if cur, ok := q.waiting[key]; !ok || at.Before(cur) {
		q.waiting[key] = at
	}
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// AddRateLimited queues key after the delay the rate limiter gives.
func (q *Queue[K]) AddRateLimited(key K) {
	q.AddAfter(key, q.limiter.When(key))
}

// Forget clears the retry history of key in the rate limiter.
func (q *Queue[K]) Forget(key K) { q.limiter.Forget(key) }

// NumRequeues returns how many times key was retried since the last Forget.
func (q *Queue[K]) NumRequeues(key K) int { return q.limiter.NumRequeues(key) }

// Get blocks until a key is available and returns it. The caller must call
// Done with the key when it finished processing it. Get returns shutdown =
// true once the queue was shut down.
func (q *Queue[K]) Get() (key K, shutdown bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.queue) == 0 && !q.shuttingDown {
		q.cond.Wait()
	}
	if len(q.queue) == 0 {
		return key, true
	}
	key = q.queue[0]
	var zero K
	q.queue[0] = zero
	q.queue = q.queue[1:]
	q.processing[key] = struct{}{}
	delete(q.dirty, key)
	return key, false
}

// Done marks the processing of key finished. If the key was added again in
// the meantime, it is queued.
func (q *Queue[K]) Done(key K) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.processing, key)
	if _, ok := q.dirty[key]; ok && !q.shuttingDown {
		q.queue = append(q.queue, key)
		q.cond.Signal()
	}
}

// Len returns the number of keys ready for processing.
func (q *Queue[K]) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.queue)
}

// Waiting returns the number of keys waiting for their delay.
func (q *Queue[K]) Waiting() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.waiting)
}

// ShutDown stops the queue: pending and delayed keys are dropped, Get
// returns shutdown = true, and keys added later are ignored. Keys being
// processed can still be marked Done.
func (q *Queue[K]) ShutDown() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.shuttingDown {
		return
	}
	q.shuttingDown = true
	q.queue = nil
	q.waiting = map[K]time.Time{}
	close(q.stop)
	q.cond.Broadcast()
}

// ShuttingDown reports whether ShutDown was called.
func (q *Queue[K]) ShuttingDown() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.shuttingDown
}

// delayLoop moves the keys whose delay elapsed to the queue.
func (q *Queue[K]) delayLoop() {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		q.mu.Lock()
		now := q.now()
		var next time.Time
		for key, at := range q.waiting {
			if !at.After(now) {
				delete(q.waiting, key)
				q.addLocked(key)
				continue
			}
			if next.IsZero() || at.Before(next) {
				next = at
			}
		}
		q.mu.Unlock()

		wait := time.Hour
		if !next.IsZero() {
			wait = next.Sub(now)
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)

		select {
		case <-q.stop:
			return
		case <-q.wake:
		case <-timer.C:
		}
	}
}
