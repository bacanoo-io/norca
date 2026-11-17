// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/bus"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/shoenig/test/must"
)

func TestBusAdminEvent(t *testing.T) {
	ci.Parallel(t)

	tests := []struct {
		name     string
		event    structs.Event
		expected bool
	}{
		{name: "job", event: structs.Event{Topic: structs.TopicJob}},
		{name: "allocation", event: structs.Event{Topic: structs.TopicAllocation}},
		{name: "variables", event: structs.Event{Topic: structs.TopicVariables}},
		{name: "secret metadata", event: structs.Event{Topic: structs.TopicSecret}},
		{name: "acl auth method", event: structs.Event{Topic: structs.TopicACLAuthMethod}, expected: true},
		{name: "acl token", event: structs.Event{Topic: structs.TopicACLToken}, expected: true},
		{name: "node", event: structs.Event{Topic: structs.TopicNode}, expected: true},
		{name: "secret access audit", event: structs.Event{Topic: structs.TopicSecretAccess}, expected: true},
		{name: "server", event: structs.Event{Topic: structs.TopicServer}, expected: true},
		{name: "backup", event: structs.Event{Topic: structs.TopicBackup}, expected: true},
		{
			name:     "admin network policy",
			event:    structs.Event{Topic: structs.TopicNetworkPolicy, Payload: &structs.AdminNetworkPolicyEvent{}},
			expected: true,
		},
		{
			name:  "network policy of a namespace",
			event: structs.Event{Topic: structs.TopicNetworkPolicy, Payload: &structs.NetworkPolicyEvent{}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must.Eq(t, tt.expected, busAdminEvent(&tt.event))
		})
	}
}

// subjects collects the subjects a subscription receives.
type subjects struct {
	mu   sync.Mutex
	seen []string
}

func (s *subjects) add(m *nats.Msg) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, m.Subject)
}

func (s *subjects) has(topic string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, subject := range s.seen {
		if strings.HasSuffix(subject, "."+topic) {
			return true
		}
	}
	return false
}

// TestBusServer_AdminEvents checks that the events the event stream API
// shows only with a right beyond reading jobs reach no connection whose
// token lacks that right. Before, the bus published every event into one
// stream on <prefix>.<region>.<namespace>.<topic>: a token whose bus rule
// covered norca.global.> received the ACL policies, the ACL auth methods
// with their OIDC client secret, the audit of the secret store, ...
func TestBusServer_AdminEvents(t *testing.T) {
	ci.Parallel(t)

	s1, root, cleanup := TestACLServer(t, enableBus(t))
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)
	st := s1.fsm.State()

	// A reader of every change on the bus (and of JetStream through its
	// bus rule, which no longer reaches it) without further rights, and a
	// reader with node read.
	reader := mock.ACLPolicy()
	reader.Name = "bus-reader"
	reader.Rules = `cdc {
  receive = ["norca.>"]
}
bus {
  publish = ["$JS.API.>", "$JS.ACK.>"]
}`
	reader.SetHash()
	nodeReader := mock.ACLPolicy()
	nodeReader.Name = "bus-node-reader"
	nodeReader.Rules = `node {
  policy = "read"
}
cdc {
  receive = ["norca.>"]
}`
	nodeReader.SetHash()
	must.NoError(t, st.UpsertACLPolicies(structs.MsgTypeTestSetup, 100, []*structs.ACLPolicy{reader, nodeReader}))
	readerToken, nodeToken := mock.ACLToken(), mock.ACLToken()
	readerToken.Policies, nodeToken.Policies = []string{reader.Name}, []string{nodeReader.Name}
	readerToken.SetHash()
	nodeToken.SetHash()
	must.NoError(t, st.UpsertACLTokens(structs.MsgTypeTestSetup, 101, []*structs.ACLToken{readerToken, nodeToken}))

	connect := func(token *structs.ACLToken) (*nats.Conn, *subjects) {
		nc, err := nats.Connect(busURL(s1), nats.Token(token.SecretID),
			nats.CustomInboxPrefix(bus.InboxPrefix+"."+token.AccessorID))
		must.NoError(t, err)
		t.Cleanup(nc.Close)
		got := &subjects{}
		_, err = nc.Subscribe("norca.>", got.add)
		must.NoError(t, err)
		must.NoError(t, nc.Flush())
		return nc, got
	}
	readerConn, readerGot := connect(readerToken)
	_, nodeGot := connect(nodeToken)
	rootConn, rootGot := connect(root)

	// Events of three rights: an ACL policy (management), a node (node
	// read), a job (read-job; the bus rule decides).
	policy := mock.ACLPolicy()
	must.NoError(t, st.UpsertACLPolicies(structs.ACLPolicyUpsertRequestType, 110, []*structs.ACLPolicy{policy}))
	must.NoError(t, st.UpsertNode(structs.NodeRegisterRequestType, 111, mock.Node()))
	must.NoError(t, st.UpsertJob(structs.JobRegisterRequestType, 112, nil, mock.Job()))

	waitBus(t, func() (bool, error) {
		return rootGot.has("ACLPolicy") && rootGot.has("Node") && rootGot.has("Job") &&
			readerGot.has("Job") && nodeGot.has("Job") && nodeGot.has("Node"), nil
	})
	time.Sleep(500 * time.Millisecond)
	must.False(t, readerGot.has("ACLPolicy"), must.Sprint("the reader received an ACL policy event"))
	must.False(t, readerGot.has("Node"), must.Sprint("the reader without node read received a node event"))
	must.False(t, nodeGot.has("ACLPolicy"), must.Sprint("the node reader received an ACL policy event"))

	// The admin events are in their own stream, not in the event stream.
	rootJS, err := jetstream.New(rootConn)
	must.NoError(t, err)
	adminStream, err := rootJS.Stream(t.Context(), bus.AdminEventStreamName("global"))
	must.NoError(t, err)
	_, err = adminStream.GetLastMsgForSubject(t.Context(), "norca.admin.global._.ACLPolicy")
	must.NoError(t, err)
	eventStream, err := rootJS.Stream(t.Context(), bus.EventStreamName("global"))
	must.NoError(t, err)
	_, err = eventStream.GetLastMsgForSubject(t.Context(), "norca.global._.ACLPolicy")
	must.True(t, errors.Is(err, jetstream.ErrMsgNotFound), must.Sprintf("ACL policy event in the event stream: %v", err))

	// Without a management token the JetStream API of the admin stream and
	// stream creation are refused, whatever the bus rule allows.
	readerJS, err := jetstream.New(readerConn)
	must.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, err = readerJS.Stream(ctx, bus.AdminEventStreamName("global"))
	must.Error(t, err)
	ctx2, cancel2 := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel2()
	_, err = readerJS.CreateStream(ctx2, jetstream.StreamConfig{
		Name:    "COPY",
		Sources: []*jetstream.StreamSource{{Name: bus.AdminEventStreamName("global")}},
	})
	must.Error(t, err)
	// The event stream stays readable, over the connection of the token as
	// the reader: a token with a bus rule has no JetStream in the bus
	// account. The admin stream is refused there too.
	asReader, err := nats.Connect(busURL(s1), nats.UserInfo(bus.ReaderUser, readerToken.SecretID))
	must.NoError(t, err)
	defer asReader.Close()
	asReaderJS, err := jetstream.New(asReader)
	must.NoError(t, err)
	ctx3, cancel3 := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel3()
	_, err = asReaderJS.Stream(ctx3, bus.EventStreamName("global"))
	must.NoError(t, err)
	ctx4, cancel4 := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel4()
	_, err = asReaderJS.Stream(ctx4, bus.AdminEventStreamName("global"))
	must.Error(t, err)
}
