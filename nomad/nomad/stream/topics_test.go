// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package stream

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

const testVariablesPolicy = `
namespace "default" {
  variables {
    path "crd/*" {
      capabilities = ["list"]
    }
    path "crd/secret/*" {
      capabilities = ["deny"]
    }
    path "other/*" {
      capabilities = ["write"]
    }
  }
}
`

func variableEvent(ns, path string, index uint64) structs.Event {
	return structs.Event{
		Topic:     structs.TopicVariables,
		Type:      structs.TypeVariableUpserted,
		Key:       path,
		Namespace: ns,
		Index:     index,
		Payload: &structs.VariableEvent{Variable: &structs.VariableMetadata{
			Namespace: ns, Path: path, CreateIndex: index, ModifyIndex: index,
		}},
	}
}

func compileTestACL(t *testing.T, rules string) *acl.ACL {
	t.Helper()
	policy, err := acl.Parse(rules)
	must.NoError(t, err)
	aclObj, err := acl.NewACL(false, []*acl.Policy{policy})
	must.NoError(t, err)
	return aclObj
}

func TestFilter_Variables_GlobKeys(t *testing.T) {
	ci.Parallel(t)

	events := []structs.Event{
		variableEvent("default", "crd/example/Widget/a", 1),
		variableEvent("default", "crd/example/Gadget/b", 1),
		variableEvent("default", "app/config", 1),
		variableEvent("other", "crd/example/Widget/c", 1),
	}

	req := &SubscribeRequest{
		Namespace: "default",
		Topics: map[structs.Topic][]string{
			// Both patterns match the first event, which must be delivered
			// only once.
			structs.TopicVariables: {"crd/example/Widget/*", "crd/*/Widget/*", "app/config"},
		},
	}
	got := filter(req, events)
	must.Eq(t, []structs.Event{events[0], events[2]}, got)

	// Glob keys are only enabled for topics that opt in.
	req = &SubscribeRequest{
		Namespace: "default",
		Topics:    map[structs.Topic][]string{structs.TopicJob: {"crd/*"}},
	}
	jobEvent := structs.Event{Topic: structs.TopicJob, Key: "crd/x", Namespace: "default"}
	must.Len(t, 0, filter(req, []structs.Event{jobEvent}))

	// A bare topic subscription matches every key.
	req = &SubscribeRequest{
		Namespace: "*",
		Topics:    map[structs.Topic][]string{structs.TopicVariables: {"*"}},
	}
	must.Len(t, 4, filter(req, events))
}

func TestAuthorizeEvents_Variables(t *testing.T) {
	ci.Parallel(t)

	aclObj := compileTestACL(t, testVariablesPolicy)

	jobEvent := structs.Event{Topic: structs.TopicJob, Key: "job", Namespace: "default"}
	events := []structs.Event{
		jobEvent,
		variableEvent("default", "crd/example/Widget/a", 1),
		variableEvent("default", "crd/secret/key", 1),
		variableEvent("default", "other/thing", 1),
		variableEvent("prod", "crd/example/Widget/a", 1),
	}
	orig := make([]structs.Event, len(events))
	copy(orig, events)

	got := authorizeEvents(aclObj, events)
	must.Eq(t, []structs.Event{jobEvent, events[1]}, got)
	must.Eq(t, orig, events, must.Sprint("input slice must not be modified"))

	// No ACL enforcement and management tokens see everything.
	must.Eq(t, events, authorizeEvents(nil, events))
	must.Eq(t, events, authorizeEvents(acl.ManagementACL, events))
}

func TestACLAllowsSubscription_Variables(t *testing.T) {
	ci.Parallel(t)

	aclObj := compileTestACL(t, testVariablesPolicy)

	testCases := []struct {
		name      string
		namespace string
		keys      []string
		allowed   bool
	}{
		{name: "covered pattern", namespace: "default", keys: []string{"crd/example/Widget/*"}, allowed: true},
		{name: "covered path", namespace: "default", keys: []string{"crd/example/Widget/a"}, allowed: true},
		{name: "all paths", namespace: "default", keys: []string{"*"}, allowed: false},
		{name: "write only path", namespace: "default", keys: []string{"other/*"}, allowed: false},
		{name: "denied path", namespace: "default", keys: []string{"crd/secret/*"}, allowed: false},
		{name: "one key not covered", namespace: "default", keys: []string{"crd/a", "app/x"}, allowed: false},
		{name: "other namespace", namespace: "prod", keys: []string{"crd/*"}, allowed: false},
		{name: "all namespaces", namespace: "*", keys: []string{"crd/*"}, allowed: false},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := &SubscribeRequest{
				Namespace: tc.namespace,
				Topics:    map[structs.Topic][]string{structs.TopicVariables: tc.keys},
			}
			must.Eq(t, tc.allowed, aclAllowsSubscription(aclObj, req))
			must.True(t, aclAllowsSubscription(acl.ManagementACL, req))
		})
	}

	// The all-topics wildcard still requires a management token.
	req := &SubscribeRequest{
		Namespace: "default",
		Topics:    map[structs.Topic][]string{structs.TopicAll: {"*"}},
	}
	must.False(t, aclAllowsSubscription(aclObj, req))
}

func TestEventBroker_Variables_ACL(t *testing.T) {
	ci.Parallel(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	token := &structs.ACLToken{
		AccessorID: uuid.Generate(),
		SecretID:   uuid.Generate(),
		Type:       structs.ACLClientToken,
		Policies:   []string{"crd"},
	}
	policy := &structs.ACLPolicy{Name: "crd", ModifyIndex: 1, Rules: testVariablesPolicy}
	policy.SetHash()
	tokenProvider := &fakeACLTokenProvider{token: token, policy: policy}

	publisher, err := NewEventBroker(ctx, &fakeACLDelegate{tokenProvider: tokenProvider}, EventBrokerCfg{})
	must.NoError(t, err)

	// A pattern not covered by the token is rejected.
	_, _, err = publisher.SubscribeWithACLCheck(&SubscribeRequest{
		Token:     token.SecretID,
		Namespace: "default",
		Topics:    map[structs.Topic][]string{structs.TopicVariables: {"*"}},
	})
	must.ErrorIs(t, err, structs.ErrPermissionDenied)

	sub, _, err := publisher.SubscribeWithACLCheck(&SubscribeRequest{
		Token:     token.SecretID,
		Namespace: "default",
		Topics:    map[structs.Topic][]string{structs.TopicVariables: {"crd/*"}},
	})
	must.NoError(t, err)
	t.Cleanup(sub.Unsubscribe)

	// Events on denied sub-paths are dropped even though the subscription
	// pattern matches them.
	publisher.Publish(&structs.Events{Index: 1, Events: []structs.Event{
		variableEvent("default", "crd/secret/password", 1),
	}})
	publisher.Publish(&structs.Events{Index: 2, Events: []structs.Event{
		variableEvent("default", "crd/example/Widget/a", 2),
		variableEvent("default", "crd/secret/other", 2),
	}})

	got, err := sub.Next(ctx)
	must.NoError(t, err)
	must.Eq(t, 2, got.Index)
	must.Len(t, 1, got.Events)
	must.Eq(t, "crd/example/Widget/a", got.Events[0].Key)

	// A policy update narrowing access is applied to the open subscription
	// without closing it, as long as the subscription is still allowed.
	narrowed := &structs.ACLPolicy{Name: "crd", ModifyIndex: 10, Rules: `
namespace "default" {
  variables {
    path "crd/*" {
      capabilities = ["list"]
    }
    path "crd/example/Widget/*" {
      capabilities = ["write"]
    }
  }
}`}
	narrowed.SetHash()
	tokenProvider.policy = narrowed
	publisher.checkSubscriptionsAgainstACLChange()

	publisher.Publish(&structs.Events{Index: 3, Events: []structs.Event{
		variableEvent("default", "crd/example/Widget/b", 3),
	}})
	publisher.Publish(&structs.Events{Index: 4, Events: []structs.Event{
		variableEvent("default", "crd/example/Gadget/c", 4),
	}})
	got, err = sub.Next(ctx)
	must.NoError(t, err)
	must.Eq(t, 4, got.Index)
	must.Eq(t, "crd/example/Gadget/c", got.Events[0].Key)
}

func TestEventBroker_SubscribeExact(t *testing.T) {
	ci.Parallel(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	newBroker := func(t *testing.T) *EventBroker {
		publisher, err := NewEventBroker(ctx, nil, EventBrokerCfg{EventBufferSize: 3})
		must.NoError(t, err)
		return publisher
	}
	jobEvents := func(index uint64) *structs.Events {
		return &structs.Events{Index: index, Events: []structs.Event{
			{Topic: structs.TopicJob, Key: "job", Namespace: "default", Index: index},
		}}
	}
	varEvents := func(index uint64) *structs.Events {
		return &structs.Events{Index: index, Events: []structs.Event{
			variableEvent("default", "crd/a", index),
		}}
	}
	exactReq := func(index uint64, topic structs.Topic) *SubscribeRequest {
		return &SubscribeRequest{
			Index:               index,
			Namespace:           "default",
			StartExactlyAtIndex: true,
			Topics:              map[structs.Topic][]string{topic: {"*"}},
		}
	}

	t.Run("index required", func(t *testing.T) {
		_, err := newBroker(t).Subscribe(exactReq(0, structs.TopicJob))
		must.ErrorIs(t, err, ErrExactIndexRequired)
	})

	t.Run("dropped index", func(t *testing.T) {
		publisher := newBroker(t)
		// Buffer of size 3: the initial sentinel, 10 and 11 are dropped.
		for _, idx := range []uint64{10, 11, 12, 13, 14, 15} {
			publisher.eventBuf.Append(jobEvents(idx))
		}

		_, err := publisher.Subscribe(exactReq(11, structs.TopicJob))
		must.ErrorIs(t, err, ErrIndexNotAvailable)

		// The default behavior is unchanged: start at the closest index.
		sub, err := publisher.Subscribe(&SubscribeRequest{
			Index:     11,
			Namespace: "default",
			Topics:    map[structs.Topic][]string{structs.TopicJob: {"*"}},
		})
		must.NoError(t, err)
		events, err := sub.NextNoBlock()
		must.NoError(t, err)
		must.Eq(t, 12, events[0].Index)

		// Resuming after the last dropped index delivers everything.
		sub, err = publisher.Subscribe(exactReq(12, structs.TopicJob))
		must.NoError(t, err)
		var got []uint64
		for {
			events, err := sub.NextNoBlock()
			must.NoError(t, err)
			if events == nil {
				break
			}
			got = append(got, events[0].Index)
		}
		must.Eq(t, []uint64{12, 13, 14, 15}, got)

		// Dropped events of other topics do not invalidate an index.
		_, err = publisher.Subscribe(exactReq(5, structs.TopicVariables))
		must.NoError(t, err)

		// Subscribing to all topics considers every dropped event.
		req := exactReq(11, structs.TopicAll)
		_, err = publisher.Subscribe(req)
		must.ErrorIs(t, err, ErrIndexNotAvailable)
	})

	t.Run("index between items", func(t *testing.T) {
		publisher := newBroker(t)
		for _, idx := range []uint64{10, 20, 30} {
			publisher.eventBuf.Append(varEvents(idx))
		}
		sub, err := publisher.Subscribe(exactReq(15, structs.TopicVariables))
		must.NoError(t, err)
		events, err := sub.NextNoBlock()
		must.NoError(t, err)
		must.Eq(t, 20, events[0].Index)
	})

	t.Run("future index", func(t *testing.T) {
		publisher := newBroker(t)
		publisher.eventBuf.Append(varEvents(10))

		sub, err := publisher.Subscribe(exactReq(50, structs.TopicVariables))
		must.NoError(t, err)

		// Events before the requested index are skipped.
		events, err := sub.NextNoBlock()
		must.NoError(t, err)
		must.Nil(t, events)

		publisher.eventBuf.Append(varEvents(40))
		publisher.eventBuf.Append(varEvents(50))
		got, err := sub.Next(ctx)
		must.NoError(t, err)
		must.Eq(t, 50, got.Index)
	})

	t.Run("restored state", func(t *testing.T) {
		publisher := newBroker(t)
		publisher.SetBaseIndexes(100, map[structs.Topic]uint64{structs.TopicVariables: 40})

		// Topics without a known base use the restored index.
		_, err := publisher.Subscribe(exactReq(100, structs.TopicJob))
		must.ErrorIs(t, err, ErrIndexNotAvailable)
		_, err = publisher.Subscribe(exactReq(101, structs.TopicJob))
		must.NoError(t, err)

		// Variables did not change after index 40.
		_, err = publisher.Subscribe(exactReq(40, structs.TopicVariables))
		must.ErrorIs(t, err, ErrIndexNotAvailable)
		_, err = publisher.Subscribe(exactReq(41, structs.TopicVariables))
		must.NoError(t, err)

		_, err = publisher.Subscribe(exactReq(41, structs.TopicAll))
		must.True(t, errors.Is(err, ErrIndexNotAvailable))
	})
}
