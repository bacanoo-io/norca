// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package acl

import (
	"fmt"
	"slices"
)

// CDCPolicy is the ACL rule for change data capture (Norca): what a reader
// may receive from the change streams of the system bus (the events of the
// namespaces, the admin events, the Consul KV notices), live and through
// the consumer the servers keep for it:
//
//	cdc {
//	  receive = ["norca.eu.games.Job", "norca.eu.games.Allocation"]
//	  leaf    = false
//	}
//
// The subjects may use the NATS wildcards "*" (one token) and ">" (the
// rest). Leaf allows a NATS server to attach to the bus as a leaf node with
// the token: the way the data leaves the cluster. Without it the token reads
// as a direct client (with an ACL token, or a task with its workload
// identity). The admin topics still need the right the event stream API asks
// for them, whatever the subjects say.
type CDCPolicy struct {
	Receive []string `hcl:"receive"`
	Leaf    bool     `hcl:"leaf"`

	// ExtraKeys are the keys of the block the decoder did not know; HCL v1
	// would ignore them silently.
	ExtraKeys []string `hcl:",unusedKeys" json:"-"`
}

func (p *CDCPolicy) validate() error {
	if err := unknownRuleKeys("cdc", p.ExtraKeys); err != nil {
		return err
	}
	if len(p.Receive) == 0 {
		return fmt.Errorf("a cdc rule names the subjects it may receive (receive)")
	}
	for _, subject := range p.Receive {
		if err := validateBusSubject(subject); err != nil {
			return err
		}
	}
	return nil
}

// ValidCDCSubject checks a subject of a cdc rule or of the filter of a
// consumer: NATS wildcards as whole tokens, nothing of the NATS system or of
// the inboxes, no leading wildcard.
func ValidCDCSubject(subject string) error {
	return validateBusSubject(subject)
}

// unknownRuleKeys refuses the keys of a rule block the decoder did not know.
// The leafnode key of the bus rule is gone: a leaf node attaches with the cdc
// rule.
func unknownRuleKeys(rule string, keys []string) error {
	for _, key := range keys {
		if rule == "bus" && key == "leafnode" {
			return fmt.Errorf("the leafnode key of the bus rule is replaced by the cdc rule: cdc { receive = [...] leaf = true }")
		}
		return fmt.Errorf("unknown key %q in the %s rule", key, rule)
	}
	return nil
}

// CDCGrant is what an ACL may receive through change data capture.
type CDCGrant struct {
	// All receives every subject and may attach a leaf node (management, or
	// ACLs disabled).
	All bool

	// Receive are the sorted subjects otherwise.
	Receive []string

	// Leaf allows attaching a NATS server as a leaf node.
	Leaf bool
}

// Empty reports whether the grant allows nothing.
func (g *CDCGrant) Empty() bool {
	return !g.All && len(g.Receive) == 0
}

// CDCGrant returns what the ACL may receive through change data capture.
func (a *ACL) CDCGrant() *CDCGrant {
	switch {
	// ACL is nil only if ACLs are disabled
	case a == nil, a.management:
		return &CDCGrant{All: true, Leaf: true}
	}
	return &CDCGrant{
		Receive: slices.Clone(a.cdcReceive),
		Leaf:    a.cdcLeaf,
	}
}

// mergeCDCPolicy adds a policy's cdc rule to the ACL; finalizeCDC sorts and
// deduplicates the subjects once every policy was added.
func (a *ACL) mergeCDCPolicy(p *CDCPolicy) {
	a.cdcReceive = append(a.cdcReceive, p.Receive...)
	a.cdcLeaf = a.cdcLeaf || p.Leaf
}

func (a *ACL) finalizeCDC() {
	slices.Sort(a.cdcReceive)
	a.cdcReceive = slices.Compact(a.cdcReceive)
}
