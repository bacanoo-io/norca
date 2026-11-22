// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/bus"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
)

// A request that may not start an operation does nothing on the bus: the
// log of an operation starts after the rights are checked. The stream of
// the logs, deleted here, is what the writer of a log makes first; the
// leader's own check of it comes each minute.
func TestRiggerOperations_LogAfterAuthorization(t *testing.T) {
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

	name := bus.RiggerLogStreamName(s.config.Region)

	waitBus(t, func() (bool, error) {
		_, err := js.Stream(t.Context(), name)

		return err == nil, err
	})
	must.NoError(t, js.DeleteStream(t.Context(), name))

	releasesOnly := mock.CreatePolicyAndToken(t, s.State(), 1001, "log-releases-only",
		`namespace "default" { object "packs.norca.io/Release" { capabilities = ["read", "write"] } }`)

	tests := []struct {
		name    string
		token   string
		request RiggerStackRequest
		err     string
	}{
		{name: "anonymous", request: RiggerStackRequest{Files: []byte("x")}, err: structs.ErrPermissionDenied.Error()},
		{
			name: "published without read on registries", token: releasesOnly.SecretID,
			request: RiggerStackRequest{Source: "example.com/acme/modules//stacks/site"}, err: structs.ErrPermissionDenied.Error(),
		},
		{name: "neither files nor a source", token: root.SecretID, err: "either published"},
	}

	for _, method := range []string{"Rigger.StackApply", "Rigger.StackDestroy"} {
		for _, tt := range tests {
			req := tt.request
			req.WriteRequest = structs.WriteRequest{Region: s.config.Region, Namespace: structs.DefaultNamespace, AuthToken: tt.token}

			var resp RiggerOperationResponse

			must.ErrorContains(t, s.RPC(method, &req, &resp), tt.err, must.Sprint(method, " ", tt.name))

			_, err := js.Stream(t.Context(), name)
			must.ErrorIs(t, err, jetstream.ErrStreamNotFound, must.Sprint(method, " ", tt.name, ": the log writer started"))
		}
	}
}
