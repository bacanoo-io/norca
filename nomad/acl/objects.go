// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package acl

import (
	"fmt"
	"regexp"
	"strings"

	glob "github.com/ryanuber/go-glob"
)

// Custom object rules (operator framework). They are written per namespace
// and per object kind, with globs in both:
//
//	namespace "prod" {
//	  object "db.example.com/PostgresCluster" {
//	    capabilities = ["read", "write"]
//	  }
//	}
//
//	namespace "*" {
//	  object "db.example.com/*" {
//	    capabilities = ["read", "write", "write-status"]
//	  }
//	}
//
// Objects of cluster-scoped kinds are authorized against the rules of the
// namespace "*" block (ObjectClusterNamespace). Managing the kinds
// themselves requires operator write.
const (
	// ObjectCapabilityRead allows reading, listing and watching objects.
	ObjectCapabilityRead = "read"

	// ObjectCapabilityWrite allows creating, updating and deleting objects
	// (spec and metadata).
	ObjectCapabilityWrite = "write"

	// ObjectCapabilityWriteStatus allows writing the status subresource.
	ObjectCapabilityWriteStatus = "write-status"

	// ObjectCapabilityWriteEvents allows writing the object event log.
	ObjectCapabilityWriteEvents = "write-events"

	// ObjectCapabilityManageFinalizers allows adding and removing
	// finalizers.
	ObjectCapabilityManageFinalizers = "manage-finalizers"

	// ObjectCapabilityDeny denies every operation.
	ObjectCapabilityDeny = "deny"

	// ObjectClusterNamespace is the namespace the rules of cluster-scoped
	// objects are looked up in.
	ObjectClusterNamespace = "*"
)

// validObjectKindPattern is a "group/Kind" reference, where both parts may
// contain globs.
var validObjectKindPattern = regexp.MustCompile(`^[a-zA-Z0-9.*-]+/[a-zA-Z0-9*]+$|^\*$`)

// ObjectPolicy is the object block of a namespace policy.
type ObjectPolicy struct {
	Kind         string `hcl:",key"`
	Policy       string
	Capabilities []string
}

func isObjectCapabilityValid(cap string) bool {
	switch cap {
	case ObjectCapabilityRead, ObjectCapabilityWrite, ObjectCapabilityWriteStatus,
		ObjectCapabilityWriteEvents, ObjectCapabilityManageFinalizers, ObjectCapabilityDeny:
		return true
	default:
		return false
	}
}

// expandObjectPolicy provides the capabilities of the short hand policy.
func expandObjectPolicy(policy string) []string {
	switch policy {
	case PolicyDeny:
		return []string{ObjectCapabilityDeny}
	case PolicyRead:
		return []string{ObjectCapabilityRead}
	case PolicyWrite:
		return []string{ObjectCapabilityRead, ObjectCapabilityWrite}
	default:
		return nil
	}
}

// parseObjectPolicies validates the object blocks of a namespace policy and
// expands their short hand policies.
func parseObjectPolicies(ns *NamespacePolicy) error {
	for _, op := range ns.Objects {
		if op.Kind == "" {
			return fmt.Errorf("Invalid missing object kind in namespace %s", ns.Name)
		}
		if !validObjectKindPattern.MatchString(op.Kind) {
			return fmt.Errorf("Invalid object kind %q in namespace %s, must be <group>/<Kind>", op.Kind, ns.Name)
		}
		switch op.Policy {
		case "", PolicyDeny, PolicyRead, PolicyWrite:
		default:
			return fmt.Errorf("Invalid object policy %q for %q in namespace %s", op.Policy, op.Kind, ns.Name)
		}
		for _, cap := range op.Capabilities {
			if !isObjectCapabilityValid(cap) {
				return fmt.Errorf("Invalid object capability '%s' for %q in namespace %s", cap, op.Kind, ns.Name)
			}
		}
		op.Capabilities = append(op.Capabilities, expandObjectPolicy(op.Policy)...)
		if len(op.Capabilities) == 0 {
			return fmt.Errorf("Invalid object rule for %q in namespace %s: no policy or capabilities", op.Kind, ns.Name)
		}
	}
	return nil
}

// objectsKey is the key of an object rule in the ACL radix trees.
func objectsKey(ns, kindRef string) []byte {
	return []byte(ns + "\x00" + kindRef)
}

// matchingObjectsCapabilitySet returns the capabilities of the most
// specific rule that matches the namespace and the "group/Kind" reference.
func (a *ACL) matchingObjectsCapabilitySet(ns, kindRef string) (capabilitySet, bool) {
	if caps, ok := a.objects.Get(objectsKey(ns, kindRef)); ok {
		return caps, true
	}
	return a.findClosestMatchingGlob(a.wildcardObjects, ns+"\x00"+kindRef)
}

// AllowObjectOperation checks if the operation is allowed on objects of the
// kind ("group/Kind") in the namespace. Use ObjectClusterNamespace for
// cluster-scoped objects. The reference may itself be a glob pattern, for
// example for event stream subscriptions; it is then checked literally
// against the rules, which grants it only if a rule covers the pattern.
func (a *ACL) AllowObjectOperation(ns, kindRef, op string) bool {
	switch {
	case a == nil:
		return true
	case a.management:
		return true
	}
	caps, ok := a.matchingObjectsCapabilitySet(ns, kindRef)
	if !ok || caps.Check(ObjectCapabilityDeny) {
		return false
	}
	return caps.Check(op)
}

// AllowObjectOperationInAnyNamespace checks if the operation is allowed on
// objects of the kind in at least one namespace. It is a loose check for
// listing across namespaces; the results must be filtered per namespace.
func (a *ACL) AllowObjectOperationInAnyNamespace(kindRef, op string) bool {
	switch {
	case a == nil:
		return true
	case a.management:
		return true
	}
	allow := false
	walk := func(k []byte, caps capabilitySet) bool {
		_, pattern, ok := strings.Cut(string(k), "\x00")
		if !ok || caps.Check(ObjectCapabilityDeny) || !caps.Check(op) {
			return false
		}
		if pattern == kindRef || glob.Glob(pattern, kindRef) {
			allow = true
		}
		return allow
	}
	a.objects.Root().Walk(walk)
	if !allow {
		a.wildcardObjects.Root().Walk(walk)
	}
	return allow
}

// AllowObjectKindRead checks if the definition of an object kind may be
// read: with operator read, or with any capability on objects of the kind.
func (a *ACL) AllowObjectKindRead(kindRef string) bool {
	switch {
	case a == nil:
		return true
	case a.management:
		return true
	case a.AllowOperatorRead():
		return true
	}
	for _, op := range []string{ObjectCapabilityRead, ObjectCapabilityWrite, ObjectCapabilityWriteStatus,
		ObjectCapabilityWriteEvents, ObjectCapabilityManageFinalizers} {
		if a.AllowObjectOperationInAnyNamespace(kindRef, op) {
			return true
		}
	}
	return false
}

// AllowObjectKindWrite checks if object kinds may be created, updated and
// deleted.
func (a *ACL) AllowObjectKindWrite() bool {
	// ACL is nil only if ACLs are disabled
	return a == nil || a.AllowOperatorWrite()
}

// AllowAnyObjectOperation checks if any object rule grants the operation,
// in any namespace and for any kind. It is a loose check for requests whose
// results are filtered per object.
func (a *ACL) AllowAnyObjectOperation(op string) bool {
	switch {
	case a == nil:
		return true
	case a.management:
		return true
	}
	allow := false
	walk := func(_ []byte, caps capabilitySet) bool {
		allow = !caps.Check(ObjectCapabilityDeny) && caps.Check(op)
		return allow
	}
	a.objects.Root().Walk(walk)
	if !allow {
		a.wildcardObjects.Root().Walk(walk)
	}
	return allow
}
