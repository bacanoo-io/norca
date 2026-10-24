// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	memdb "github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/nomad/structs"
)

// quotaEventsFromChanges returns the events of the Quota topic for a set of
// changes: quota specification upserts and deletes, and the exhaustion and
// freeing of quota dimensions. Unlike other events, their type does not
// depend on the Raft message type, since quota usage changes as a side
// effect of many different writes (plans, client updates, namespaces).
func quotaEventsFromChanges(tx ReadTxn, changes Changes) []structs.Event {
	var events []structs.Event
	var specs map[string]*structs.QuotaSpec

	for _, change := range changes.Changes {
		switch change.Table {
		case TableQuotaSpecs:
			if change.Deleted() {
				before, ok := change.Before.(*structs.QuotaSpec)
				if !ok {
					continue
				}
				events = append(events, quotaSpecEvent(structs.TypeQuotaSpecDeleted, before, changes.Index))
				continue
			}
			after, ok := change.After.(*structs.QuotaSpec)
			if !ok {
				continue
			}
			if specs == nil {
				specs = make(map[string]*structs.QuotaSpec)
			}
			specs[after.Name] = after
			events = append(events, quotaSpecEvent(structs.TypeQuotaSpecUpserted, after, changes.Index))

		case TableQuotaUsage:
			events = append(events, quotaUsageEvents(tx, change, specs, changes.Index)...)
		}
	}
	return events
}

func quotaSpecEvent(eventType string, spec *structs.QuotaSpec, index uint64) structs.Event {
	return structs.Event{
		Topic:   structs.TopicQuota,
		Type:    eventType,
		Key:     spec.Name,
		Index:   index,
		Payload: &structs.QuotaSpecEvent{QuotaSpec: spec},
	}
}

// quotaUsageEvents compares the exhausted dimensions of a quota usage before
// and after a change. The specification is taken from the same set of
// changes if it was modified, otherwise from the state before the change.
func quotaUsageEvents(tx ReadTxn, change memdb.Change, specs map[string]*structs.QuotaSpec, index uint64) []structs.Event {
	after, ok := change.After.(*structs.QuotaUsage)
	if !ok || after == nil {
		// Usages are only deleted together with their specification.
		return nil
	}
	before, _ := change.Before.(*structs.QuotaUsage)

	// The transaction reads the state before the change, so it holds the
	// previous version of the specification.
	var prevSpec *structs.QuotaSpec
	if raw, err := tx.First(TableQuotaSpecs, indexID, after.Name); err == nil && raw != nil {
		prevSpec = raw.(*structs.QuotaSpec)
	}
	spec := specs[after.Name]
	if spec == nil {
		spec = prevSpec
	}
	if spec == nil {
		return nil
	}

	var events []structs.Event
	for _, limit := range spec.Limits {
		used, ok := after.Used[limit.HashKey()]
		if !ok {
			continue
		}
		afterDims := limit.ExhaustedDimensions(used.UsageResources())

		// Compare with the usage before the change against the limit that
		// applied before the change, so raising or lowering a limit is
		// reported as well.
		var beforeDims []string
		if prev := before.RegionUsage(limit.Region); prev != nil {
			if prevLimit := prevSpec.LimitForRegion(limit.Region); prevLimit != nil {
				beforeDims = prevLimit.ExhaustedDimensions(prev.UsageResources())
			}
		}

		if added := quotaDimensionsDiff(afterDims, beforeDims); len(added) > 0 {
			events = append(events, quotaUsageEvent(structs.TypeQuotaExhausted, after, added, index))
		}
		if removed := quotaDimensionsDiff(beforeDims, afterDims); len(removed) > 0 {
			events = append(events, quotaUsageEvent(structs.TypeQuotaFreed, after, removed, index))
		}
	}
	return events
}

func quotaUsageEvent(eventType string, usage *structs.QuotaUsage, dims []string, index uint64) structs.Event {
	return structs.Event{
		Topic: structs.TopicQuota,
		Type:  eventType,
		Key:   usage.Name,
		Index: index,
		Payload: &structs.QuotaUsageEvent{
			QuotaUsage: usage,
			Dimensions: dims,
		},
	}
}

// quotaDimensionsDiff returns the elements of a that are not in b.
func quotaDimensionsDiff(a, b []string) []string {
	var out []string
OUTER:
	for _, x := range a {
		for _, y := range b {
			if x == y {
				continue OUTER
			}
		}
		out = append(out, x)
	}
	return out
}
