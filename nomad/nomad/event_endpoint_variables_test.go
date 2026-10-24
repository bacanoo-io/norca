// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"encoding/json"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/hashicorp/go-msgpack/codec"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
)

// openTestEventStream sends req to the server's Event.Stream handler and
// returns the decoded responses.
func openTestEventStream(t *testing.T, s *Server, req structs.EventStreamRequest) <-chan *structs.EventStreamWrapper {
	t.Helper()

	handler, err := s.StreamingRpcHandler("Event.Stream")
	must.NoError(t, err)

	p1, p2 := net.Pipe()
	t.Cleanup(func() {
		p1.Close()
		p2.Close()
	})
	go handler(p2)

	msgCh := make(chan *structs.EventStreamWrapper, 16)
	go func() {
		defer close(msgCh)
		decoder := codec.NewDecoder(p1, structs.MsgpackHandle)
		for {
			var msg structs.EventStreamWrapper
			if err := decoder.Decode(&msg); err != nil {
				return
			}
			msgCh <- &msg
		}
	}()

	must.NoError(t, codec.NewEncoder(p1, structs.MsgpackHandle).Encode(req))
	return msgCh
}

// nextTestStreamEvents returns the next error or events from the stream,
// skipping heartbeats.
func nextTestStreamEvents(t *testing.T, msgCh <-chan *structs.EventStreamWrapper) (*structs.RpcError, *structs.Events) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case msg, ok := <-msgCh:
			must.True(t, ok, must.Sprint("stream closed"))
			if msg.Error != nil {
				return msg.Error, nil
			}
			var events structs.Events
			must.NoError(t, json.Unmarshal(msg.Event.Data, &events))
			if len(events.Events) == 0 {
				continue
			}
			return nil, &events
		case <-timeout:
			t.Fatal("timeout waiting for event stream message")
		}
	}
}

func TestEventStream_Exact(t *testing.T) {
	ci.Parallel(t)

	s1, cleanupS1 := TestServer(t, func(c *Config) {
		c.EnableEventBroker = true
		c.EventBufferSize = 5
	})
	defer cleanupS1()
	testutil.WaitForLeader(t, s1.RPC)

	publisher, err := s1.State().EventBroker()
	must.NoError(t, err)

	// Fill the small buffer with events far above any index the test
	// server writes itself, so that early events are dropped.
	node := mock.Node()
	const first, last = 1_000_001, 1_000_010
	for i := uint64(first); i <= last; i++ {
		publisher.Publish(&structs.Events{Index: i, Events: []structs.Event{{Topic: "test", Index: i, Payload: node}}})
	}
	testutil.WaitForResult(func() (bool, error) {
		return publisher.Len() == 5, nil
	}, func(err error) { t.Fatalf("buffer not filled: %v", err) })

	newReq := func(index int, exact bool) structs.EventStreamRequest {
		return structs.EventStreamRequest{
			Topics:              map[structs.Topic][]string{"test": {"*"}},
			Index:               index,
			StartExactlyAtIndex: exact,
			QueryOptions:        structs.QueryOptions{Region: s1.Region()},
		}
	}

	t.Run("dropped index is gone", func(t *testing.T) {
		rpcErr, _ := nextTestStreamEvents(t, openTestEventStream(t, s1, newReq(first, true)))
		must.NotNil(t, rpcErr)
		must.Eq(t, http.StatusGone, *rpcErr.Code)
	})

	t.Run("without exact the closest index is used", func(t *testing.T) {
		rpcErr, events := nextTestStreamEvents(t, openTestEventStream(t, s1, newReq(first, false)))
		must.Nil(t, rpcErr)
		must.Greater(t, first, events.Index)
	})

	t.Run("retained index", func(t *testing.T) {
		rpcErr, events := nextTestStreamEvents(t, openTestEventStream(t, s1, newReq(last, true)))
		must.Nil(t, rpcErr)
		must.Eq(t, last, events.Index)
	})

	t.Run("index required", func(t *testing.T) {
		rpcErr, _ := nextTestStreamEvents(t, openTestEventStream(t, s1, newReq(0, true)))
		must.NotNil(t, rpcErr)
		must.Eq(t, http.StatusBadRequest, *rpcErr.Code)
	})
}

func TestEventStream_Variables_ACL(t *testing.T) {
	ci.Parallel(t)

	s1, _, cleanupS1 := TestACLServer(t, func(c *Config) {
		c.EnableEventBroker = true
	})
	defer cleanupS1()
	testutil.WaitForLeader(t, s1.RPC)

	policy := `
namespace "default" {
  variables {
    path "crd/*" {
      capabilities = ["list"]
    }
    path "crd/secret/*" {
      capabilities = ["deny"]
    }
  }
}`
	token := mock.CreatePolicyAndToken(t, s1.State(), 1000, "crd-list", policy)

	newReq := func(keys ...string) structs.EventStreamRequest {
		return structs.EventStreamRequest{
			Topics: map[structs.Topic][]string{structs.TopicVariables: keys},
			Index:  1,
			QueryOptions: structs.QueryOptions{
				Region:    s1.Region(),
				Namespace: "default",
				AuthToken: token.SecretID,
			},
		}
	}

	// A pattern the token cannot list is rejected.
	rpcErr, _ := nextTestStreamEvents(t, openTestEventStream(t, s1, newReq("*")))
	must.NotNil(t, rpcErr)
	must.StrContains(t, rpcErr.Error(), structs.ErrPermissionDenied.Error())

	msgCh := openTestEventStream(t, s1, newReq("crd/example/Widget/*", "crd/secret/*"))

	// Every pattern must be allowed: a denied one rejects the subscription.
	rpcErr, _ = nextTestStreamEvents(t, msgCh)
	must.NotNil(t, rpcErr)

	msgCh = openTestEventStream(t, s1, newReq("crd/*"))

	setVar := func(index uint64, path string) {
		sv := mock.VariableEncrypted()
		sv.Namespace = "default"
		sv.Path = path
		sv.Data = []byte("ciphertext-" + path)
		resp := s1.State().VarSet(index, &structs.VarApplyStateRequest{Op: structs.VarOpSet, Var: sv})
		must.NoError(t, resp.Error)
	}
	setVar(2000, "crd/secret/password")
	setVar(2001, "app/config")
	setVar(2002, "crd/example/Widget/a")

	rpcErr, events := nextTestStreamEvents(t, msgCh)
	must.Nil(t, rpcErr)
	must.Eq(t, 2002, events.Index)
	must.Len(t, 1, events.Events)
	must.Eq(t, "crd/example/Widget/a", events.Events[0].Key)
	must.Eq(t, structs.TypeVariableUpserted, events.Events[0].Type)

	raw, err := json.Marshal(events.Events[0].Payload)
	must.NoError(t, err)
	must.StrNotContains(t, string(raw), "ciphertext")
	must.StrNotContains(t, string(raw), "KeyID")
}
