// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-msgpack/codec"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/stream"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
)

// testEventStream opens an event stream with the streaming RPC handler of the
// server and returns the messages received.
func testEventStream(t *testing.T, s *Server, token string, topics map[structs.Topic][]string) <-chan *structs.EventStreamWrapper {
	t.Helper()

	handler, err := s.StreamingRpcHandler("Event.Stream")
	must.NoError(t, err)

	p1, p2 := net.Pipe()
	t.Cleanup(func() {
		p1.Close()
		p2.Close()
	})
	go handler(p2)

	msgs := make(chan *structs.EventStreamWrapper, 16)
	go func() {
		defer close(msgs)
		decoder := codec.NewDecoder(p1, structs.MsgpackHandle)
		for {
			var msg structs.EventStreamWrapper
			if err := decoder.Decode(&msg); err != nil {
				return
			}
			msgs <- &msg
		}
	}()

	encoder := codec.NewEncoder(p1, structs.MsgpackHandle)
	must.NoError(t, encoder.Encode(structs.EventStreamRequest{
		Topics: topics,
		QueryOptions: structs.QueryOptions{
			Region:    s.Region(),
			Namespace: structs.DefaultNamespace,
			AuthToken: token,
		},
	}))
	return msgs
}

// nextEventMsg returns the next message of the stream that carries events or
// an error.
func nextEventMsg(t *testing.T, msgs <-chan *structs.EventStreamWrapper) *structs.EventStreamWrapper {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case msg, ok := <-msgs:
			if !ok {
				t.Fatal("event stream closed")
			}
			if msg.Error == nil && msg.Event != nil && string(msg.Event.Data) == "{}" {
				continue // heartbeat
			}
			return msg
		case <-timeout:
			t.Fatal("timeout waiting for the event stream")
		}
	}
}

// TestEventStream_WorkloadIdentity asserts that a task can subscribe to the
// event stream with its workload identity, authorized by the ACL policies
// bound to its job, and that the subscription is checked again when policies
// change and closed when the allocation is terminal.
func TestEventStream_WorkloadIdentity(t *testing.T) {
	ci.Parallel(t)

	s, _, cleanup := TestACLServer(t, nil)
	defer cleanup()
	testutil.WaitForLeader(t, s.RPC)

	store := s.fsm.State()
	alloc := mock.Alloc()
	alloc.ClientStatus = structs.AllocClientStatusRunning
	must.NoError(t, store.UpsertAllocs(structs.MsgTypeTestSetup, 1000, []*structs.Allocation{alloc}))

	idToken, _, err := s.encrypter.SignClaims(alloc.ToTaskIdentityClaims(nil, "web"))
	must.NoError(t, err)

	bindPolicy := func(index uint64, rules string) {
		policy := mock.ACLPolicy()
		policy.Name = "job-policy"
		policy.Rules = rules
		policy.JobACL = &structs.JobACL{Namespace: alloc.Namespace, JobID: alloc.JobID}
		policy.SetHash()
		must.NoError(t, store.UpsertACLPolicies(structs.ACLPolicyUpsertRequestType, index, []*structs.ACLPolicy{policy}))
	}
	bindPolicy(1001, `namespace "default" {
  capabilities = ["read-job"]
  variables {
    path "app/*" { capabilities = ["list"] }
  }
}`)

	publisher, err := s.State().EventBroker()
	must.NoError(t, err)
	publishVariable := func(index uint64, path string) {
		publisher.Publish(&structs.Events{Index: index, Events: []structs.Event{{
			Topic:     structs.TopicVariables,
			Type:      structs.TypeVariableUpserted,
			Key:       path,
			Namespace: structs.DefaultNamespace,
			Index:     index,
			Payload:   &structs.VariableEvent{Variable: &structs.VariableMetadata{Namespace: structs.DefaultNamespace, Path: path}},
		}}})
	}

	t.Run("denied topic", func(t *testing.T) {
		msgs := testEventStream(t, s, idToken, map[structs.Topic][]string{structs.TopicNode: {"*"}})
		msg := nextEventMsg(t, msgs)
		must.NotNil(t, msg.Error)
		must.NotNil(t, msg.Error.Code)
		must.Eq(t, http.StatusForbidden, *msg.Error.Code)
	})

	t.Run("denied variables path", func(t *testing.T) {
		msgs := testEventStream(t, s, idToken, map[structs.Topic][]string{structs.TopicVariables: {"other/*"}})
		msg := nextEventMsg(t, msgs)
		must.NotNil(t, msg.Error)
		must.NotNil(t, msg.Error.Code)
		must.Eq(t, http.StatusForbidden, *msg.Error.Code)
	})

	t.Run("token of another server", func(t *testing.T) {
		other, _, cleanupOther := TestACLServer(t, nil)
		defer cleanupOther()
		testutil.WaitForLeader(t, other.RPC)
		token, _, err := other.encrypter.SignClaims(alloc.ToTaskIdentityClaims(nil, "web"))
		must.NoError(t, err)

		msgs := testEventStream(t, s, token, map[structs.Topic][]string{structs.TopicVariables: {"app/*"}})
		msg := nextEventMsg(t, msgs)
		must.NotNil(t, msg.Error)
		must.Eq(t, http.StatusForbidden, *msg.Error.Code)
	})

	// The allowed subscription stays open across a policy change that keeps
	// its permissions, is closed when the bound policy no longer allows it,
	// and a new subscription is closed once the allocation is terminal.
	msgs := testEventStream(t, s, idToken, map[structs.Topic][]string{
		structs.TopicVariables: {"app/*"},
		structs.TopicJob:       {"*"},
	})
	publishVariable(2000, "app/one")
	msg := nextEventMsg(t, msgs)
	must.Nil(t, msg.Error)
	must.StrContains(t, string(msg.Event.Data), `"Key":"app/one"`)

	bindPolicy(2001, `namespace "default" {
  capabilities = ["read-job", "list-jobs"]
  variables {
    path "app/*" { capabilities = ["list"] }
  }
}`)
	publishVariable(2002, "app/two")
	msg = nextEventMsg(t, msgs)
	must.Nil(t, msg.Error)
	must.StrContains(t, string(msg.Event.Data), `"Key":"app/two"`)

	bindPolicy(2003, `namespace "default" {
  variables {
    path "app/*" { capabilities = ["list"] }
  }
}`)
	msg = nextEventMsg(t, msgs)
	must.NotNil(t, msg.Error)
	must.StrContains(t, msg.Error.Error(), stream.ErrSubscriptionClosed.Error())

	msgs = testEventStream(t, s, idToken, map[structs.Topic][]string{structs.TopicVariables: {"app/*"}})
	publishVariable(2004, "app/three")
	msg = nextEventMsg(t, msgs)
	must.Nil(t, msg.Error)
	must.True(t, strings.Contains(string(msg.Event.Data), `"Key":"app/`))

	stopped := alloc.Copy()
	stopped.ClientStatus = structs.AllocClientStatusComplete
	must.NoError(t, store.UpdateAllocsFromClient(structs.MsgTypeTestSetup, 2005, []*structs.Allocation{stopped}))
	publishVariable(2006, "app/four")
	for {
		msg = nextEventMsg(t, msgs)
		if msg.Error != nil {
			break
		}
		// Events published before the allocation stopped may still arrive.
		must.False(t, strings.Contains(string(msg.Event.Data), `"Key":"app/four"`))
	}
	must.StrContains(t, msg.Error.Error(), structs.ErrTokenExpired.Error())

	// A terminal allocation's identity cannot subscribe anymore.
	msgs = testEventStream(t, s, idToken, map[structs.Topic][]string{structs.TopicVariables: {"app/*"}})
	msg = nextEventMsg(t, msgs)
	must.NotNil(t, msg.Error)
	must.Eq(t, http.StatusForbidden, *msg.Error.Code)

}
