// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"regexp"
	"strings"
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

func TestCDCConsumerName(t *testing.T) {
	ci.Parallel(t)

	must.Eq(t, "0b8f7d2e-1c4a-4f8e-9a6b-2d3c4e5f6a7b", cdcConsumerName(bus.KindToken, "0b8f7d2e-1c4a-4f8e-9a6b-2d3c4e5f6a7b"))
	must.Eq(t, "a1_web", cdcConsumerName(bus.KindWorkload, "a1/web"))
	must.Eq(t, "a1_web_api", cdcConsumerName(bus.KindWorkload, "a1/web_api"))
	// A task name with characters a consumer name can't hold: replaced, with
	// a digest of the name, so it can't meet the task that has the
	// replacement in its name.
	dotted := cdcConsumerName(bus.KindWorkload, "a1/web.api")
	must.RegexMatch(t, regexp.MustCompile(`^a1_web_api-[0-9a-f]{8}$`), dotted)
	must.Eq(t, dotted, cdcConsumerName(bus.KindWorkload, "a1/web.api"))
	must.NotEq(t, dotted, cdcConsumerName(bus.KindWorkload, "a1/web api"))
	must.NotEq(t, dotted, cdcConsumerName(bus.KindWorkload, "a1/web_api"))

	// The inbox token of a task follows the same rule.
	must.Eq(t, "web_api", busTaskToken("web_api", structs.BusSubjectToken))
	must.Eq(t, "web+api", busTaskToken("web+api", structs.BusSubjectToken))
	inbox := busTaskToken("web.api", structs.BusSubjectToken)
	must.RegexMatch(t, regexp.MustCompile(`^web_api-[0-9a-f]{8}$`), inbox)
	must.NotEq(t, inbox, busTaskToken("web api", structs.BusSubjectToken))
	must.Eq(t, "", cdcConsumerName(bus.KindToken, "anonymous"))
	must.Eq(t, "", cdcConsumerName(bus.KindToken, "leader"))
	must.Eq(t, "", cdcConsumerName(bus.KindNode, "n1"))
}

// TestBusServer_CDCRule checks the cdc rule on the bus: a reader receives
// the change streams by it, live, and nothing else under the prefix (the
// telemetry of the nodes, the pack push notices); a bus rule no longer
// reaches the change streams; and a reader can't make consumers itself.
func TestBusServer_CDCRule(t *testing.T) {
	ci.Parallel(t)

	s1, _, cleanup := TestACLServer(t, enableBus(t))
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)
	st := s1.fsm.State()

	cdcPolicy := mock.ACLPolicy()
	cdcPolicy.Name = "cdc-reader"
	cdcPolicy.Rules = `cdc { receive = ["norca.>"] }`
	cdcPolicy.SetHash()
	busPolicy := mock.ACLPolicy()
	busPolicy.Name = "bus-only"
	busPolicy.Rules = `bus { subscribe = ["norca.>", "app.>"] }`
	busPolicy.SetHash()
	must.NoError(t, st.UpsertACLPolicies(structs.MsgTypeTestSetup, 100, []*structs.ACLPolicy{cdcPolicy, busPolicy}))
	reader, busOnly := mock.ACLToken(), mock.ACLToken()
	reader.Policies, busOnly.Policies = []string{cdcPolicy.Name}, []string{busPolicy.Name}
	reader.SetHash()
	busOnly.SetHash()
	must.NoError(t, st.UpsertACLTokens(structs.MsgTypeTestSetup, 101, []*structs.ACLToken{reader, busOnly}))
	node := mock.Node()
	must.NoError(t, st.UpsertNode(structs.MsgTypeTestSetup, 102, node))

	readerConn, err := nats.Connect(busURL(s1), nats.Token(reader.SecretID),
		nats.CustomInboxPrefix(bus.InboxPrefix+"."+reader.AccessorID))
	must.NoError(t, err)
	defer readerConn.Close()
	got := &subjects{}
	_, err = readerConn.Subscribe("norca.>", got.add)
	must.NoError(t, err)
	must.NoError(t, readerConn.Flush())

	// The bus rule's "norca.>" is left out: the subscription is refused.
	busErrs := make(chan error, 4)
	busConn, err := nats.Connect(busURL(s1), nats.Token(busOnly.SecretID),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) { busErrs <- err }))
	must.NoError(t, err)
	defer busConn.Close()
	_, err = busConn.SubscribeSync("norca.>")
	must.NoError(t, err)
	must.NoError(t, busConn.Flush())
	select {
	case err := <-busErrs:
		must.ErrorContains(t, err, "Permissions Violation")
	case <-time.After(5 * time.Second):
		t.Fatal("the bus rule still reaches the change streams")
	}

	// A change and the telemetry of a node.
	must.NoError(t, st.UpsertJob(structs.JobRegisterRequestType, 110, nil, mock.Job()))
	nodeConn, err := nats.Connect(busURL(s1), nats.UserInfo(node.ID, node.SecretID))
	must.NoError(t, err)
	defer nodeConn.Close()
	must.NoError(t, nodeConn.Publish(structs.BusTelemetrySubject("global", node.ID), []byte("{}")))

	waitBus(t, func() (bool, error) { return got.has("Job"), nil })
	time.Sleep(500 * time.Millisecond)
	got.mu.Lock()
	for _, subject := range got.seen {
		must.False(t, strings.HasPrefix(subject, "norca.telemetry."), must.Sprintf("the reader received %s", subject))
	}
	got.mu.Unlock()

	// The reader reads the streams through the consumer the servers keep for
	// it; it can't make one itself.
	js, err := jetstream.New(readerConn)
	must.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, err = js.Stream(ctx, bus.EventStreamName("global"))
	must.NoError(t, err)
	_, err = js.CreateConsumer(ctx, bus.EventStreamName("global"), jetstream.ConsumerConfig{Durable: reader.AccessorID})
	must.Error(t, err)
}
