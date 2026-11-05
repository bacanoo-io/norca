// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"regexp"
	"slices"
	"strings"
)

// Unplaced is the demand of one task group of a blocked evaluation: Count
// allocations of the same size that the scheduler could not place.
type Unplaced struct {
	Namespace string
	Job       string
	Group     string
	Count     int

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
// be checked against the class of a node group: the operand is one of those
// below and the value is a literal, not the property of a node.
func ClassConstraintSupported(operand, value string) bool {
	if strings.Contains(value, "${") {
		return false
	}
	switch operand {
	case "=", "==", "is", "!=", "not", "regexp", "set_contains", "set_contains_all",
		"set_contains_any", "is_set", "is_not_set":
		return true
	}
	return false
}

// classAllowed reports whether a node class passes all the constraints.
func classAllowed(cs []ClassConstraint, class string) bool {
	for _, c := range cs {
		if !c.matches(class) {
			return false
		}
	}
	return true
}

// matches reports whether a node class passes the constraint, as the
// scheduler checks it: the class of a node is always set (it may be empty),
// and a set operand reads the class as a list separated by commas.
func (c ClassConstraint) matches(class string) bool {
	set := func(s string) map[string]bool {
		out := map[string]bool{}
		for _, v := range strings.Split(s, ",") {
			out[strings.TrimSpace(v)] = true
		}
		return out
	}
	switch c.Operand {
	case "=", "==", "is":
		return class == c.Value
	case "!=", "not":
		return class != c.Value
	case "regexp":
		re, err := regexp.Compile(c.Value)
		return err == nil && re.MatchString(class)
	case "set_contains", "set_contains_all":
		have := set(class)
		for v := range set(c.Value) {
			if !have[v] {
				return false
			}
		}
		return true
	case "set_contains_any":
		have := set(class)
		for v := range set(c.Value) {
			if have[v] {
				return true
			}
		}
		return false
	case "is_set":
		return true
	case "is_not_set":
		return false
	}
	return false
}

// NodeSize is the allocatable size of one node of a node group.
type NodeSize struct {
	CPU, Memory, Disk float64
}

// fits reports whether an allocation of the demand fits a node of size n.
func (u Unplaced) fits(n NodeSize) bool {
	return u.CPU <= n.CPU && u.Memory <= n.Memory && u.Disk <= n.Disk
}

// PackResult is the outcome of packing the unplaced allocations.
type PackResult struct {
	// Nodes is the number of nodes of the group the demand needs.
	Nodes int
	// TooLarge are the demands of which one allocation does not fit an empty
	// node of the group: the group cannot take this work.
	TooLarge []Unplaced
}

// PackNodes packs the unplaced allocations into virtual nodes of size node
// and returns the number of nodes used.
//
// The allocations that need a node of their own are put first, one on each
// new node. The others are sorted largest first (by the share of the node
// they take in the dimension where they take the most) and each goes into the
// first virtual node where it fits on every dimension, also into the room
// that is left on the nodes of the exclusive ones; a new node is opened when
// none has room. Four allocations of 6 GB on nodes of 8 GB need four nodes,
// not three.
func PackNodes(demands []Unplaced, node NodeSize) PackResult {
	var res PackResult
	var free []NodeSize // the room left on each virtual node

	var rest []Unplaced
	for _, d := range demands {
		if d.Count <= 0 {
			continue
		}
		if !d.fits(node) {
			res.TooLarge = append(res.TooLarge, d)
			continue
		}
		if !d.Exclusive {
			rest = append(rest, d)
			continue
		}
		for range d.Count {
			free = append(free, NodeSize{node.CPU - d.CPU, node.Memory - d.Memory, node.Disk - d.Disk})
		}
	}

	share := func(d Unplaced) float64 {
		s := 0.0
		for _, r := range [][2]float64{{d.CPU, node.CPU}, {d.Memory, node.Memory}, {d.Disk, node.Disk}} {
			if r[1] > 0 {
				s = max(s, r[0]/r[1])
			}
		}
		return s
	}
	slices.SortStableFunc(rest, func(a, b Unplaced) int {
		sa, sb := share(a), share(b)
		switch {
		case sa > sb:
			return -1
		case sa < sb:
			return 1
		}
		return 0
	})

	for _, d := range rest {
		for range d.Count {
			placed := false
			for i := range free {
				f := &free[i]
				if d.CPU <= f.CPU && d.Memory <= f.Memory && d.Disk <= f.Disk {
					f.CPU, f.Memory, f.Disk = f.CPU-d.CPU, f.Memory-d.Memory, f.Disk-d.Disk
					placed = true
					break
				}
			}
			if !placed {
				free = append(free, NodeSize{node.CPU - d.CPU, node.Memory - d.Memory, node.Disk - d.Disk})
			}
		}
	}
	res.Nodes = len(free)
	return res
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
func (u Unplaced) Restrict(g *AdviceGroup) (Unplaced, bool) {
	if u.Unverifiable != "" {
		return u, false
	}
	for _, c := range u.Classes {
		if !c.matches(g.Class) {
			return u, false
		}
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
