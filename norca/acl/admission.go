// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package acl

// AdmissionPolicy is the ACL rule for dynamic admission control objects:
//
//	admission {
//	  policy = "read" # or "write", "deny"
//	}
//
// It is an operator-level rule that is not scoped to a namespace.
type AdmissionPolicy struct {
	Policy string
}

func (p *AdmissionPolicy) isValid() bool {
	switch p.Policy {
	case PolicyDeny, PolicyRead, PolicyWrite:
		return true
	default:
		return false
	}
}

// AllowAdmissionRead checks if read operations are allowed for admission
// control objects.
func (a *ACL) AllowAdmissionRead() bool {
	switch {
	// ACL is nil only if ACLs are disabled
	case a == nil:
		return true
	case a.management:
		return true
	case a.admission == PolicyWrite:
		return true
	case a.admission == PolicyRead:
		return true
	default:
		return false
	}
}

// AllowAdmissionWrite checks if write operations are allowed for admission
// control objects.
func (a *ACL) AllowAdmissionWrite() bool {
	switch {
	// ACL is nil only if ACLs are disabled
	case a == nil:
		return true
	case a.management:
		return true
	case a.admission == PolicyWrite:
		return true
	default:
		return false
	}
}
