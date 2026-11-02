// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package acl

import (
	"fmt"
	"slices"
	"strings"
)

// BusPolicy is the ACL rule for the system bus of the servers (embedded
// NATS):
//
//	bus {
//	  subscribe = ["nomad.*.>"]
//	  publish   = ["nomad.packs.pushed", "nomad.packs.pushed.>"]
//	  leafnode  = false
//	}
//
// The lists hold NATS subjects with the wildcards "*" (one token) and ">"
// (the rest). A connection with the token may publish and subscribe to the
// union of the lists of its policies. It is an operator-level rule that is
// not scoped to a namespace; management tokens may use every subject.
type BusPolicy struct {
	Subscribe []string `hcl:"subscribe"`
	Publish   []string `hcl:"publish"`

	// Leafnode allows connecting a NATS server to the bus as a leaf node,
	// limited to the subjects of the lists.
	Leafnode bool `hcl:"leafnode"`
}

func (p *BusPolicy) validate() error {
	for _, list := range [][]string{p.Subscribe, p.Publish} {
		for _, subject := range list {
			if err := validateBusSubject(subject); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateBusSubject checks a NATS subject with wildcards. Subjects of the
// NATS system ($SYS) are never granted.
func validateBusSubject(subject string) error {
	if subject == "" {
		return fmt.Errorf("empty bus subject")
	}
	if strings.ContainsAny(subject, " \t\r\n") {
		return fmt.Errorf("bus subject %q contains white space", subject)
	}
	if subject == "$SYS" || strings.HasPrefix(subject, "$SYS.") {
		return fmt.Errorf("bus subject %q: the $SYS subjects can't be granted", subject)
	}
	tokens := strings.Split(subject, ".")
	for i, tok := range tokens {
		switch {
		case tok == "":
			return fmt.Errorf("bus subject %q has an empty token", subject)
		case tok == ">" && i != len(tokens)-1:
			return fmt.Errorf("bus subject %q: \">\" must be the last token", subject)
		case tok != ">" && tok != "*" && strings.ContainsAny(tok, "*>"):
			return fmt.Errorf("bus subject %q: wildcards must be whole tokens", subject)
		}
	}
	return nil
}

// BusGrant is what an ACL allows on the system bus.
type BusGrant struct {
	// All allows every subject (management, or ACLs disabled).
	All bool

	// Publish and Subscribe are the sorted subjects otherwise.
	Publish   []string
	Subscribe []string

	// Leafnode allows leaf node connections.
	Leafnode bool
}

// Empty reports whether the grant allows nothing.
func (g *BusGrant) Empty() bool {
	return !g.All && len(g.Publish) == 0 && len(g.Subscribe) == 0
}

// BusGrant returns the subjects the ACL may publish and subscribe to on the
// system bus.
func (a *ACL) BusGrant() *BusGrant {
	switch {
	// ACL is nil only if ACLs are disabled
	case a == nil:
		return &BusGrant{All: true, Leafnode: true}
	case a.management:
		return &BusGrant{All: true, Leafnode: true}
	}
	return &BusGrant{
		Publish:   slices.Clone(a.busPublish),
		Subscribe: slices.Clone(a.busSubscribe),
		Leafnode:  a.busLeafnode,
	}
}

// mergeBusPolicy adds a policy's bus rule to the ACL; finalizeBus sorts and
// deduplicates the lists once every policy was added.
func (a *ACL) mergeBusPolicy(p *BusPolicy) {
	a.busPublish = append(a.busPublish, p.Publish...)
	a.busSubscribe = append(a.busSubscribe, p.Subscribe...)
	a.busLeafnode = a.busLeafnode || p.Leafnode
}

func (a *ACL) finalizeBus() {
	slices.Sort(a.busPublish)
	a.busPublish = slices.Compact(a.busPublish)
	slices.Sort(a.busSubscribe)
	a.busSubscribe = slices.Compact(a.busSubscribe)
}
