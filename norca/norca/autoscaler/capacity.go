// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import "sync"

// capacityBook is what the blocked work trigger found, in its last look, of
// work that more nodes will not help: no node group can take it, the groups
// that could are at their maximum or the pool is, an allocation is too large
// for a node, or a constraint cannot be checked ahead of time.
//
// The policy of a task group reads it. A group whose allocations have waited
// for capacity for the settle timeout, and for which no capacity is coming,
// is not scaled up further: its count would only grow away from what runs.
//
// The trigger replaces the whole book at every look, so an entry lasts as
// long as the trigger keeps finding the work unhelped. It lives in the
// leader's memory and is empty after a leader change until the first look.
type capacityBook struct {
	l       sync.Mutex
	reasons map[targetKey]string
}

func newCapacityBook() *capacityBook {
	return &capacityBook{reasons: map[targetKey]string{}}
}

// replace sets what the last look found: the reason by task group.
func (b *capacityBook) replace(reasons map[targetKey]string) {
	b.l.Lock()
	defer b.l.Unlock()
	b.reasons = reasons
}

// unhelped returns why more nodes will not help the waiting allocations of a
// task group, "" when nothing says so.
func (b *capacityBook) unhelped(key targetKey) string {
	b.l.Lock()
	defer b.l.Unlock()
	return b.reasons[key]
}
