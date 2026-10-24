// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package stream

import (
	"strings"

	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/nomad/structs"
)

func init() {
	// Custom objects are authorized per namespace and kind with the object
	// ACL rules. Subscription keys are "group/Kind" references (globs
	// allowed, e.g. "db.example.com/*") or "group/Kind/name"; every event
	// is checked against the rules of its own namespace and kind.
	topicPolicies[structs.TopicCustomObject] = topicPolicy{
		allowSubscribe: allowObjectsSubscription,
		allowEvent:     allowObjectEvent,
		globKeys:       true,
	}
}

// allowObjectsSubscription requires the object "read" capability for the
// kind of each subscription key in the requested namespace (in any
// namespace for "*"). The kind is checked literally against the rules, so a
// glob key is granted only if a rule covers it. The key "*" only requires
// read access to some objects; its events are filtered per event.
func allowObjectsSubscription(aclObj *acl.ACL, namespace string, keys []string) bool {
	if aclObj == nil || aclObj.IsManagement() {
		return true
	}
	for _, key := range keys {
		kindRef := objectKeyKind(key)
		if kindRef == string(structs.TopicAll) {
			if !aclObj.AllowAnyObjectOperation(acl.ObjectCapabilityRead) {
				return false
			}
			continue
		}
		if namespace == structs.AllNamespacesSentinel {
			if !aclObj.AllowObjectOperationInAnyNamespace(kindRef, acl.ObjectCapabilityRead) {
				return false
			}
			continue
		}
		if !aclObj.AllowObjectOperation(namespace, kindRef, acl.ObjectCapabilityRead) &&
			!aclObj.AllowObjectOperation(acl.ObjectClusterNamespace, kindRef, acl.ObjectCapabilityRead) {
			return false
		}
	}
	return true
}

// allowObjectEvent requires the object "read" capability on the namespace
// and kind of an object event, and read access to the kind for kind
// events.
func allowObjectEvent(aclObj *acl.ACL, event *structs.Event) bool {
	if aclObj == nil {
		return true
	}
	if len(event.FilterKeys) == 0 {
		return false
	}
	kindRef := event.FilterKeys[0]
	switch event.Type {
	case structs.TypeObjectKindUpserted, structs.TypeObjectKindDeleted:
		return aclObj.AllowObjectKindRead(kindRef)
	}
	namespace := event.Namespace
	if namespace == "" {
		namespace = acl.ObjectClusterNamespace
	}
	return aclObj.AllowObjectOperation(namespace, kindRef, acl.ObjectCapabilityRead)
}

// objectKeyKind returns the "group/Kind" part of a subscription key.
func objectKeyKind(key string) string {
	parts := strings.SplitN(key, "/", 3)
	if len(parts) < 2 {
		return key
	}
	return parts[0] + "/" + parts[1]
}
