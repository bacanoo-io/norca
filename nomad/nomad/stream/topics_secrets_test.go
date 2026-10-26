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

func TestTopics_Secrets(t *testing.T) {
	ci.Parallel(t)

	compile := func(rules string) *acl.ACL {
		p, err := acl.Parse(rules)
		must.NoError(t, err)
		a, err := acl.NewACL(false, []*acl.Policy{p})
		must.NoError(t, err)
		return a
	}
	lister := compile(`namespace "prod" { secret "app/*" { capabilities = ["list-secrets"] } }`)
	operator := compile(`operator { policy = "read" }`)

	sub := func(topic structs.Topic, key string) *SubscribeRequest {
		return &SubscribeRequest{Namespace: "prod", Topics: map[structs.Topic][]string{topic: {key}}}
	}

	// Secret: list-secrets on the subscribed paths.
	must.True(t, aclAllowsSubscription(lister, sub(structs.TopicSecret, "app/*")))
	must.True(t, aclAllowsSubscription(lister, sub(structs.TopicSecret, "app/db")))
	must.False(t, aclAllowsSubscription(lister, sub(structs.TopicSecret, "*")))
	must.False(t, aclAllowsSubscription(operator, sub(structs.TopicSecret, "app/db")))

	events := []structs.Event{
		{Topic: structs.TopicSecret, Namespace: "prod", Key: "app/db"},
		{Topic: structs.TopicSecret, Namespace: "prod", Key: "other"},
	}
	must.Len(t, 1, authorizeEvents(lister, events))
	must.True(t, topicKeyMatches(structs.TopicSecret, "app/*", "app/db"))

	// SecretAccess: operator read.
	must.True(t, aclAllowsSubscription(operator, sub(structs.TopicSecretAccess, "*")))
	must.False(t, aclAllowsSubscription(lister, sub(structs.TopicSecretAccess, "*")))
	must.True(t, aclAllowsSubscription(nil, sub(structs.TopicSecretAccess, "*")))
}
