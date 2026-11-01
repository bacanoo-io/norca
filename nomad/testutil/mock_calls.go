// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package testutil

import (
	"sync"
	"sync/atomic"
)

// CallCounter tallies calls by name, typically made by test doubles that run
// on several goroutines. The zero value is ready to use.
type CallCounter struct {
	// tally maps a call name to its *atomic.Int64 count.
	tally sync.Map
}

// NewCallCounter returns a CallCounter with no calls recorded.
func NewCallCounter() *CallCounter {
	return new(CallCounter)
}

// Inc records one call of name.
func (c *CallCounter) Inc(name string) {
	n, _ := c.tally.LoadOrStore(name, new(atomic.Int64))
	n.(*atomic.Int64).Add(1)
}

// Get returns the number of calls recorded so far for each name. The result is
// a snapshot that belongs to the caller.
func (c *CallCounter) Get() map[string]int {
	snapshot := map[string]int{}
	c.tally.Range(func(name, n any) bool {
		snapshot[name.(string)] = int(n.(*atomic.Int64).Load())
		return true
	})
	return snapshot
}
