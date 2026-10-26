// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package stream

import (
	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/nomad/structs"
)

func init() {
	// Secret events carry metadata and the keyed value hash only; they are
	// visible to everyone who may list the secret's path.
	topicPolicies[structs.TopicSecret] = topicPolicy{
		allowSubscribe: func(aclObj *acl.ACL, namespace string, keys []string) bool {
			if aclObj == nil {
				return true
			}
			for _, key := range keys {
				if !aclObj.AllowSecretOperation(namespace, key, acl.NamespaceCapabilityListSecrets) {
					return false
				}
			}
			return true
		},
		allowEvent: func(aclObj *acl.ACL, event *structs.Event) bool {
			if aclObj == nil {
				return true
			}
			return aclObj.AllowSecretOperation(event.Namespace, event.Key, acl.NamespaceCapabilityListSecrets)
		},
		globKeys: true,
	}

	// The audit records of the secret store require operator read.
	topicPolicies[structs.TopicSecretAccess] = topicPolicy{
		allowSubscribe: func(aclObj *acl.ACL, _ string, _ []string) bool {
			return aclObj == nil || aclObj.AllowOperatorRead()
		},
	}
}
