// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"fmt"

	"github.com/hashicorp/nomad/nomad/structs"
)

// topicIndexTables maps an event topic to the state store tables whose writes
// produce the topic's events. After a snapshot restore the event broker holds
// no events; the restored index of these tables tells it from which index on
// its view of the topic is complete, so exact-index subscriptions to a topic
// that did not change recently do not have to resynchronize after a restore.
//
// A table may only be listed if every write producing an event of the topic
// also updates the table's entry in the index table. Topics not listed fall
// back to the latest index of the restored state, which is always safe.
var topicIndexTables = map[structs.Topic][]string{
	structs.TopicVariables: {TableVariables},
}

// eventBaseIndexes returns the latest index of the state visible in txn and,
// for the topics in topicIndexTables, the latest index of their tables.
func eventBaseIndexes(txn ReadTxn) (uint64, map[structs.Topic]uint64, error) {
	iter, err := txn.Get("index", "id")
	if err != nil {
		return 0, nil, fmt.Errorf("index lookup failed: %v", err)
	}

	var latest uint64
	tableIndexes := make(map[string]uint64)
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		entry := raw.(*IndexEntry)
		tableIndexes[entry.Key] = entry.Value
		latest = max(latest, entry.Value)
	}

	byTopic := make(map[structs.Topic]uint64, len(topicIndexTables))
	for topic, tables := range topicIndexTables {
		var index uint64
		for _, table := range tables {
			index = max(index, tableIndexes[table])
		}
		byTopic[topic] = index
	}
	return latest, byTopic, nil
}
