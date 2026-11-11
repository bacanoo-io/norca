// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package stream

import (
	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

func init() {
	// The condition changes of the CSI volumes are visible to everyone who
	// may read the volumes of the namespace.
	topicPolicies[structs.TopicCSIVolume] = topicPolicy{
		allowSubscribe: func(aclObj *acl.ACL, namespace string, _ []string) bool {
			return allowCSIVolumeRead(aclObj, namespace)
		},
		allowEvent: func(aclObj *acl.ACL, event *structs.Event) bool {
			return allowCSIVolumeRead(aclObj, event.Namespace)
		},
	}
}

// allowCSIVolumeRead reports whether the ACL object may read the CSI volumes
// of the namespace.
func allowCSIVolumeRead(aclObj *acl.ACL, namespace string) bool {
	if aclObj == nil {
		return true
	}
	return aclObj.AllowNsOp(namespace, acl.NamespaceCapabilityCSIReadVolume)
}
