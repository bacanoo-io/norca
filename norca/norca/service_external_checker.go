// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"crypto/sha256"
	"encoding/binary"
	"path"
	"time"

	"github.com/hashicorp/go-metrics"

	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

const (
	// externalCheckerOutputNoNode is the output of the node checks of an
	// external service when no node may run them.
	externalCheckerOutputNoNode = "no node to run the checks"

	// externalCheckerOutputNoReport is the output of the node checks of an
	// external service whose checking node stopped reporting.
	externalCheckerOutputNoReport = "no report from the checking node"

	// externalCheckerStaleIntervals is how many check intervals may pass
	// without a report before the checks turn critical.
	externalCheckerStaleIntervals = 3
)

// checkerScore is the rendezvous hash of a registration and a node.
func checkerScore(regID, nodeID string) uint64 {
	sum := sha256.Sum256([]byte(regID + "\x00" + nodeID))
	return binary.BigEndian.Uint64(sum[:8])
}

// checkerEligible returns whether the node may run the checks selected by cb.
func checkerEligible(node *structs.Node, cb *structs.ExternalCheckedBy) bool {
	if !node.Ready() {
		return false
	}
	if cb.NodePool != "" && !node.IsInPool(cb.NodePool) {
		return false
	}
	if len(cb.Datacenters) == 0 {
		return true
	}
	for _, pattern := range cb.Datacenters {
		if ok, err := path.Match(pattern, node.Datacenter); err == nil && ok {
			return true
		}
	}
	return false
}

// pickCheckerNode returns the node that runs the checks of the registration:
// among the nodes selected by cb, the one with the highest rendezvous hash,
// so that a change of the nodes moves only the registrations of the nodes
// that went away (or the ones the new node wins). It returns nil when no node
// is eligible.
func pickCheckerNode(snap *state.StateStore, regID string, cb *structs.ExternalCheckedBy) (*structs.Node, error) {
	var best *structs.Node
	var bestScore uint64

	var iterNodes func() (next func() any, err error)
	if cb.NodePool != "" {
		iterNodes = func() (func() any, error) {
			iter, err := snap.NodesByNodePool(nil, cb.NodePool)
			if err != nil {
				return nil, err
			}
			return iter.Next, nil
		}
	} else {
		iterNodes = func() (func() any, error) {
			iter, err := snap.Nodes(nil)
			if err != nil {
				return nil, err
			}
			return iter.Next, nil
		}
	}
	next, err := iterNodes()
	if err != nil {
		return nil, err
	}
	for raw := next(); raw != nil; raw = next() {
		node := raw.(*structs.Node)
		if !checkerEligible(node, cb) {
			continue
		}
		score := checkerScore(regID, node.ID)
		if best == nil || score > bestScore || (score == bestScore && node.ID < best.ID) {
			best, bestScore = node, score
		}
	}
	return best, nil
}

// externalNodeCheckInterval returns the longest interval of the node checks.
func externalNodeCheckInterval(ext *structs.ServiceExternal) (longest int64) {
	for _, c := range ext.Checks {
		if c.IsNodeCheck() && int64(c.Interval) > longest {
			longest = int64(c.Interval)
		}
	}
	return longest
}

// assignCheckersLocked gives the registrations with a checked_by selector
// the node that runs their checks, and turns their node checks critical while
// there is no such node or the node does not report.
func (e *externalServices) assignCheckersLocked(regs []*structs.ServiceRegistration) error {
	now := e.now()
	snap := e.srv.fsm.State()

	var apply []*structs.ServiceRegistration
	for _, reg := range regs {
		if reg.External.CheckedBy == nil {
			continue
		}
		node, err := pickCheckerNode(snap, reg.ID, reg.External.CheckedBy)
		if err != nil {
			return err
		}
		next := reg.Copy()
		key := externalCheckKey{reg.Namespace, reg.ID, ""}

		switch {
		case node == nil:
			next.NodeID = ""
			delete(e.reported, key)
			setNodeChecksCritical(next, externalCheckerOutputNoNode)
		case node.ID != reg.NodeID:
			next.NodeID = node.ID
			if next.Datacenter == "" {
				next.Datacenter = node.Datacenter
			}
			// the new node has a full period to report
			e.reported[key] = now
		default:
			last, ok := e.reported[key]
			if !ok {
				e.reported[key] = now
				break
			}
			if stale := externalNodeCheckInterval(reg.External) * externalCheckerStaleIntervals; stale > 0 &&
				now.Sub(last) > time.Duration(stale) {
				setNodeChecksCritical(next, externalCheckerOutputNoReport)
			}
		}

		next.SetExternalHealth(reg, now)
		if next.NodeID == reg.NodeID && next.HealthEqual(reg) && next.Datacenter == reg.Datacenter {
			continue
		}
		apply = append(apply, next)
	}
	if len(apply) == 0 {
		return nil
	}
	metrics.IncrCounter([]string{"nomad", "service_registration", "external_checker_assigned"}, float32(len(apply)))
	_, err := e.writeLocked(apply)
	return err
}

// setNodeChecksCritical turns the checks run by a node critical.
func setNodeChecksCritical(reg *structs.ServiceRegistration, output string) {
	for _, def := range reg.External.Checks {
		if !def.IsNodeCheck() {
			continue
		}
		if state := reg.CheckState(def.ID); state != nil {
			state.Status = structs.ServiceHealthCritical
			state.Output = output
		}
	}
}
