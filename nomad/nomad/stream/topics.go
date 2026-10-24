// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package stream

import (
	"strings"

	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/ryanuber/go-glob"
)

// topicPolicy describes how subscriptions to a topic are authorized and
// filtered. Topics without an entry in topicPolicies keep the built-in
// behavior of aclAllowsSubscription and exact key matching.
//
// New topics whose objects are protected by fine-grained ACLs (for example
// path based rules) should register here rather than requiring a management
// token.
type topicPolicy struct {
	// allowSubscribe reports whether the ACL object may subscribe to the
	// topic in the namespace with the given keys. It is evaluated when the
	// subscription is created and every time the token, its policies or
	// roles change.
	allowSubscribe func(aclObj *acl.ACL, namespace string, keys []string) bool

	// allowEvent, if set, is evaluated for every event of the topic before
	// it is delivered to a subscription created with an ACL check. It must
	// be used whenever the subscription time check cannot prove access to
	// every object the subscription keys can match (e.g. deny rules on a
	// sub-path).
	allowEvent func(aclObj *acl.ACL, event *structs.Event) bool

	// globKeys makes subscription keys containing "*" match event keys as
	// glob patterns, using the same glob semantics as ACL path rules.
	globKeys bool
}

var topicPolicies = map[structs.Topic]topicPolicy{
	structs.TopicVariables: {
		allowSubscribe: allowVariablesSubscription,
		allowEvent:     allowVariableEvent,
		globKeys:       true,
	},
}

// allowVariablesSubscription requires the Variables "list" capability for
// each requested path pattern in the requested namespace. The pattern is
// checked as a literal path against the token's path rules, which grants it
// only when a rule glob covers the whole pattern.
func allowVariablesSubscription(aclObj *acl.ACL, namespace string, keys []string) bool {
	if aclObj == nil {
		return true
	}
	for _, key := range keys {
		if !aclObj.AllowVariableOperation(namespace, key, acl.VariablesCapabilityList, nil) {
			return false
		}
	}
	return true
}

// allowVariableEvent requires the Variables "list" capability on the exact
// namespace and path of the changed variable, so the most specific rule
// (including deny rules) applies, as it does for the Variables list API.
func allowVariableEvent(aclObj *acl.ACL, event *structs.Event) bool {
	if aclObj == nil {
		return true
	}
	return aclObj.AllowVariableOperation(event.Namespace, event.Key, acl.VariablesCapabilityList, nil)
}

// topicKeyMatches reports whether a subscription key matches an event key
// for topics that support glob keys.
func topicKeyMatches(topic structs.Topic, key, eventKey string) bool {
	policy, ok := topicPolicies[topic]
	if !ok || !policy.globKeys || !strings.Contains(key, "*") {
		return false
	}
	return glob.Glob(key, eventKey)
}

// authorizeEvents drops the events the ACL object is not allowed to see. The
// input slice is never modified. A nil ACL object means ACLs are not enforced
// for the subscription.
func authorizeEvents(aclObj *acl.ACL, events []structs.Event) []structs.Event {
	if aclObj == nil || aclObj.IsManagement() {
		return events
	}

	var result []structs.Event
	for i := range events {
		policy, ok := topicPolicies[events[i].Topic]
		allowed := !ok || policy.allowEvent == nil || policy.allowEvent(aclObj, &events[i])
		switch {
		case allowed && result != nil:
			result = append(result, events[i])
		case !allowed && result == nil:
			// First dropped event: copy the allowed events seen so far.
			result = make([]structs.Event, i, len(events))
			copy(result, events[:i])
		}
	}
	if result == nil {
		return events
	}
	return result
}
