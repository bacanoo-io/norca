// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"net/http"
	"strings"
)

// The check-and-set of the writes of namespaces, quotas, ACL policies, ACL
// roles and network policies: the ExpectedModifyIndex of their upsert
// requests maps the key of a resource of the request (its name; for a
// network policy NetworkPolicyIndexKey) to the modify index the writer read
// (of an ACL role: of the stored role of the ID in the request), 0 for a
// resource that must not exist yet. A resource without an entry is written
// unchecked. The check runs at the endpoint and again in the FSM, in the
// transaction of the write; if one resource fails it, nothing of the request
// is written and the answer is a 409 Conflict (IsIndexConflictError).

// errIndexConflictPrefix starts the message of a refused check-and-set
// write of a namespace, quota, ACL policy or network policy.
const errIndexConflictPrefix = "check-and-set conflict"

// IsIndexConflictError reports whether err is the conflict of a refused
// check-and-set write of a namespace, quota, ACL policy or network policy.
func IsIndexConflictError(err error) bool {
	code, msg, ok := CodeFromRPCCodedErr(err)
	return ok && code == http.StatusConflict && strings.HasPrefix(msg, errIndexConflictPrefix)
}

// CheckModifyIndex checks the write of one resource against expected (see
// above). kind and key name the resource, as in `namespace "web"`; exists
// and have say whether it is stored and with which modify index.
func CheckModifyIndex(expected map[string]uint64, kind, key string, exists bool, have uint64) error {
	want, checked := expected[key]
	switch {
	case !checked:
		return nil
	case want == 0 && exists:
		return NewErrRPCCodedf(http.StatusConflict, "%s: %s %q exists (modify index %d), expected it not to exist",
			errIndexConflictPrefix, kind, key, have)
	case want != 0 && !exists:
		return NewErrRPCCodedf(http.StatusConflict, "%s: %s %q does not exist, expected modify index %d",
			errIndexConflictPrefix, kind, key, want)
	case want != 0 && have != want:
		return NewErrRPCCodedf(http.StatusConflict, "%s: %s %q has modify index %d, expected %d",
			errIndexConflictPrefix, kind, key, have, want)
	}
	return nil
}

// NetworkPolicyIndexKey is the key of a network policy in the
// ExpectedModifyIndex of a NetworkPolicyUpsertRequest: its namespace and its
// name joined by "/", which neither can contain.
func NetworkPolicyIndexKey(namespace, name string) string {
	return namespace + "/" + name
}
