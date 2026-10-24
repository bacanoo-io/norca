// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package stream

import (
	"testing"

	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func TestTopicScaling_ACL(t *testing.T) {
	ci.Parallel(t)

	newACL := func(rules string) *acl.ACL {
		policy, err := acl.Parse(rules)
		must.NoError(t, err)
		aclObj, err := acl.NewACL(false, []*acl.Policy{policy})
		must.NoError(t, err)
		return aclObj
	}
	sub := func(ns string) *SubscribeRequest {
		return &SubscribeRequest{
			Namespace: ns,
			Topics:    map[structs.Topic][]string{structs.TopicScaling: {"*"}},
		}
	}
	event := func(ns string) structs.Event {
		return structs.Event{Topic: structs.TopicScaling, Namespace: ns, Key: "job"}
	}

	readJob := newACL(mock.NamespacePolicy("default", "", []string{acl.NamespaceCapabilityReadJob}))
	listPolicies := newACL(mock.NamespacePolicy("default", "", []string{acl.NamespaceCapabilityListScalingPolicies}))
	other := newACL(mock.NamespacePolicy("default", "", []string{acl.NamespaceCapabilitySubmitJob}))

	must.True(t, aclAllowsSubscription(readJob, sub("default")))
	must.True(t, aclAllowsSubscription(listPolicies, sub("default")))
	must.False(t, aclAllowsSubscription(other, sub("default")))
	must.False(t, aclAllowsSubscription(readJob, sub("prod")))
	must.True(t, aclAllowsSubscription(nil, sub("prod")))

	// Events of other namespaces are filtered out per event.
	events := []structs.Event{event("default"), event("prod")}
	must.Eq(t, []structs.Event{event("default")}, authorizeEvents(readJob, events))
	must.Eq(t, []structs.Event{event("default")}, authorizeEvents(listPolicies, events))
}
