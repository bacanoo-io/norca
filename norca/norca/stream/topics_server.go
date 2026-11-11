// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package stream

import (
	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

func init() {
	// The changes of the servers (standby promotions and demotions) are
	// visible to everyone who may read the Raft configuration.
	topicPolicies[structs.TopicServer] = topicPolicy{
		allowSubscribe: func(aclObj *acl.ACL, _ string, _ []string) bool {
			return aclObj.AllowOperatorRead()
		},
	}
}
