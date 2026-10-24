// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package stream

import (
	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/nomad/structs"
)

func init() {
	// The decisions of the built-in autoscaler are visible to everyone who
	// may read the job or list its scaling policies.
	topicPolicies[structs.TopicScaling] = topicPolicy{
		allowSubscribe: func(aclObj *acl.ACL, namespace string, _ []string) bool {
			return allowScalingRead(aclObj, namespace)
		},
		allowEvent: func(aclObj *acl.ACL, event *structs.Event) bool {
			return allowScalingRead(aclObj, event.Namespace)
		},
	}
}

// allowScalingRead reports whether the ACL object may see the scaling
// decisions of the namespace.
func allowScalingRead(aclObj *acl.ACL, namespace string) bool {
	if aclObj == nil {
		return true
	}
	return aclObj.AllowNsOp(namespace, acl.NamespaceCapabilityReadJob) ||
		aclObj.AllowNsOp(namespace, acl.NamespaceCapabilityListScalingPolicies)
}
