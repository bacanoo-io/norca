// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package stream

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

// testResolver is an ACLResolver whose result can be changed by the test.
type testResolver struct {
	mu     sync.Mutex
	rules  string
	err    error
	expiry *time.Time
	calls  int
}

func (r *testResolver) resolve(token string) (*acl.ACL, *time.Time, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if token != "workload-identity" {
		return nil, nil, structs.ErrTokenNotFound
	}
	if r.err != nil {
		return nil, nil, r.err
	}
	policy, err := acl.Parse(r.rules)
	if err != nil {
		return nil, nil, err
	}
	aclObj, err := acl.NewACL(false, []*acl.Policy{policy})
	return aclObj, r.expiry, err
}

func (r *testResolver) set(rules string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rules, r.err = rules, err
}

func (r *testResolver) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// TestEventBroker_ACLResolver asserts that a subscription with an ACL
// resolver is authorized with it, without an ACL token in the state, and is
// checked with it again when ACL tokens or policies change.
func TestEventBroker_ACLResolver(t *testing.T) {
	ci.Parallel(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Without a delegate, only the resolver can authorize subscriptions.
	broker, err := NewEventBroker(ctx, nil, EventBrokerCfg{})
	must.NoError(t, err)

	const readJob = `namespace "default" { capabilities = ["read-job"] }`
	expiry := time.Now().Add(time.Hour)
	resolver := &testResolver{rules: readJob, expiry: &expiry}

	subscribe := func(token string) (*Subscription, *time.Time, error) {
		return broker.SubscribeWithACLCheck(&SubscribeRequest{
			Token:      token,
			Namespace:  structs.DefaultNamespace,
			Topics:     map[structs.Topic][]string{structs.TopicJob: {"*"}},
			ResolveACL: resolver.resolve,
		})
	}

	_, _, err = subscribe("unknown")
	must.ErrorIs(t, err, structs.ErrPermissionDenied)

	sub, gotExpiry, err := subscribe("workload-identity")
	must.NoError(t, err)
	must.Eq(t, &expiry, gotExpiry)
	defer sub.Unsubscribe()

	jobEvent := structs.Event{Topic: structs.TopicJob, Namespace: structs.DefaultNamespace, Key: "job"}
	next := func() error {
		ctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		_, err := sub.Next(ctx)
		return err
	}
	broker.Publish(&structs.Events{Index: 1, Events: []structs.Event{jobEvent}})
	must.NoError(t, next())

	// A policy change that keeps the permission keeps the subscription.
	calls := resolver.callCount()
	resolver.set(readJob+"\n"+`namespace "other" { capabilities = ["read-job"] }`, nil)
	broker.Publish(&structs.Events{Index: 2, Events: []structs.Event{{
		Topic: structs.TopicACLPolicy, Type: structs.TypeACLPolicyUpserted,
		Payload: &structs.ACLPolicyEvent{ACLPolicy: &structs.ACLPolicy{Name: "p"}},
	}}})
	deadline := time.Now().Add(5 * time.Second)
	for resolver.callCount() == calls {
		must.True(t, time.Now().Before(deadline), must.Sprint("the subscription was not checked again"))
		time.Sleep(10 * time.Millisecond)
	}
	broker.Publish(&structs.Events{Index: 3, Events: []structs.Event{jobEvent}})
	must.NoError(t, next())

	// A policy change that removes it closes the subscription.
	resolver.set(`namespace "default" { capabilities = ["list-jobs"] }`, nil)
	broker.Publish(&structs.Events{Index: 4, Events: []structs.Event{{
		Topic: structs.TopicACLPolicy, Type: structs.TypeACLPolicyUpserted,
		Payload: &structs.ACLPolicyEvent{ACLPolicy: &structs.ACLPolicy{Name: "p"}},
	}}})
	broker.Publish(&structs.Events{Index: 5, Events: []structs.Event{jobEvent}})
	for {
		err := next()
		if err == nil {
			continue
		}
		must.True(t, errors.Is(err, ErrSubscriptionClosed), must.Sprintf("%v", err))
		break
	}
}
