// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package stream

import (
	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/nomad/structs"
)

func init() {
	// Backup and restore events are visible to everyone who may read the
	// backups of the region.
	topicPolicies[structs.TopicBackup] = topicPolicy{
		allowSubscribe: func(aclObj *acl.ACL, _ string, _ []string) bool {
			return aclObj.AllowBackupRead()
		},
	}
}
