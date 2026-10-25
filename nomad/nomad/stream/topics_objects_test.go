// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package stream

import (
	"testing"

	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func TestTopicCustomObject_ACL(t *testing.T) {
	ci.Parallel(t)

	newACL := func(rules string) *acl.ACL {
		policy, err := acl.Parse(rules)
		must.NoError(t, err)
		aclObj, err := acl.NewACL(false, []*acl.Policy{policy})
		must.NoError(t, err)
		return aclObj
	}
	sub := func(ns string, keys ...string) *SubscribeRequest {
		return &SubscribeRequest{
			Namespace: ns,
			Topics:    map[structs.Topic][]string{structs.TopicCustomObject: keys},
		}
	}
	objectEvent := func(ns, kindRef, name string) structs.Event {
		return structs.Event{
			Topic:      structs.TopicCustomObject,
			Type:       structs.TypeCustomObjectUpserted,
			Namespace:  ns,
			Key:        kindRef + "/" + name,
			FilterKeys: []string{kindRef},
		}
	}
	kindEvent := func(kindRef string) structs.Event {
		return structs.Event{
			Topic:      structs.TopicCustomObject,
			Type:       structs.TypeObjectKindUpserted,
			Key:        kindRef,
			FilterKeys: []string{kindRef},
		}
	}

	const pg = "db.example.com/PostgresCluster"
	user := newACL(`namespace "default" { object "db.example.com/PostgresCluster" { policy = "read" } }`)
	controller := newACL(`namespace "*" { object "db.example.com/*" { capabilities = ["read", "write-status"] } }`)
	statusOnly := newACL(`namespace "*" { object "db.example.com/*" { capabilities = ["write-status"] } }`)

	must.True(t, aclAllowsSubscription(user, sub("default", pg)))
	must.True(t, aclAllowsSubscription(user, sub("default", pg+"/billing")))
	must.True(t, aclAllowsSubscription(user, sub("default", "*")))
	must.False(t, aclAllowsSubscription(user, sub("prod", pg)))
	must.False(t, aclAllowsSubscription(user, sub("default", "db.example.com/*")))
	must.False(t, aclAllowsSubscription(user, sub("default", "lb.example.com/LoadBalancer")))
	must.True(t, aclAllowsSubscription(user, sub("*", pg)))

	must.True(t, aclAllowsSubscription(controller, sub("*", "db.example.com/*")))
	must.True(t, aclAllowsSubscription(controller, sub("prod", pg)))
	must.False(t, aclAllowsSubscription(controller, sub("*", "lb.example.com/LoadBalancer")))
	must.False(t, aclAllowsSubscription(statusOnly, sub("*", pg)))
	must.False(t, aclAllowsSubscription(statusOnly, sub("default", "*")))
	must.True(t, aclAllowsSubscription(nil, sub("*", "*")))

	// Events are filtered per namespace and kind. Cluster-scoped objects
	// (no namespace) need a rule of namespace "*".
	events := []structs.Event{
		objectEvent("default", pg, "a"),
		objectEvent("prod", pg, "b"),
		objectEvent("default", "db.example.com/Other", "c"),
		objectEvent("", pg, "d"),
		kindEvent(pg),
		kindEvent("lb.example.com/LoadBalancer"),
	}
	must.Eq(t, []structs.Event{events[0], events[4]}, authorizeEvents(user, events))
	must.Eq(t, []structs.Event{events[0], events[1], events[2], events[3], events[4]}, authorizeEvents(controller, events))
	must.Eq(t, events, authorizeEvents(nil, events))

	// Glob keys match object events of all kinds of a group.
	req := sub("*", "db.example.com/*")
	must.Eq(t, []structs.Event{events[0], events[1], events[2], events[3], events[4]}, filter(req, events))
	req = sub("default", pg)
	must.Eq(t, []structs.Event{events[0], events[3], events[4]}, filter(req, events))
	req = sub("*", pg+"/b")
	must.Eq(t, []structs.Event{events[1]}, filter(req, events))
}
