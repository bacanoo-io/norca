// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package fetch

import (
	"context"
	"fmt"
	"sync"
)

// keyedLock is a lock per key that a caller waits for no longer than its
// context. The network of a fetch runs under it: one lock for all would
// let a repository that does not answer hold up every other. The zero
// value is ready to use.
type keyedLock struct {
	mu   sync.Mutex
	held map[string]*heldLock
}

// heldLock is the lock of one key while someone holds it or waits for it.
type heldLock struct {
	token chan struct{}
	users int
}

// lock takes the lock of key, and returns what gives it back.
func (l *keyedLock) lock(ctx context.Context, key string) (unlock func(), err error) {
	l.mu.Lock()

	if l.held == nil {
		l.held = map[string]*heldLock{}
	}

	h := l.held[key]
	if h == nil {
		h = &heldLock{token: make(chan struct{}, 1)}
		l.held[key] = h
	}

	h.users++
	l.mu.Unlock()

	leave := func() {
		l.mu.Lock()
		defer l.mu.Unlock()

		h.users--
		if h.users == 0 {
			delete(l.held, key)
		}
	}

	select {
	case h.token <- struct{}{}:
		return func() {
			<-h.token
			leave()
		}, nil
	case <-ctx.Done():
		leave()

		return nil, fmt.Errorf("waiting for another fetch of the repository: %w", ctx.Err())
	}
}
