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

// The tests of this file state what right on an OWNER it takes to mark a
// resource as its dependent (the open item "norca.owner" of the review round
// 2026-11-17; review findings JOG-20 and JOG-21).

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
// the right on that owner that naming an owner of an object takes (review
// finding JOG-20): with the right submit-job in one namespace a caller does
// not make a job the dependent of an object of another namespace, which it
// can neither read nor write. The administrator of that object deletes it in
// every mode, and the event log of the object never has the ID of such a
// job.
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

	// The mark is refused without a right on the owner, with the same answer
	// for an owner that does not exist, and no job is written.
	err := attacker.squatJob("other", "squat-shop", "shop")
	must.Eq(t, 403, errCode(t, err), must.Sprint("the owner mark was accepted without a right on the owner"))
	must.ErrorContains(t, err, `owner example.com/Database "shop" in namespace "default"`)
	must.ErrorContains(t, err, "requires the write or manage-finalizers capability on the owner")
	missingErr := attacker.squatJob("other", "squat-shop", "no-such-database")
	must.Eq(t, 403, errCode(t, missingErr))
	must.Eq(t, strings.ReplaceAll(err.Error(), `"shop"`, `"no-such-database"`), missingErr.Error())
	job, err := c.s.State().JobByID(nil, "other", "squat-shop")
	must.NoError(t, err)
	must.Nil(t, job)

	// (a) The administrator of the owner deletes it in the modes that take
	// the dependents.
	must.NoError(t, victim.deleteWith(testOwnerGroup, testOwnerKind, "shop", ""))
	c.waitGone(testOwnerGroup, testOwnerKind, "shop")
	must.NoError(t, victim.deleteWith(testOwnerGroup, testOwnerKind, "site", structs.ObjectPropagationForeground))
	c.waitGone(testOwnerGroup, testOwnerKind, "site")

	// (e) No job is attached to an owner that is being deleted in the
	// foreground mode, so its event log has no text of the attacker.
	must.NoError(t, victim.objectFinalizers(testOwnerKind, "held", []string{"example.com/hold"}, nil))
	must.NoError(t, victim.deleteWith(testOwnerGroup, testOwnerKind, "held", structs.ObjectPropagationForeground))
	err = attacker.squatJob("other", "text-chosen-by-the-attacker", "held")
	must.Eq(t, 403, errCode(t, err))
	must.Wait(t, wait.ContinualSuccess(wait.BoolFunc(func() bool {
		log, err := c.s.State().ObjectEventLog(nil, testOwnerGroup, testOwnerKind, "default", "held")
		must.NoError(t, err)
		return log == nil || !slices.ContainsFunc(log.Events, func(e *structs.ObjectEvent) bool {
			return e.Reason == objectEventDependentKept || strings.Contains(e.Message, "text-chosen-by-the-attacker")
		})
	}), wait.Timeout(time.Second), wait.Gap(100*time.Millisecond)),
		must.Sprint("the event log of the owner names a job of the attacker"))
	must.NoError(t, victim.objectFinalizers(testOwnerKind, "held", nil, []string{"example.com/hold"}))
	c.waitGone(testOwnerGroup, testOwnerKind, "held")

	// A job of the administrator, in the namespace of the owner, is a
	// dependent as before: it is stopped with its owner.
	must.NoError(t, victim.applyOwned(testOwnerGroup, testOwnerKind, "mine", map[string]any{}))
	must.NoError(t, victim.squatJob("default", "mine-app", "mine"))
	must.NoError(t, victim.deleteWith(testOwnerGroup, testOwnerKind, "mine", ""))
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		return c.jobIn("default", "mine-app").Stop
	}), wait.Timeout(30*time.Second), wait.Gap(100*time.Millisecond)), must.Sprint("the dependent job is stopped"))
}

// An object names its owners in Owners, and naming an owner takes "write" or
// "manage-finalizers" on the owner (checkObjectOwnerAccess), also when the
// owner is in the namespace of the object (review finding JOG-21): a caller
// with the rights of one kind in a namespace does not make an object of that
// kind the dependent of an object of another kind of the namespace. The
// refusal is the same for an owner that exists and for one that does not,
// and the ID of the owner is in the reply only for a writer who may read the
// owner.
func TestObjectOwnerMark_OtherKindOfTheSameNamespace(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := ownerMarkServer(t)
	defer cleanup()

	// The rights of the Hooks, none on the Databases.
	attacker := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1001, "attacker", `namespace "default" {
  object "example.com/Hook" { capabilities = ["read", "write", "manage-finalizers"] }
}`))
	victim := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1003, "victim", `namespace "default" {
  object "example.com/Database" { capabilities = ["read", "write", "manage-finalizers"] }
}`))
	// The Hooks, and the finalizers of the Databases: enough to name one as
	// an owner, not to read it.
	finalizers := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1005, "finalizers", `namespace "default" {
  object "example.com/Hook" { capabilities = ["read", "write", "manage-finalizers"] }
  object "example.com/Database" { capabilities = ["manage-finalizers"] }
}`))
	// The Hooks, and the Databases written and read.
	writer := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1007, "writer", `namespace "default" {
  object "example.com/Hook" { capabilities = ["read", "write", "manage-finalizers"] }
  object "example.com/Database" { capabilities = ["read", "write"] }
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
	hook := func(name string, owners ...*structs.ObjectOwnerReference) *structs.CustomObject {
		return &structs.CustomObject{Group: testOwnerGroup, Kind: testOwnerMarkHookKind, Name: name,
			Spec: map[string]any{}, Finalizers: []string{fin}, Owners: owners}
	}
	database := func(name string) *structs.ObjectOwnerReference {
		return ownedBy(testOwnerGroup, testOwnerKind, name)
	}

	// Without a right on the owner the reference is refused, with the same
	// answer for an owner that exists and for one that does not.
	_, err = attacker.applyObject(hook("hold", database("shop")))
	must.Eq(t, 403, errCode(t, err), must.Sprint("an owner of the same namespace was accepted without a right on it"))
	must.ErrorContains(t, err, `owner example.com/Database "shop" in namespace "default"`)
	must.ErrorContains(t, err, "requires the write or manage-finalizers capability on the owner")
	_, missingErr := attacker.applyObject(hook("probe", database("no-such-database")))
	must.Eq(t, 403, errCode(t, missingErr))
	must.Eq(t, strings.ReplaceAll(err.Error(), `"shop"`, `"no-such-database"`), missingErr.Error())
	must.Nil(t, c.object(testOwnerGroup, testOwnerMarkHookKind, "hold"))
	must.Nil(t, c.object(testOwnerGroup, testOwnerMarkHookKind, "probe"))

	// An object without an owner is theirs to write.
	_, err = attacker.applyObject(hook("alone"))
	must.NoError(t, err)

	// With the finalizers of the owner the reference is made. The reply has
	// no ID of an owner the writer may not read, whether it exists or not.
	written, err := finalizers.applyObject(hook("kept", database("shop")))
	must.NoError(t, err)
	must.Eq(t, "default", written.Owners[0].Namespace)
	must.Eq(t, "", written.Owners[0].ID, must.Sprint("the reply tells a caller who may not read the owner that it exists"))
	written, err = finalizers.applyObject(hook("kept-missing", database("no-such-database")))
	must.NoError(t, err)
	must.Eq(t, "", written.Owners[0].ID)

	// With write and read on the owner the reply has its ID, if it exists.
	written, err = writer.applyObject(hook("tracked", database("shop")))
	must.NoError(t, err)
	must.Eq(t, shop.ID, written.Owners[0].ID)
	written, err = writer.applyObject(hook("tracked-missing", database("no-such-database")))
	must.NoError(t, err)
	must.Eq(t, "", written.Owners[0].ID)

	// A writer of the dependent keeps the references it already has, without
	// the right on the owner and without learning its ID, but adds none.
	update := hook("tracked", database("shop"))
	update.Spec = map[string]any{"changed": true}
	written, err = attacker.applyObject(update)
	must.NoError(t, err)
	must.Len(t, 1, written.Owners)
	must.Eq(t, "", written.Owners[0].ID)
	must.True(t, structs.ObjectOwnedBy(c.object(testOwnerGroup, testOwnerMarkHookKind, "tracked"),
		testOwnerGroup, testOwnerKind, "default", "shop"))
	update.Owners = append(update.Owners, database("site"))
	_, err = attacker.applyObject(update)
	must.Eq(t, 403, errCode(t, err))
	must.ErrorContains(t, err, `owner example.com/Database "site" in namespace "default"`)

	// An owner of the same kind is checked like any other: whoever writes the
	// objects of a kind in a namespace has the right on them.
	_, err = attacker.applyObject(hook("parent"))
	must.NoError(t, err)
	written, err = attacker.applyObject(hook("child", ownedBy(testOwnerGroup, testOwnerMarkHookKind, "parent")))
	must.NoError(t, err)
	must.Eq(t, c.object(testOwnerGroup, testOwnerMarkHookKind, "parent").ID, written.Owners[0].ID)
}

// A volume request can be written, with or without an owner, only with the
// rights of the volume API: the sentence of the review finding SEC-8, "the
// owner reference can't be added without the csi right", holds. That right
// and the right to write requests are not enough to name an object of
// another kind of the namespace as the owner (review finding JOG-21): its
// administrator deletes it in every mode, and the request stays.
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

	// The check that holds: no request without the volume right, with an
	// owner (the check of the owner comes first) or without one.
	err := noVolume.applyOwned(structs.OperatorsGroup, csiVolumeRequestKind, "squat", deletes, database)
	must.Eq(t, 403, errCode(t, err))
	err = noVolume.applyOwned(structs.OperatorsGroup, csiVolumeRequestKind, "squat", deletes)
	must.Eq(t, 403, errCode(t, err))
	must.ErrorContains(t, err, "csi-write-volume")

	// With it, the mark is refused without a right on the owner.
	err = attacker.applyOwned(structs.OperatorsGroup, csiVolumeRequestKind, "squat", deletes, database)
	must.Eq(t, 403, errCode(t, err), must.Sprint("a volume request was made the dependent of an owner without a right on it"))
	must.ErrorContains(t, err, `owner example.com/Database "shop" in namespace "default"`)
	must.NoError(t, attacker.applyOwned(structs.OperatorsGroup, csiVolumeRequestKind, "squat", deletes))
	c.waitRequest("squat", "created", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return readyReason(obj) == "True" && slices.Contains(obj.Finalizers, structs.OperatorsFinalizer)
	})

	// The administrator of the owner deletes it in the default mode, and the
	// request of the attacker is none of its dependents.
	must.NoError(t, victim.deleteWith(testOwnerGroup, testOwnerKind, "shop", ""))
	c.waitGone(testOwnerGroup, testOwnerKind, "shop")
	squat := c.object(structs.OperatorsGroup, csiVolumeRequestKind, "squat")
	must.NotNil(t, squat)
	must.False(t, squat.Deleting())
}
