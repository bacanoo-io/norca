// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
)

// Drift detection compares the resources a release owns with their live
// state: a resource that was deleted, or whose content hash differs from the
// hash recorded when the release applied it, has drifted. Drift is only
// reported (the Drifted condition and a ReleaseDrifted event); it is never
// corrected automatically.

// computeDrift returns the drifted resources of a release.
func (m *releaseManager) computeDrift(snap *state.StateSnapshot, rel *structs.Release) ([]*structs.ReleaseDriftEntry, error) {
	var out []*structs.ReleaseDriftEntry
	for _, ref := range rel.Status.Resources {
		if ref == nil || ref.Hook != "" {
			continue
		}
		live, err := m.readLive(snap, ref)
		if err != nil {
			return nil, err
		}
		entry := &structs.ReleaseDriftEntry{Type: ref.Type, Namespace: ref.Namespace, Name: ref.Name}
		switch {
		case !live.exists():
			entry.Reason = "missing"
		case ref.Hash != "" && live.hash != ref.Hash:
			entry.Reason = "modified"
		default:
			continue
		}
		out = append(out, entry)
	}
	return out, nil
}

// checkDrift checks a release for drift and records the result in its
// Drifted condition. It returns the drifted resources.
func (m *releaseManager) checkDrift(namespace, name string) (*structs.Release, []*structs.ReleaseDriftEntry, error) {
	for attempt := 0; ; attempt++ {
		snap, err := m.srv.State().Snapshot()
		if err != nil {
			return nil, nil, err
		}
		obj, rel, err := releaseObject(snap, namespace, name)
		if err != nil {
			return nil, nil, err
		}
		if rel == nil {
			return nil, nil, nil
		}
		entries, err := m.computeDrift(snap, rel)
		if err != nil {
			return nil, nil, err
		}
		if rel.Status.Phase != structs.ReleasePhaseDeployed && rel.Status.Phase != structs.ReleasePhaseFailed {
			return rel, entries, nil
		}

		status, reason, msg := structs.ReleaseConditionFalse, "InSync", ""
		if len(entries) > 0 {
			status, reason, msg = structs.ReleaseConditionTrue, "Drifted", driftMessage(entries)
		}
		old := rel.Status.Condition(structs.ReleaseConditionDrifted)
		if old != nil && old.Status == status && old.Message == msg {
			return rel, entries, nil
		}
		becameDrifted := status == structs.ReleaseConditionTrue && (old == nil || old.Status != status)
		rel.Status.SetCondition(structs.ReleaseConditionDrifted, status, reason, msg, time.Now())
		written, err := m.writeRelease(rel, obj.ModifyIndex)
		if structs.IsObjectConflictError(err) && attempt < 3 {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		if becameDrifted {
			m.publishDrift(written, entries)
		}
		return written, entries, nil
	}
}

func driftMessage(entries []*structs.ReleaseDriftEntry) string {
	parts := make([]string, 0, len(entries))
	for i, e := range entries {
		if i == 5 {
			parts = append(parts, fmt.Sprintf("and %d more", len(entries)-i))
			break
		}
		name := e.Name
		if e.Namespace != "" {
			name = e.Namespace + "/" + e.Name
		}
		parts = append(parts, fmt.Sprintf("%s %s %s", e.Type, name, e.Reason))
	}
	return strings.Join(parts, ", ")
}

// publishDrift emits a ReleaseDrifted event on the CustomObject topic. Like
// admission denials, the event is not written to Raft and is published on
// the leader only.
func (m *releaseManager) publishDrift(rel *structs.Release, entries []*structs.ReleaseDriftEntry) {
	store := m.srv.State()
	if store == nil {
		return
	}
	broker, err := store.EventBroker()
	if err != nil {
		return
	}
	index, _ := store.LatestIndex()
	broker.Publish(&structs.Events{
		Index: index,
		Events: []structs.Event{{
			Topic:      structs.TopicCustomObject,
			Type:       structs.TypeReleaseDrifted,
			Key:        structs.ReleaseKindRef + "/" + rel.Name,
			Namespace:  rel.Namespace,
			FilterKeys: []string{structs.ReleaseKindRef},
			Index:      index,
			Payload: &structs.ReleaseDriftEvent{
				Namespace: rel.Namespace,
				Name:      rel.Name,
				Revision:  rel.Status.Revision,
				Resources: entries,
			},
		}},
	})
}

// checkAllDrift checks every release without a running operation.
func (m *releaseManager) checkAllDrift() {
	snap, err := m.srv.State().Snapshot()
	if err != nil {
		return
	}
	iter, err := snap.CustomObjects(nil, structs.ReleaseObjectGroup, structs.ReleaseKind, structs.AllNamespacesSentinel, state.SortDefault)
	if err != nil {
		return
	}
	var targets [][2]string
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		obj := raw.(*structs.CustomObject)
		targets = append(targets, [2]string{obj.Namespace, obj.Name})
	}
	for _, t := range targets {
		if m.isBusy(t[0], t[1]) {
			continue
		}
		if _, _, err := m.checkDrift(t[0], t[1]); err != nil {
			m.logger.Debug("drift check failed", "namespace", t[0], "release", t[1], "error", err)
		}
	}
}
