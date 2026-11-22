// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	"github.com/hashicorp/go-memdb"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/bus"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
)

// The log of an operation goes with it, also one deleted through the object
// API, and so does the log of a start that never became an operation. The
// log that is being written and that of an operation that is there stay.
func TestRiggerOperations_PurgeLogs(t *testing.T) {
	ci.Parallel(t)

	s, root, cleanup := TestACLServer(t, enableBus(t))
	t.Cleanup(cleanup)
	testutil.WaitForLeader(t, s.RPC)
	waitOperatorKinds(t, s)
	waitBus(t, func() (bool, error) { return s.bus.JetStreamReady(), nil })

	nc, err := s.bus.Connect()
	must.NoError(t, err)
	t.Cleanup(nc.Close)

	js, err := jetstream.New(nc)
	must.NoError(t, err)

	var stream jetstream.Stream

	waitBus(t, func() (bool, error) {
		var err error
		stream, err = js.Stream(t.Context(), bus.RiggerLogStreamName(s.config.Region))

		return err == nil, err
	})

	records := func(subject string) uint64 {
		info, err := stream.Info(t.Context(), jetstream.WithSubjectFilter(subject))
		must.NoError(t, err)

		return info.State.Subjects[subject]
	}

	// The log being written, before its operation is.
	live := s.newRiggerLogWriter(s.riggerLogSubject("live"))
	closed := false

	t.Cleanup(func() {
		if !closed {
			live.close()
		}
	})

	live.send([]byte(`{"msg":"plan"}`))
	waitBus(t, func() (bool, error) { return records(live.subject) > 0, nil })

	subjects := map[string]string{}

	for _, name := range []string{"shop-kept", "shop-deleted"} {
		subjects[name] = s.riggerLogSubject(name)

		_, err := s.writeRiggerOperation(nil, &RiggerOperation{
			Namespace: structs.DefaultNamespace, Name: name,
			Spec:   RiggerOperationSpec{Stack: "shop", Action: riggerOperationApply},
			Status: RiggerOperationStatus{Phase: riggerOpSucceeded, LogSubject: subjects[name]},
		})
		must.NoError(t, err)

		_, err = js.Publish(t.Context(), subjects[name], []byte(`{"msg":"done"}`))
		must.NoError(t, err)
	}

	var deleted structs.CustomObjectWriteResponse

	must.NoError(t, s.RPC("CustomObject.Delete", &structs.CustomObjectDeleteRequest{
		Group: structs.RiggerObjectGroup, Kind: structs.RiggerOperationKind, Name: "shop-deleted",
		WriteRequest: structs.WriteRequest{Region: s.Region(), Namespace: structs.DefaultNamespace, AuthToken: root.SecretID},
	}, &deleted))

	operator := newRiggerOperationOperator(s, s.logger)
	pass := func() {
		_, err := operator.reconcile(memdb.NewWatchSet())
		must.NoError(t, err)
	}

	waitBus(t, func() (bool, error) {
		pass()

		return records(subjects["shop-deleted"]) == 0, nil
	})

	must.Positive(t, records(subjects["shop-kept"]), must.Sprint("the log of an operation that is there"))
	must.Positive(t, records(live.subject), must.Sprint("the log being written"))

	live.close()

	closed = true

	waitBus(t, func() (bool, error) {
		pass()

		return records(live.subject) == 0, nil
	})
}
