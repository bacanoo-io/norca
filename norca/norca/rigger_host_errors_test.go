// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/bacanoo-io/norca/rigger/host"
)

// Not found is the code of an error, not digits in its text: an address
// with :40400 in it would have new passwords generated over stored ones.
func TestRiggerHosts_NotFoundIsACode(t *testing.T) {
	ci.Parallel(t)

	missing := structs.NewErrRPCCoded(http.StatusNotFound, "secret not found")

	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{name: "port", err: errors.New("dial tcp 127.0.0.1:40400: connect: connection refused")},
		{name: "text", err: structs.NewErrRPCCoded(http.StatusInternalServerError, "lease 404 is broken")},
		{name: "no error"},
		{name: "coded", err: missing, expected: true},
		{name: "wrapped", err: fmt.Errorf("rpc error: %w", missing), expected: true},
		{name: "forwarded twice", err: errors.New("rpc error: rpc error: " + missing.Error()), expected: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must.Eq(t, tt.expected, errors.Is(notFoundErr(tt.err), host.ErrNotFound))
		})
	}
}

// A conflict is "made meanwhile" only when the secret is there: any other
// write that failed with 409 is an error.
func TestRiggerHosts_EnsureConflict(t *testing.T) {
	ci.Parallel(t)

	s, root, cleanup := TestACLServer(t, nil)
	t.Cleanup(cleanup)
	testutil.WaitForLeader(t, s.RPC)

	secrets := riggerSecrets{&riggerHosts{srv: s, token: root.SecretID, namespace: structs.DefaultNamespace}}
	secret := host.Secret{Path: "rigger/deleted", Items: map[string]host.Sensitive{"token": host.NewSensitive("one")}}

	must.Wait(t, wait.InitialSuccess(wait.ErrorFunc(func() error {
		_, err := secrets.Ensure(t.Context(), secret)

		return err
	})))

	var resp structs.SecretWriteResponse

	must.NoError(t, s.RPC("Secret.Delete", &structs.SecretVersionsRequest{
		Path: secret.Path, Versions: []uint64{1},
		WriteRequest: structs.WriteRequest{Region: s.Region(), Namespace: structs.DefaultNamespace, AuthToken: root.SecretID},
	}, &resp))

	written, err := secrets.Ensure(t.Context(), secret)
	must.False(t, written)
	must.Error(t, err, must.Sprint("a write that failed was taken for a secret that is there"))
}
