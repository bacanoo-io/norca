// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"net"
	"testing"
	"time"

	"github.com/hashicorp/go-msgpack/codec"
	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/client/config"
	cstructs "github.com/bacanoo-io/norca/norca/client/structs"
	"github.com/bacanoo-io/norca/norca/helper/pluginutils/catalog"
	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/bacanoo-io/norca/norca/norca"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	nstructs "github.com/bacanoo-io/norca/norca/norca/structs"
	nconfig "github.com/bacanoo-io/norca/norca/norca/structs/config"
	"github.com/bacanoo-io/norca/norca/plugins/drivers"
	"github.com/bacanoo-io/norca/norca/testutil"
)

// A command run by `alloc exec` sees the task's environment and secrets
// directory, so its workload identity token and the ACL policies bound to the
// job (review finding JOG-14). Exec into an allocation of a job with bound
// policies needs a token that holds each of them, directly or through a role,
// or a management token; alloc-exec (and alloc-node-exec for a driver without
// file system isolation) is enough for any other allocation.
func TestAlloc_ExecStreaming_BoundPolicies(t *testing.T) {
	ci.Parallel(t)

	for _, isolation := range []drivers.FSIsolation{drivers.FSIsolationImage, drivers.FSIsolationNone} {
		t.Run(string(isolation), func(t *testing.T) {
			testExecBoundPolicies(t, isolation)
		})
	}
}

func testExecBoundPolicies(t *testing.T, isolation drivers.FSIsolation) {
	s, root, cleanupS := norca.TestACLServer(t, nil)
	defer cleanupS()
	testutil.WaitForLeader(t, s.RPC)

	client, cleanupC := TestClient(t, func(c *config.Config) {
		c.ACLEnabled = true
		c.Servers = []string{s.GetConfig().RPCAddr.String()}
		c.RPCHoldTimeout = time.Second // the retries without servers below
		c.PluginLoader = catalog.TestPluginLoaderWithOptions(t, "", map[string]string{},
			[]*nconfig.PluginConfig{{
				Name:   "mock_driver",
				Config: map[string]any{"fs_isolation": string(isolation)},
			}})
	})
	defer cleanupC()

	job := mock.BatchJob()
	job.TaskGroups[0].Count = 1
	job.TaskGroups[0].Tasks[0].Config = map[string]any{
		"run_for": "60s",
		"exec_command": map[string]any{
			"run_for":       "1ms",
			"stdout_string": "some output",
		},
	}
	allocs := testutil.WaitForRunningWithToken(t, s.RPC, job, root.SecretID)
	must.Len(t, 1, allocs)
	allocID := allocs[0].ID

	store := s.State()
	index, err := store.LatestIndex()
	must.NoError(t, err)
	exec := mock.NamespacePolicy(nstructs.DefaultNamespace, "",
		[]string{acl.NamespaceCapabilityAllocExec, acl.NamespaceCapabilityAllocNodeExec})
	execOnly := mock.CreatePolicyAndToken(t, store, index+1, "exec", exec)

	// execInto starts an exec session with the token and returns the error
	// the client refused it with, nil when the session sent its first frame.
	execInto := func(t *testing.T, token string) *cstructs.RpcError {
		t.Helper()
		req := &cstructs.AllocExecRequest{
			AllocID: allocID,
			Task:    job.TaskGroups[0].Tasks[0].Name,
			Cmd:     []string{"placeholder command"},
			QueryOptions: nstructs.QueryOptions{
				Region:    "global",
				AuthToken: token,
				Namespace: nstructs.DefaultNamespace,
			},
		}
		handler, err := client.StreamingRpcHandler("Allocations.Exec")
		must.NoError(t, err)
		p1, p2 := net.Pipe()
		defer p1.Close()
		defer p2.Close()
		go handler(p2)

		// The first message of the stream: an error or a frame.
		first := make(chan cstructs.StreamErrWrapper, 1)
		go func() {
			var msg cstructs.StreamErrWrapper
			if err := codec.NewDecoder(p1, nstructs.MsgpackHandle).Decode(&msg); err != nil {
				msg.Error = cstructs.NewRpcError(err, nil)
			}
			first <- msg
		}()
		must.NoError(t, codec.NewEncoder(p1, nstructs.MsgpackHandle).Encode(req))

		select {
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for the exec session")
		case msg := <-first:
			if msg.Error == nil {
				must.SliceNotEmpty(t, msg.Payload)
			}
			return msg.Error
		}
		return nil
	}

	// No policies are bound to the job yet: alloc-exec is enough.
	if rpcErr := execInto(t, execOnly.SecretID); rpcErr != nil {
		t.Fatalf("exec refused: %s", rpcErr.Message)
	}

	// Bind a policy to the job, and give it to a token by name and to
	// another through a role.
	bound := &nstructs.ACLPolicy{
		Name:   "ops-policy",
		Rules:  `namespace "default" { policy = "read" }`,
		JobACL: &nstructs.JobACL{Namespace: nstructs.DefaultNamespace, JobID: job.ID},
	}
	bound.SetHash()
	must.NoError(t, store.UpsertACLPolicies(nstructs.MsgTypeTestSetup, index+3, []*nstructs.ACLPolicy{bound}))
	holder := mock.CreateToken(t, store, index+4, []string{"exec", "ops-policy"})
	role := &nstructs.ACLRole{
		ID:       uuid.Generate(),
		Name:     "ops-role",
		Policies: []*nstructs.ACLRolePolicyLink{{Name: "ops-policy"}},
	}
	role.SetHash()
	must.NoError(t, store.UpsertACLRoles(nstructs.MsgTypeTestSetup, index+5, []*nstructs.ACLRole{role}, false))
	roleHolder := mock.ACLToken()
	roleHolder.Policies = []string{"exec"}
	roleHolder.Roles = []*nstructs.ACLTokenRoleLink{{ID: role.ID}}
	roleHolder.SetHash()
	must.NoError(t, store.UpsertACLTokens(nstructs.MsgTypeTestSetup, index+6, []*nstructs.ACLToken{roleHolder}))

	t.Run("alloc-exec without the bound policy", func(t *testing.T) {
		rpcErr := execInto(t, execOnly.SecretID)
		must.NotNil(t, rpcErr)
		must.StrContains(t, rpcErr.Message, nstructs.ErrPermissionDenied.Error())
		must.StrContains(t, rpcErr.Message, `"ops-policy"`)
		must.NotNil(t, rpcErr.Code)
		must.Eq(t, 403, *rpcErr.Code)
	})
	for name, token := range map[string]string{
		"policy by name":   holder.SecretID,
		"policy by role":   roleHolder.SecretID,
		"management token": root.SecretID,
	} {
		t.Run(name, func(t *testing.T) {
			if rpcErr := execInto(t, token); rpcErr != nil {
				t.Fatalf("exec refused: %s", rpcErr.Message)
			}
		})
	}

	// Without the servers the client can't know the bound policies: it
	// refuses every token but a management one (the tokens are cached).
	t.Run("servers unreachable", func(t *testing.T) {
		must.NoError(t, s.Shutdown())
		rpcErr := execInto(t, holder.SecretID)
		must.NotNil(t, rpcErr)
		must.StrContains(t, rpcErr.Message, "exec needs the servers to check the ACL policies bound to the job")
		if rpcErr := execInto(t, root.SecretID); rpcErr != nil {
			t.Fatalf("exec refused: %s", rpcErr.Message)
		}
	})
}
