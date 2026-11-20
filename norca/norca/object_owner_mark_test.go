// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/objectgc"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// The tests of this file document what right on an OWNER it takes to mark a
// resource as its dependent (the open item "norca.owner" of the review round
// 2026-11-17), as the code stands. An assertion marked CURRENT BEHAVIOUR
// states what the servers do today and is a suspected defect: when the
// behaviour is fixed, that assertion is to be turned around, not kept.

const testOwnerMarkHookKind = "Hook"

// ownerMarkServer is dependentsTestServer with a collector that runs often
// and a second custom kind, example.com/Hook.
func ownerMarkServer(t *testing.T) (*certTest, func()) {
	t.Helper()
	c, _, _, cleanup := dependentsTestServer(t)
	c.s.objectGC.SetConfig(&objectgc.Config{
		Interval:           50 * time.Millisecond,
		IdleInterval:       time.Second,
		TombstoneRetention: time.Second,
	})
	var resp structs.ObjectKindUpsertResponse
	must.NoError(t, c.s.RPC("ObjectKind.UpsertObjectKind", &structs.ObjectKindUpsertRequest{
		ObjectKind:   &structs.ObjectKind{Group: testOwnerGroup, Kind: testOwnerMarkHookKind, Namespaced: true, Schema: `{"type": "object"}`},
		WriteRequest: c.write(""),
	}, &resp))
	return c, cleanup
}

// as returns the client of another token.
func (c *certTest) as(token *structs.ACLToken) *certTest {
	return &certTest{t: c.t, s: c.s, token: token.SecretID}
}

// applyObject writes an object of the default namespace as it is given.
func (c *certTest) applyObject(obj *structs.CustomObject) (*structs.CustomObject, error) {
	var resp structs.CustomObjectWriteResponse
	err := c.s.RPC("CustomObject.Apply", &structs.CustomObjectApplyRequest{Object: obj, WriteRequest: c.write("default")}, &resp)
	return resp.Object, err
}

// objectFinalizers adds and removes finalizers of an object of the default
// namespace.
func (c *certTest) objectFinalizers(kind, name string, add, remove []string) error {
	var resp structs.CustomObjectWriteResponse
	return c.s.RPC("CustomObject.UpdateFinalizers", &structs.CustomObjectFinalizersRequest{
		Group: testOwnerGroup, Kind: kind, Name: name, Add: add, Remove: remove,
		WriteRequest: c.write("default"),
	}, &resp)
}

// squatJob registers a job in a namespace whose meta names a Database of the
// default namespace as its owner.
func (c *certTest) squatJob(namespace, id, ownerName string) error {
	job := mock.Job()
	job.ID, job.Name, job.Namespace = id, id, namespace
	job.Meta = map[string]string{
		structs.JobMetaOwner:     structs.ObjectOwnerMetaValue(testOwnerGroup, testOwnerKind, "default", ownerName),
		structs.JobMetaOwnerKind: structs.ObjectKindRef(testOwnerGroup, testOwnerKind),
	}
	var resp structs.JobRegisterResponse
	return c.s.RPC("Job.Register", &structs.JobRegisterRequest{Job: job, WriteRequest: c.write(namespace)}, &resp)
}

func (c *certTest) jobIn(namespace, id string) *structs.Job {
	c.t.Helper()
	job, err := c.s.State().JobByID(nil, namespace, id)
	must.NoError(c.t, err)
	must.NotNil(c.t, job)
	return job
}

// A job names its owner in its meta (norca.owner), and Job.Register asks for
// no right on that owner: the owner may be an object of another namespace
// that the writer of the job can neither read nor write.
//
// CURRENT BEHAVIOUR: with the right submit-job in any one namespace a caller
// makes a job the dependent of an object of another namespace. The deletion
// of that object in the default (background) and in the foreground mode is
// then refused to everyone who may not stop jobs in the namespace of that
// job, whatever their rights in the namespace of the object are; only the
// orphan mode is left to them, which keeps their own dependents too. The
// servers also write the ID of such a job into the event log of the owner.
func TestObjectOwnerMark_JobOfAnotherNamespace(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := ownerMarkServer(t)
	defer cleanup()

	other := mock.Namespace()
	other.Name = "other"
	must.NoError(t, c.s.State().UpsertNamespaces(990, []*structs.Namespace{other}))

	// The attacker: jobs of "other", nothing in "default".
	attacker := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1001, "attacker", `namespace "other" {
  capabilities = ["submit-job", "read-job", "list-jobs"]
}`))
	// The administrator of "default": its Databases and its jobs.
	victim := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1003, "victim", `namespace "default" {
  object "example.com/Database" { capabilities = ["read", "write", "manage-finalizers"] }
  capabilities = ["submit-job", "read-job", "list-jobs"]
}`))

	for _, name := range []string{"shop", "site", "held"} {
		must.NoError(t, victim.applyOwned(testOwnerGroup, testOwnerKind, name, map[string]any{}))
	}

	// CURRENT BEHAVIOUR: the mark is accepted without a right on the owner.
	must.NoError(t, attacker.squatJob("other", "squat-shop", "shop"))
	must.NoError(t, attacker.squatJob("other", "squat-site", "site"))

	// (a) The deletion of the owner is refused to its administrator in the
	// modes that take the dependents. The error names the namespace of the
	// job, not the job.
	for _, mode := range []string{"", structs.ObjectPropagationBackground, structs.ObjectPropagationForeground} {
		err := victim.deleteWith(testOwnerGroup, testOwnerKind, "shop", mode)
		must.Eq(t, 403, errCode(t, err), must.Sprintf("mode %q", mode))
		must.ErrorContains(t, err, `in the namespace "other"`)
		must.ErrorContains(t, err, "submit-job")
		must.StrNotContains(t, err.Error(), "squat-shop")
	}
	must.False(t, c.object(testOwnerGroup, testOwnerKind, "shop").Deleting())

	// The orphan mode is the way out the administrator has.
	must.NoError(t, victim.deleteWith(testOwnerGroup, testOwnerKind, "shop", structs.ObjectPropagationOrphan))
	c.waitGone(testOwnerGroup, testOwnerKind, "shop")
	must.False(t, c.jobIn("other", "squat-shop").Stop)

	// (b) A management token passes the check, and the collector stops the
	// job of the attacker with the owner.
	must.NoError(t, c.deleteWith(testOwnerGroup, testOwnerKind, "site", ""))
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		return c.jobIn("other", "squat-site").Stop
	}), wait.Timeout(30*time.Second), wait.Gap(100*time.Millisecond)), must.Sprint("the job of the attacker is stopped"))

	// (e) A job that names an owner which is being deleted in the foreground
	// mode is not stopped (nobody was checked for it), and the servers say
	// so in the event log of the owner, with the ID the attacker chose.
	must.NoError(t, victim.objectFinalizers(testOwnerKind, "held", []string{"example.com/hold"}, nil))
	must.NoError(t, victim.deleteWith(testOwnerGroup, testOwnerKind, "held", structs.ObjectPropagationForeground))
	must.NoError(t, attacker.squatJob("other", "text-chosen-by-the-attacker", "held"))
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		log, err := c.s.State().ObjectEventLog(nil, testOwnerGroup, testOwnerKind, "default", "held")
		must.NoError(t, err)
		return log != nil && slices.ContainsFunc(log.Events, func(e *structs.ObjectEvent) bool {
			return e.Reason == objectEventDependentKept &&
				strings.Contains(e.Message, `"text-chosen-by-the-attacker"`) && strings.Contains(e.Message, `"other"`)
		})
	}), wait.Timeout(30*time.Second), wait.Gap(100*time.Millisecond)),
		must.Sprint("the event log of the owner names the job of the attacker"))
	must.NoError(t, victim.objectFinalizers(testOwnerKind, "held", nil, []string{"example.com/hold"}))
	c.waitGone(testOwnerGroup, testOwnerKind, "held")
	must.False(t, c.jobIn("other", "text-chosen-by-the-attacker").Stop)
}

// An object names its owners in Owners. An owner outside the namespace of
// the object takes "write" or "manage-finalizers" on the owner
// (checkObjectOwnerAccess); an owner in the same namespace takes nothing,
// whatever its kind is.
//
// CURRENT BEHAVIOUR: a caller with the rights of one kind in a namespace
// makes an object of that kind the dependent of an object of another kind of
// the namespace, which the caller can neither read nor write. The reply tells
// the caller whether that owner exists, and its ID. With a finalizer on the
// dependent, a foreground deletion of the owner never ends: the administrator
// of the owner can neither take it back nor change it to the orphan mode, nor
// remove the finalizer of the dependent. A management token ends it.
func TestObjectOwnerMark_OtherKindOfTheSameNamespace(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := ownerMarkServer(t)
	defer cleanup()

	attacker := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1001, "attacker", `namespace "default" {
  object "example.com/Hook" { capabilities = ["read", "write", "manage-finalizers"] }
}`))
	victim := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1003, "victim", `namespace "default" {
  object "example.com/Database" { capabilities = ["read", "write", "manage-finalizers"] }
}`))

	must.NoError(t, victim.applyOwned(testOwnerGroup, testOwnerKind, "shop", map[string]any{}))
	shop := c.object(testOwnerGroup, testOwnerKind, "shop")

	// The attacker may not read Databases.
	var get structs.SingleCustomObjectResponse
	err := c.s.RPC("CustomObject.GetCustomObject", &structs.CustomObjectSpecificRequest{
		Group: testOwnerGroup, Kind: testOwnerKind, Name: "shop", QueryOptions: attacker.query("default"),
	}, &get)
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())

	const fin = "example.com/never-removed"
	hook := func(name, owner string) *structs.CustomObject {
		return &structs.CustomObject{Group: testOwnerGroup, Kind: testOwnerMarkHookKind, Name: name,
			Spec: map[string]any{}, Finalizers: []string{fin}, Owners: []*structs.ObjectOwnerReference{ownedBy(testOwnerGroup, testOwnerKind, owner)}}
	}

	// CURRENT BEHAVIOUR: the mark is accepted without a right on the owner,
	// and (d) the reply has the ID of the owner if the owner exists.
	written, err := attacker.applyObject(hook("hold", "shop"))
	must.NoError(t, err)
	must.Eq(t, shop.ID, written.Owners[0].ID)
	written, err = attacker.applyObject(hook("probe", "no-such-database"))
	must.NoError(t, err)
	must.Eq(t, "", written.Owners[0].ID)

	// (c) The foreground deletion of the owner is accepted (the dependent
	// destroys no storage), the collector marks the dependent, and the owner
	// waits for the finalizer of the attacker.
	must.NoError(t, victim.deleteWith(testOwnerGroup, testOwnerKind, "shop", structs.ObjectPropagationForeground))
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		obj := c.object(testOwnerGroup, testOwnerMarkHookKind, "hold")
		return obj != nil && obj.Deleting()
	}), wait.Timeout(30*time.Second), wait.Gap(50*time.Millisecond)), must.Sprint("the dependent is marked for deletion"))
	must.Wait(t, wait.ContinualSuccess(wait.BoolFunc(func() bool {
		obj := c.object(testOwnerGroup, testOwnerKind, "shop")
		return obj != nil && obj.Deleting()
	}), wait.Timeout(2*time.Second), wait.Gap(100*time.Millisecond)), must.Sprint("the owner stays in deletion"))

	// The administrator of the owner has no way out: a deletion in the
	// orphan mode changes nothing of an object being deleted, and the
	// finalizer of the dependent is not theirs to remove.
	var resp structs.CustomObjectWriteResponse
	must.NoError(t, c.s.RPC("CustomObject.Delete", &structs.CustomObjectDeleteRequest{
		Group: testOwnerGroup, Kind: testOwnerKind, Name: "shop", Propagation: structs.ObjectPropagationOrphan,
		WriteRequest: victim.write("default"),
	}, &resp))
	must.True(t, resp.Unchanged)
	must.Eq(t, structs.ObjectPropagationForeground, c.object(testOwnerGroup, testOwnerKind, "shop").DeletionPropagation)
	err = victim.objectFinalizers(testOwnerMarkHookKind, "hold", nil, []string{fin})
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
	must.NotNil(t, c.object(testOwnerGroup, testOwnerKind, "shop"))

	// A management token ends it, on the dependent.
	must.NoError(t, c.objectFinalizers(testOwnerMarkHookKind, "hold", nil, []string{fin}))
	c.waitGone(testOwnerGroup, testOwnerMarkHookKind, "hold")
	c.waitGone(testOwnerGroup, testOwnerKind, "shop")
}

// A volume request can be written, with or without an owner, only with the
// rights of the volume API: the sentence of the review finding SEC-8, "the
// owner reference can't be added without the csi right", holds. With that
// right and the right to write requests, and none on the owner:
//
// CURRENT BEHAVIOUR: a request that does not retain its volume is made the
// dependent of an object of another kind of the namespace, and the deletion
// of that object is refused to its administrator in the modes that take the
// dependents (the check of SEC-8), who has the orphan mode left.
func TestObjectOwnerMark_VolumeRequestOfAnotherOwner(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := ownerMarkServer(t)
	defer cleanup()

	database := ownedBy(testOwnerGroup, testOwnerKind, "shop")
	deletes := volumeSpec(func(m map[string]any) { m["deletion_policy"] = "delete" })

	noVolume := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1001, "requests", `namespace "default" {
  object "norca.io/CSIVolumeRequest" { capabilities = ["read", "write"] }
}`))
	attacker := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1003, "volumes", `namespace "default" {
  object "norca.io/CSIVolumeRequest" { capabilities = ["read", "write"] }
  capabilities = ["csi-write-volume"]
}
plugin { policy = "read" }`))
	victim := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1005, "victim", `namespace "default" {
  object "example.com/Database" { capabilities = ["read", "write", "manage-finalizers"] }
}`))

	must.NoError(t, victim.applyOwned(testOwnerGroup, testOwnerKind, "shop", map[string]any{}))

	// The check that holds: no request, and so no owner mark on one, without
	// the volume right.
	err := noVolume.applyOwned(structs.OperatorsGroup, csiVolumeRequestKind, "squat", deletes, database)
	must.Eq(t, 403, errCode(t, err))
	must.ErrorContains(t, err, "csi-write-volume")

	// CURRENT BEHAVIOUR: with it, the mark is accepted without a right on
	// the owner.
	must.NoError(t, attacker.applyOwned(structs.OperatorsGroup, csiVolumeRequestKind, "squat", deletes, database))
	c.waitRequest("squat", "created", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return readyReason(obj) == "True" && slices.Contains(obj.Finalizers, structs.OperatorsFinalizer)
	})

	// (a) The administrator of the owner is refused, and is not told which
	// request it is (they may not read requests).
	for _, mode := range []string{"", structs.ObjectPropagationBackground, structs.ObjectPropagationForeground} {
		err := victim.deleteWith(testOwnerGroup, testOwnerKind, "shop", mode)
		must.Eq(t, 403, errCode(t, err), must.Sprintf("mode %q", mode))
		must.ErrorContains(t, err, "csi-write-volume")
		must.StrNotContains(t, err.Error(), "squat")
	}
	must.NoError(t, victim.deleteWith(testOwnerGroup, testOwnerKind, "shop", structs.ObjectPropagationOrphan))
	c.waitGone(testOwnerGroup, testOwnerKind, "shop")
	squat := c.object(structs.OperatorsGroup, csiVolumeRequestKind, "squat")
	must.NotNil(t, squat)
	must.False(t, squat.Deleting())
}
