// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/objectgc"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// TestCustomObjectEndpoint_OwnerInAnotherNamespace checks that naming an
// owner outside the namespace of an object takes a right on the owner: a
// dependent with a finalizer holds back the foreground deletion of its
// owner, so without the check anyone who writes objects in any namespace
// could keep an object of another namespace in deletion for good.
func TestCustomObjectEndpoint_OwnerInAnotherNamespace(t *testing.T) {
	ci.Parallel(t)

	s, root, cleanupS := TestACLServer(t, nil)
	defer cleanupS()
	c := &objectClient{t: t, codec: rpcClient(t, s)}
	testutil.WaitForLeader(t, s.RPC)
	s.objectGC.SetConfig(&objectgc.Config{
		Interval:           50 * time.Millisecond,
		IdleInterval:       time.Second,
		TombstoneRetention: time.Second,
	})

	other := mock.Namespace()
	other.Name = "other"
	must.NoError(t, s.State().UpsertNamespaces(1000, []*structs.Namespace{other}))

	// Every right on the objects of "other", none elsewhere.
	outsider := mock.CreatePolicyAndToken(t, s.State(), 1001, "outsider", `
namespace "other" {
  object "db.example.com/*" { capabilities = ["read", "write", "write-status", "write-events", "manage-finalizers"] }
}`)
	// The same, and the owner may be written.
	ownerWriter := mock.CreatePolicyAndToken(t, s.State(), 1003, "owner-writer", `
namespace "other" {
  object "db.example.com/*" { capabilities = ["read", "write", "manage-finalizers"] }
}
namespace "default" {
  object "db.example.com/PostgresCluster" { capabilities = ["write"] }
}`)
	// The same, with the finalizers of the cluster-scoped kind.
	fleetController := mock.CreatePolicyAndToken(t, s.State(), 1005, "fleet-controller", `
namespace "other" {
  object "db.example.com/*" { capabilities = ["read", "write"] }
}
namespace "*" {
  object "db.example.com/Fleet" { capabilities = ["read", "manage-finalizers"] }
}`)

	_, err := c.upsertKind(testPGKind(true), false, root.SecretID)
	must.NoError(t, err)
	fleetKind := testPGKind(false)
	fleetKind.Kind = "Fleet"
	_, err = c.upsertKind(fleetKind, false, root.SecretID)
	must.NoError(t, err)

	spec := map[string]any{"version": "16"}
	_, err = c.apply(testPGObject("default", "victim", spec), nil, false, root.SecretID)
	must.NoError(t, err)
	fleet := testPGObject("", "main", spec)
	fleet.Kind = "Fleet"
	_, err = c.apply(fleet, nil, false, root.SecretID)
	must.NoError(t, err)

	const fin = "example.com/never-removed"
	victimRef := &structs.ObjectOwnerReference{Kind: "db.example.com/PostgresCluster", Namespace: "default", Name: "victim"}
	fleetRef := &structs.ObjectOwnerReference{Kind: "db.example.com/Fleet", Name: "main"}
	dependent := func(name string, owners ...*structs.ObjectOwnerReference) *structs.CustomObject {
		obj := testPGObject("other", name, spec)
		obj.Owners = owners
		obj.Finalizers = []string{fin}
		return obj
	}

	// Without a right on the owner the reference is refused: to an object
	// of another namespace, and to a cluster-scoped object.
	_, err = c.apply(dependent("hook", victimRef), nil, false, outsider.SecretID)
	must.Error(t, err, must.Sprint("an owner in another namespace was accepted without a right on it"))
	must.Eq(t, 403, errCode(t, err))
	must.ErrorContains(t, err, `owner db.example.com/PostgresCluster "victim" in namespace "default"`)
	_, err = c.apply(dependent("hook", fleetRef), nil, false, outsider.SecretID)
	must.Eq(t, 403, errCode(t, err))

	// An owner in the object's own namespace is checked too (JOG-21): the
	// writer of the kind has the right on it.
	_, err = c.apply(testPGObject("other", "parent", spec), nil, false, outsider.SecretID)
	must.NoError(t, err)
	_, err = c.apply(dependent("child", &structs.ObjectOwnerReference{Kind: "db.example.com/PostgresCluster", Name: "parent"}),
		nil, false, outsider.SecretID)
	must.NoError(t, err)

	// With write on the owner, or its finalizers, the reference is made.
	resp, err := c.apply(dependent("tracked", victimRef), nil, false, ownerWriter.SecretID)
	must.NoError(t, err)
	must.Eq(t, "default", resp.Object.Owners[0].Namespace)
	member := dependent("fleet-member", fleetRef)
	member.Finalizers = nil // the token has no manage-finalizers in "other"
	_, err = c.apply(member, nil, false, fleetController.SecretID)
	must.NoError(t, err)

	// A writer of the dependent keeps the references it already has, but
	// adds none.
	tracked := dependent("tracked", victimRef)
	tracked.Spec = map[string]any{"version": "17"}
	_, err = c.apply(tracked, nil, false, outsider.SecretID)
	must.NoError(t, err)
	tracked.Owners = append(tracked.Owners, fleetRef)
	_, err = c.apply(tracked, nil, false, outsider.SecretID)
	must.Eq(t, 403, errCode(t, err))

	// The victim's foreground deletion waits only for the dependent the
	// writer of the victim made.
	_, err = c.deleteWith("default", "victim", structs.ObjectPropagationForeground, root.SecretID)
	must.NoError(t, err)
	_, err = c.finalizers("other", "tracked", nil, []string{fin}, ownerWriter.SecretID)
	must.NoError(t, err)
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool {
			obj, err := c.get("default", "victim", root.SecretID)
			must.NoError(t, err)
			return obj == nil
		}),
		wait.Timeout(30*time.Second),
		wait.Gap(50*time.Millisecond),
	))
}
