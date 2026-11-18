// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"net"
	"testing"
	"time"

	"github.com/hashicorp/go-msgpack/codec"
	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/ci"
	cstructs "github.com/bacanoo-io/norca/norca/client/structs"
	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// execPolicyFixture adds to boundPolicyFixture an allocation of the job
// "ctrl" (policy "ctrl-policy" bound), one of a periodic child of the job
// "cron" (policy "cron-policy" bound to the parent), one of a job without
// bound policies, and tokens with alloc-exec: one without the bound
// policies, one that names "ctrl-policy" and one that has it through a role.
type execPolicyFixture struct {
	*boundPolicyFixture

	ctrlAlloc, childAlloc, plainAlloc *structs.Allocation
	exec, execHolder, execRoleHolder  *structs.ACLToken
}

func newExecPolicyFixture(t *testing.T) (*execPolicyFixture, func()) {
	bf, cleanup := newBoundPolicyFixture(t)
	f := &execPolicyFixture{boundPolicyFixture: bf}
	store := bf.srv.fsm.State()
	index, err := store.LatestIndex()
	must.NoError(t, err)

	child := mock.Job()
	child.ID = "cron/periodic-1700000000"
	child.ParentID = "cron"
	plain := mock.Job()
	must.NoError(t, store.UpsertJob(structs.MsgTypeTestSetup, index+1, nil, child))
	must.NoError(t, store.UpsertJob(structs.MsgTypeTestSetup, index+2, nil, plain))

	allocOf := func(job *structs.Job) *structs.Allocation {
		alloc := mock.Alloc()
		alloc.Job, alloc.JobID, alloc.Namespace = job, job.ID, job.Namespace
		return alloc
	}
	f.ctrlAlloc = allocOf(bf.live("ctrl"))
	f.childAlloc = allocOf(child)
	f.plainAlloc = allocOf(plain)
	must.NoError(t, store.UpsertAllocs(structs.MsgTypeTestSetup, index+3,
		[]*structs.Allocation{f.ctrlAlloc, f.childAlloc, f.plainAlloc}))

	exec := mock.NamespacePolicy(structs.DefaultNamespace, "", []string{acl.NamespaceCapabilityAllocExec})
	f.exec = mock.CreatePolicyAndToken(t, store, index+4, "exec", exec)
	f.execHolder = mock.CreateToken(t, store, index+6, []string{"exec", "ctrl-policy"})
	f.execRoleHolder = mock.ACLToken()
	f.execRoleHolder.Policies = []string{"exec"}
	f.execRoleHolder.Roles = bf.roleHolder.Roles
	f.execRoleHolder.SetHash()
	must.NoError(t, store.UpsertACLTokens(structs.MsgTypeTestSetup, index+7,
		[]*structs.ACLToken{f.execRoleHolder}))
	return f, cleanup
}

func (f *execPolicyFixture) check(allocID, token string) error {
	var reply structs.GenericResponse
	return f.call("Alloc.CheckExecBoundPolicies", &structs.AllocSpecificRequest{
		AllocID:      allocID,
		QueryOptions: structs.QueryOptions{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: token},
	}, &reply)
}

// A command run in an allocation by exec can use the ACL policies bound to
// its job (review finding JOG-14): exec into it needs a token that holds each
// of them, by name or through a role, or a management token.
func TestAlloc_CheckExecBoundPolicies(t *testing.T) {
	ci.Parallel(t)
	f, cleanup := newExecPolicyFixture(t)
	defer cleanup()

	// alloc-exec alone is refused, with the policy it lacks named.
	err := f.check(f.ctrlAlloc.ID, f.exec.SecretID)
	must.ErrorContains(t, err, "403,Permission denied")
	must.ErrorContains(t, err, `"ctrl-policy"`)
	must.ErrorContains(t, err, `its job "ctrl"`)

	// The policy by name, through a role, or a management token.
	must.NoError(t, f.check(f.ctrlAlloc.ID, f.execHolder.SecretID))
	must.NoError(t, f.check(f.ctrlAlloc.ID, f.execRoleHolder.SecretID))
	must.NoError(t, f.check(f.ctrlAlloc.ID, f.root.SecretID))

	// A child job's allocation runs with its parent's bound policies.
	err = f.check(f.childAlloc.ID, f.exec.SecretID)
	must.ErrorContains(t, err, "403,Permission denied")
	must.ErrorContains(t, err, `"cron-policy"`)
	must.ErrorContains(t, err, `the parent job "cron"`)
	must.NoError(t, f.check(f.childAlloc.ID, f.root.SecretID))

	// A job without bound policies: alloc-exec is enough.
	must.NoError(t, f.check(f.plainAlloc.ID, f.exec.SecretID))

	// Without alloc-exec the caller learns nothing about the policies.
	err = f.check(f.ctrlAlloc.ID, f.team.SecretID)
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
	must.StrNotContains(t, err.Error(), "ctrl-policy")
	err = f.check(f.ctrlAlloc.ID, uuid.Generate())
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())

	// An allocation the servers don't know can't be checked: refused.
	must.ErrorContains(t, f.check(uuid.Generate(), f.exec.SecretID), "Unknown allocation")
}

// The server that forwards an exec to the client refuses it too when the
// token doesn't hold the policies bound to the allocation's job, before it
// looks for the node.
func TestClientAllocations_Exec_BoundPolicies(t *testing.T) {
	ci.Parallel(t)
	f, cleanup := newExecPolicyFixture(t)
	defer cleanup()

	execErr := func(allocID, token string) *cstructs.RpcError {
		t.Helper()
		handler, err := f.srv.StreamingRpcHandler("Allocations.Exec")
		must.NoError(t, err)
		p1, p2 := net.Pipe()
		defer p1.Close()
		defer p2.Close()
		go handler(p2)

		first := make(chan cstructs.StreamErrWrapper, 1)
		go func() {
			var msg cstructs.StreamErrWrapper
			if err := codec.NewDecoder(p1, structs.MsgpackHandle).Decode(&msg); err != nil {
				msg.Error = cstructs.NewRpcError(err, nil)
			}
			first <- msg
		}()
		must.NoError(t, codec.NewEncoder(p1, structs.MsgpackHandle).Encode(&cstructs.AllocExecRequest{
			AllocID:      allocID,
			Task:         "web",
			Cmd:          []string{"sh"},
			QueryOptions: structs.QueryOptions{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: token},
		}))
		select {
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for the exec stream")
		case msg := <-first:
			must.NotNil(t, msg.Error)
			return msg.Error
		}
		return nil
	}

	denied := execErr(f.ctrlAlloc.ID, f.exec.SecretID)
	must.StrContains(t, denied.Message, structs.ErrPermissionDenied.Error())
	must.StrContains(t, denied.Message, `"ctrl-policy"`)
	must.NotNil(t, denied.Code)
	must.Eq(t, 403, *denied.Code)

	// With the policy the server goes on to the node, which the mock
	// allocation's node isn't.
	for _, token := range []string{f.execHolder.SecretID, f.root.SecretID} {
		must.StrContains(t, execErr(f.ctrlAlloc.ID, token).Message, "Unknown node")
	}
	must.StrContains(t, execErr(f.plainAlloc.ID, f.exec.SecretID).Message, "Unknown node")
}
