// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package testutil

import (
	"sync"

	testing "github.com/mitchellh/go-testing-interface"
)

// CallCounter counts how many times named calls were made, typically from
// within test doubles. It is safe for concurrent use.
type CallCounter struct {
	lock   sync.Mutex
	counts map[string]int
}

// NewCallCounter returns an empty CallCounter.
func NewCallCounter() *CallCounter {
	return &CallCounter{
		counts: make(map[string]int),
	}
}

// Inc increments the count for name by one.
func (c *CallCounter) Inc(name string) {
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.counts == nil {
		c.counts = make(map[string]int)
	}
	c.counts[name]++
}

// Get returns a snapshot of the current counts. The returned map is a copy
// and may be modified freely by the caller.
func (c *CallCounter) Get() map[string]int {
	c.lock.Lock()
	defer c.lock.Unlock()
	out := make(map[string]int, len(c.counts))
	for name, count := range c.counts {
		out[name] = count
	}
	return out
}

// AssertCalled fails the test if name was never counted.
func (c *CallCounter) AssertCalled(t testing.T, name string) {
	t.Helper()
	c.lock.Lock()
	count := c.counts[name]
	c.lock.Unlock()
	if count < 1 {
		t.Errorf("expected %q to be called, but it was not", name)
	}
}
