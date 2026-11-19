// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/client/config"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/shoenig/test/must"
)

// rolesUnreachable passes the RPCs of the client to the server, except the
// lookup of ACL roles while fail is set, as if no server answered.
type rolesUnreachable struct {
	config.RPCHandler
	fail atomic.Bool
}

func (h *rolesUnreachable) RPC(method string, args, reply any) error {
	if h.fail.Load() && method == structs.ACLGetRolesByIDRPCMethod {
		return errors.New("no servers")
	}
	return h.RPCHandler.RPC(method, args, reply)
}

// When the servers can't be asked and only expired roles are needed, the
// client resolves a token's roles from its expired cache entries, as it
// does for tokens and policies (review finding MUK-27; inherited). It used
// to read the empty response of the failed RPC instead, so the policies of
// the roles were dropped and a token that gets its rights through roles
// lost them on the client while the servers were away.
func TestClient_resolveTokenACLRoles_ExpiredCacheOnRPCError(t *testing.T) {
	ci.Parallel(t)

	testServer, _, rootACLToken, cleanupServer := testACLServer(t, nil)
	defer cleanupServer()
	testutil.WaitForLeader(t, testServer.RPC)

	handler := &rolesUnreachable{RPCHandler: testServer}
	testClient, cleanup := TestClient(t, func(c *config.Config) {
		c.RPCHandler = handler
		c.ACLEnabled = true
	})
	defer cleanup()

	role := mock.ACLRole()
	must.NoError(t, testServer.State().UpsertACLRoles(structs.MsgTypeTestSetup, 10, []*structs.ACLRole{role}, true))
	links := []*structs.ACLTokenRoleLink{{ID: role.ID}}

	want, err := testClient.resolveTokenACLRoles(rootACLToken.SecretID, links)
	must.NoError(t, err)
	must.Len(t, 2, want)

	// The cached role expires and the servers stop answering.
	expiredAt := time.Now().Add(-2 * testClient.GetConfig().ACLRoleTTL)
	testClient.roleCache.AddAtTime(role.ID, role, expiredAt)
	handler.fail.Store(true)

	got, err := testClient.resolveTokenACLRoles(rootACLToken.SecretID, links)
	must.NoError(t, err)
	must.SliceContainsAll(t, want, got)

	// A role the client never cached can't be resolved without the servers.
	other := mock.ACLRole()
	_, err = testClient.resolveTokenACLRoles(rootACLToken.SecretID,
		[]*structs.ACLTokenRoleLink{{ID: role.ID}, {ID: other.ID}})
	must.ErrorContains(t, err, "no servers")

	// Once the servers answer again, the role is looked up and cached anew.
	handler.fail.Store(false)
	got, err = testClient.resolveTokenACLRoles(rootACLToken.SecretID, links)
	must.NoError(t, err)
	must.SliceContainsAll(t, want, got)
	entry, ok := testClient.roleCache.Get(role.ID)
	must.True(t, ok)
	must.True(t, entry.Age() < time.Since(expiredAt), must.Sprint("the role was not cached anew"))
}
