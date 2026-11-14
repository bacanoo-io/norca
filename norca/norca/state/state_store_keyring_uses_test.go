// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

// TestStateStore_RootKeyMetaUses checks the count of what needs a root key,
// by kind, against IsRootKeyMetaInUse.
func TestStateStore_RootKeyMetaUses(t *testing.T) {
	ci.Parallel(t)
	store := testStateStore(t)
	const key = "f1a4b1c8-6a3d-4d0b-9d53-0c1e7a2b3c4d"

	uses := func() *RootKeyUses {
		t.Helper()
		u, err := store.RootKeyMetaUses(key)
		must.NoError(t, err)
		inUse, err := store.IsRootKeyMetaInUse(key)
		must.NoError(t, err)
		must.Eq(t, inUse, u.InUse())
		return u
	}
	must.Eq(t, &RootKeyUses{}, uses())

	// Two allocations signed with the key, one of them ended on both
	// sides, and one signed with another key.
	live, ended, other := mock.Alloc(), mock.Alloc(), mock.Alloc()
	live.SigningKeyID, ended.SigningKeyID, other.SigningKeyID = key, key, "another"
	ended.ClientStatus = structs.AllocClientStatusFailed
	ended.DesiredStatus = structs.AllocDesiredStatusStop
	must.NoError(t, store.UpsertAllocs(structs.MsgTypeTestSetup, 10, []*structs.Allocation{live, ended, other}))
	must.Eq(t, &RootKeyUses{Allocations: 1}, uses())

	// Two Variables encrypted with the key.
	for i, path := range []string{"a", "b", "c"} {
		v := mock.VariableEncrypted()
		v.Path = path
		v.KeyID = key
		if path == "c" {
			v.KeyID = "another"
		}
		resp := store.VarSet(uint64(20+i), &structs.VarApplyStateRequest{Op: structs.VarOpSet, Var: v})
		must.NoError(t, resp.Error)
	}
	must.Eq(t, &RootKeyUses{Allocations: 1, Variables: 2}, uses())

	// A certificate authority whose key it encrypts.
	root := &structs.MTLSRoot{ID: "r1", Region: "global", Active: true, SigningKeyID: key, SigningKey: []byte("x")}
	must.NoError(t, store.UpsertMTLSRoots(structs.MsgTypeTestSetup, 30, []*structs.MTLSRoot{root}, nil))
	must.Eq(t, &RootKeyUses{Allocations: 1, Variables: 2, MTLSRoots: 1}, uses())
}
