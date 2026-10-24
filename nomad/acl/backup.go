// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package acl

// BackupPolicy is the ACL rule for the backups of a region:
//
//	backup {
//	  policy = "read" # or "write", "deny"
//	}
//
// It is an operator-level rule that is not scoped to a namespace.
type BackupPolicy struct {
	Policy string
}

func (p *BackupPolicy) isValid() bool {
	switch p.Policy {
	case PolicyDeny, PolicyRead, PolicyWrite:
		return true
	default:
		return false
	}
}

// AllowBackupRead checks if the backups may be listed, inspected, browsed
// and compared (without Variables values).
func (a *ACL) AllowBackupRead() bool {
	switch {
	// ACL is nil only if ACLs are disabled
	case a == nil:
		return true
	case a.management:
		return true
	case a.backup == PolicyWrite:
		return true
	case a.backup == PolicyRead:
		return true
	default:
		return false
	}
}

// AllowBackupWrite checks if manual backups, pinning, labeling, deletion
// and download links are allowed.
func (a *ACL) AllowBackupWrite() bool {
	switch {
	// ACL is nil only if ACLs are disabled
	case a == nil:
		return true
	case a.management:
		return true
	case a.backup == PolicyWrite:
		return true
	default:
		return false
	}
}
