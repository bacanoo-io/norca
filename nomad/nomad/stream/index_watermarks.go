// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package stream

import (
	"sync"

	"github.com/hashicorp/nomad/nomad/structs"
)

// indexWatermarks tracks, per topic, the highest Raft index at which events
// may be missing from the event buffer. A subscription that must start
// exactly at an index can be served only if that index is above the
// watermark of every topic it subscribes to; otherwise it would silently
// miss events.
//
// Events can be missing for two reasons:
//   - they were dropped from the head of the buffer when it was full
//   - they were applied before the buffer existed, i.e. they are part of a
//     state store snapshot the server restored (see SetBaseIndexes)
type indexWatermarks struct {
	mu sync.RWMutex

	// base is the index up to which the state was restored without events.
	base uint64

	// baseByTopic optionally narrows base per topic: the restored state holds
	// no change of the topic after this index.
	baseByTopic map[structs.Topic]uint64

	// dropped is the highest index of a dropped buffer item that contained
	// an event of the topic.
	dropped map[structs.Topic]uint64

	// droppedAll is the highest index of any dropped buffer item.
	droppedAll uint64
}

func newIndexWatermarks() *indexWatermarks {
	return &indexWatermarks{
		dropped: make(map[structs.Topic]uint64),
	}
}

// setBase records the index the state was restored at, along with optional
// tighter per topic indexes.
func (w *indexWatermarks) setBase(base uint64, byTopic map[structs.Topic]uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.base = base
	w.baseByTopic = make(map[structs.Topic]uint64, len(byTopic))
	for topic, index := range byTopic {
		w.baseByTopic[topic] = min(index, base)
	}
}

// recordDropped records the events of an item dropped from the buffer.
func (w *indexWatermarks) recordDropped(item *bufferItem) {
	if item == nil || item.Events == nil || len(item.Events.Events) == 0 {
		return
	}
	index := item.Events.Index

	w.mu.Lock()
	defer w.mu.Unlock()

	w.droppedAll = max(w.droppedAll, index)
	for _, event := range item.Events.Events {
		w.dropped[event.Topic] = max(w.dropped[event.Topic], index)
	}
}

// missingAt returns the highest index at which an event of the given
// topics may be missing from the buffer.
func (w *indexWatermarks) missingAt(topics map[structs.Topic][]string) uint64 {
	w.mu.RLock()
	defer w.mu.RUnlock()

	if _, ok := topics[structs.TopicAll]; ok {
		return max(w.base, w.droppedAll)
	}

	var missing uint64
	for topic := range topics {
		base, ok := w.baseByTopic[topic]
		if !ok {
			base = w.base
		}
		missing = max(missing, base, w.dropped[topic])
	}
	return missing
}
