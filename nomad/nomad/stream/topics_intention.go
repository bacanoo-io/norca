// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package stream

import (
	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/nomad/structs"
)

func init() {
	// Service intentions are visible to everyone who may read the jobs of
	// their namespace, as through the ServiceIntention.List endpoint.
	topicPolicies[structs.TopicServiceIntention] = topicPolicy{
		allowSubscribe: func(aclObj *acl.ACL, namespace string, _ []string) bool {
			return allowIntentionRead(aclObj, namespace)
		},
		allowEvent: func(aclObj *acl.ACL, event *structs.Event) bool {
			return allowIntentionRead(aclObj, event.Namespace)
		},
	}
}

// allowIntentionRead reports whether the ACL object may read the service
// intentions of the namespace.
func allowIntentionRead(aclObj *acl.ACL, namespace string) bool {
	if aclObj == nil {
		return true
	}
	return aclObj.AllowNsOp(namespace, acl.NamespaceCapabilityReadJob)
}
