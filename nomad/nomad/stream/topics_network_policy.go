// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package stream

import (
	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/nomad/structs"
)

func init() {
	// Network policy events are visible to everyone who may read the
	// network policies of their namespace; admin network policy events
	// require operator read.
	topicPolicies[structs.TopicNetworkPolicy] = topicPolicy{
		allowSubscribe: func(aclObj *acl.ACL, namespace string, _ []string) bool {
			if aclObj == nil {
				return true
			}
			return aclObj.AllowNetworkPolicyRead(namespace) || aclObj.AllowOperatorRead()
		},
		allowEvent: func(aclObj *acl.ACL, event *structs.Event) bool {
			if aclObj == nil {
				return true
			}
			if _, ok := event.Payload.(*structs.AdminNetworkPolicyEvent); ok {
				return aclObj.AllowOperatorRead()
			}
			return aclObj.AllowNetworkPolicyRead(event.Namespace)
		},
	}
}
