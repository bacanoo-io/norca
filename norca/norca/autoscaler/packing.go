// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"regexp"
	"strings"

	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// Unplaced is the demand of one task group of a blocked evaluation: Count
// allocations of the same size that the scheduler could not place.
type Unplaced struct {
	Namespace string
	Job       string
	Group     string
	Count     int

	// EvalID is the blocked evaluation the demand comes from.
	EvalID string

	// The size of one allocation: CPU in MHz, memory and disk in MB.
	CPU, Memory, Disk float64

	// Distinct are the distinct_property constraints the allocations are
	// under. Unverifiable is the text of a constraint on a property of the
	// new node that cannot be known ahead of time; the work then waits.
	Distinct     []DistinctProperty
	Unverifiable string

	// Classes are the constraints on ${node.class}: a node group helps only if
	// the class of its nodes passes all of them.
	Classes []ClassConstraint

	// Exclusive is set when every allocation needs a node of its own
	// (distinct_hosts, a static port that every node has taken).
	Exclusive bool
}

// DistinctProperty is a distinct_property constraint: at most Limit
// allocations of the job (or group) may have the same value of Target. Used
// counts the live allocations by value.
type DistinctProperty struct {
	Target string
	Limit  int
	Used   map[string]int
}

// ClassConstraint is a constraint on the class of a node.
type ClassConstraint struct {
	Operand string
	Value   string
}

// ClassConstraintSupported reports whether a constraint on the node class can
// be checked against the class of a node group: the scheduler's operand is one
// that check knows and the value is a literal, not the property of a node.
func ClassConstraintSupported(operand, value string) bool {
	if strings.Contains(value, "${") {
		return false
	}
	_, known := ClassConstraint{Operand: operand, Value: value}.check("")
	return known
}

// classAllowed reports whether a node class passes all the constraints.
func classAllowed(cs []ClassConstraint, class string) bool {
	for _, c := range cs {
		if ok, _ := c.check(class); !ok {
			return false
		}
	}
	return true
}

// check reports whether a node class passes the constraint, as the scheduler
// checks it: the class of a node is always set (it may be empty), and a set
// operand reads the class as a list separated by commas. known is false for an
// operand it does not handle.
func (c ClassConstraint) check(class string) (ok, known bool) {
	switch c.Operand {
	case "=", "==", "is":
		return class == c.Value, true
	case "!=", "not":
		return class != c.Value, true
	case structs.ConstraintRegex:
		re, err := regexp.Compile(c.Value)
		return err == nil && re.MatchString(class), true
	case structs.ConstraintSetContains, structs.ConstraintSetContainsAll:
		have := splitSet(class)
		for v := range splitSet(c.Value) {
			if !have[v] {
				return false, true
			}
		}
		return true, true
	case structs.ConstraintSetContainsAny:
		have := splitSet(class)
		for v := range splitSet(c.Value) {
			if have[v] {
				return true, true
			}
		}
		return false, true
	case structs.ConstraintAttributeIsSet:
		return true, true
	case structs.ConstraintAttributeIsNotSet:
		return false, true
	}
	return false, false
}

// splitSet reads a list separated by commas, as the set operands of the
// scheduler do.
func splitSet(s string) map[string]bool {
	out := map[string]bool{}
	for v := range strings.SplitSeq(s, ",") {
		out[strings.TrimSpace(v)] = true
	}
	return out
}

// NodeGroupKey is what a new node of a node group would be: its node pool,
// datacenter and node class.
type NodeGroupKey struct {
	Pool, Datacenter, Class string
}

// NodeSize is the allocatable size of one node of a node group.
type NodeSize struct {
	CPU, Memory, Disk float64
}

// fits reports whether an allocation of the demand fits a node of size n.
func (u Unplaced) fits(n NodeSize) bool {
	return u.CPU <= n.CPU && u.Memory <= n.Memory && u.Disk <= n.Disk
}

// Restrict returns the part of the demand that a new node of the group could
// take, and false when there is none. A distinct_property constraint on the
// datacenter or the node pool is checked against the value the new node would
// have: when the allocations that are already there use up the limit of that
// value, a node of this group does not help (the node must go to a group with
// another value), and otherwise the count is capped at what is left. A
// constraint on anything else is not known about a node that does not exist
// yet, so the demand is left out (see Unverifiable). A constraint on the node
// class is checked against the class of the group.
func (u Unplaced) Restrict(g NodeGroupKey) (Unplaced, bool) {
	if u.Unverifiable != "" {
		return u, false
	}
	if !classAllowed(u.Classes, g.Class) {
		return u, false
	}
	for _, d := range u.Distinct {
		var value string
		switch d.Target {
		case "${node.datacenter}":
			value = g.Datacenter
		case "${node.pool}":
			value = g.Pool
		default:
			return u, false
		}
		left := d.Limit - d.Used[value]
		if left <= 0 {
			return u, false
		}
		u.Count = min(u.Count, left)
	}
	return u, true
}
