// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

const (
	// AllocAffinityDefaultScope is the topology domain of a colocate or
	// separate rule that does not set one: the node itself.
	AllocAffinityDefaultScope = "${node.unique.id}"

	// allocAffinityDefaultWeight is the weight of a preferred (not required)
	// rule that does not set one, matching the affinity block default.
	allocAffinityDefaultWeight = 50
)

// AllocAffinity is a cross-job placement rule of a task group, used by the
// colocate (affinity) and separate (anti-affinity) blocks. The selector picks
// allocations by the meta of their job and task group; the scope is a node
// attribute interpolation defining the topology domain.
type AllocAffinity struct {
	Selector  map[string]string `hcl:"selector,block"`
	Namespace string            `hcl:"namespace,optional"`
	Scope     string            `hcl:"scope,optional"`
	Weight    *int8             `hcl:"weight,optional"`
	Required  bool              `hcl:"required,optional"`
}

// NewAllocAffinity returns a preferred rule with the given selector, scope and
// weight.
func NewAllocAffinity(selector map[string]string, scope string, weight int8) *AllocAffinity {
	return &AllocAffinity{
		Selector: selector,
		Scope:    scope,
		Weight:   pointerOf(weight),
	}
}

// Canonicalize sets the defaults of the rule.
func (a *AllocAffinity) Canonicalize() {
	if a.Scope == "" {
		a.Scope = AllocAffinityDefaultScope
	}
	if a.Weight == nil {
		if a.Required {
			a.Weight = pointerOf(int8(0))
		} else {
			a.Weight = pointerOf(int8(allocAffinityDefaultWeight))
		}
	}
}

// AddColocate adds a colocate (cross-job affinity) rule to the task group.
func (g *TaskGroup) AddColocate(a *AllocAffinity) *TaskGroup {
	g.Colocate = append(g.Colocate, a)
	return g
}

// AddSeparate adds a separate (cross-job anti-affinity) rule to the task
// group.
func (g *TaskGroup) AddSeparate(a *AllocAffinity) *TaskGroup {
	g.Separate = append(g.Separate, a)
	return g
}
