// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/objectgc"
	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

const (
	testOwnerGroup = "example.com"
	testOwnerKind  = "Database"
)

// dependentsTestServer starts a server with ACLs whose operators of volume
// requests and of snapshot schedules use fake plugins, with the plugin
// "minnie", its volume "db", and the custom kind example.com/Database, a
// kind that owns volume requests as an operator of databases would make
// them.
func dependentsTestServer(t *testing.T, cbs ...func(*Config)) (*certTest, *fakeVolumeCSI, *fakeSnapshotCSI, func()) {
	t.Helper()
	volumes, snapshots := &fakeVolumeCSI{}, newFakeSnapshotCSI()
	s, root, cleanup := secretTestServer(t, true, func(c *Config) {
		c.volumeCSI, c.snapshotCSI = volumes, snapshots
		for _, cb := range cbs {
			cb(c)
		}
	})

	node := mock.Node()
	node.CSIControllerPlugins = map[string]*structs.CSIInfo{
		"minnie": {
			PluginID: "minnie",
			Healthy:  true,
			ControllerInfo: &structs.CSIControllerInfo{
				SupportsCreateDelete:         true,
				SupportsCreateDeleteSnapshot: true,
			},
			RequiresControllerPlugin: true,
		},
	}
	node.CSINodePlugins = map[string]*structs.CSIInfo{
		"minnie": {
			PluginID:                 "minnie",
			Healthy:                  true,
			NodeInfo:                 &structs.CSINodeInfo{MaxVolumes: 10},
			RequiresControllerPlugin: true,
		},
	}
	must.NoError(t, s.State().UpsertNode(structs.MsgTypeTestSetup, 900, node))
	must.NoError(t, s.State().UpsertCSIVolume(901, []*structs.CSIVolume{{
		ID:             "db",
		Namespace:      structs.DefaultNamespace,
		AccessMode:     structs.CSIVolumeAccessModeSingleNodeWriter,
		AttachmentMode: structs.CSIVolumeAttachmentModeFilesystem,
		PluginID:       "minnie",
		ExternalID:     "ext-db",
	}}))

	waitOperatorKinds(t, s)

	c := &certTest{t: t, s: s, token: root.SecretID}
	var resp structs.ObjectKindUpsertResponse
	must.NoError(t, s.RPC("ObjectKind.UpsertObjectKind", &structs.ObjectKindUpsertRequest{
		ObjectKind:   &structs.ObjectKind{Group: testOwnerGroup, Kind: testOwnerKind, Namespaced: true, Schema: `{"type": "object"}`},
		WriteRequest: c.write(""),
	}, &resp))
	return c, volumes, snapshots, cleanup
}

// ownedBy is an owner reference to an object of the default namespace.
func ownedBy(group, kind, name string) *structs.ObjectOwnerReference {
	return &structs.ObjectOwnerReference{Kind: structs.ObjectKindRef(group, kind), Name: name}
}

// applyOwned writes an object of the default namespace with its owners.
func (c *certTest) applyOwned(group, kind, name string, spec map[string]any, owners ...*structs.ObjectOwnerReference) error {
	var resp structs.CustomObjectWriteResponse
	return c.s.RPC("CustomObject.Apply", &structs.CustomObjectApplyRequest{
		Object:       &structs.CustomObject{Group: group, Kind: kind, Name: name, Spec: spec, Owners: owners},
		WriteRequest: c.write("default"),
	}, &resp)
}

// deleteWith deletes an object of the default namespace in a propagation
// mode.
func (c *certTest) deleteWith(group, kind, name, propagation string) error {
	var resp structs.CustomObjectWriteResponse
	return c.s.RPC("CustomObject.Delete", &structs.CustomObjectDeleteRequest{
		Group: group, Kind: kind, Name: name, Propagation: propagation,
		WriteRequest: c.write("default"),
	}, &resp)
}

func (c *certTest) object(group, kind, name string) *structs.CustomObject {
	c.t.Helper()
	obj, err := c.s.State().CustomObjectByName(nil, group, kind, "default", name)
	must.NoError(c.t, err)
	return obj
}

func (c *certTest) waitGone(group, kind, name string) {
	c.t.Helper()
	must.Wait(c.t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		return c.object(group, kind, name) == nil
	}), wait.Timeout(30*time.Second), wait.Gap(100*time.Millisecond)), must.Sprintf("%s %s is gone", kind, name))
}

// volumeRequestOwnedBy writes a volume request with a deletion policy and
// owners, and waits for its volume.
func (c *certTest) volumeRequestOwnedBy(name, policy string, owners ...*structs.ObjectOwnerReference) {
	c.t.Helper()
	must.NoError(c.t, c.applyOwned(structs.OperatorsGroup, csiVolumeRequestKind, name,
		volumeSpec(func(m map[string]any) { m["deletion_policy"] = policy }), owners...))
	c.waitRequest(name, "created", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return readyReason(obj) == "True" && slices.Contains(obj.Finalizers, structs.OperatorsFinalizer)
	})
}

// scheduleOwnedBy writes a snapshot schedule of the volume "db" that deletes
// its snapshots with it, with two snapshots taken, and its owners.
func (c *certTest) scheduleOwnedBy(fake *fakeSnapshotCSI, name string, owners ...*structs.ObjectOwnerReference) {
	c.t.Helper()
	must.NoError(c.t, c.applyOwned(structs.OperatorsGroup, volumeSnapshotScheduleKind, name, map[string]any{
		"volume": "db", "schedule": farSchedule, "keep": map[string]any{"last": 5}, "delete_snapshots": true,
	}, owners...))
	c.waitSchedule(name, "finalizer", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool {
		return slices.Contains(obj.Finalizers, structs.OperatorsFinalizer)
	})
	obj, _ := c.schedule(name)
	writeTestScheduleStatus(c.t, c.s, obj, &snapshotScheduleStatus{Snapshots: listedSnapshots(fake, name, 2)})
}

// The deletion of an object takes its dependents with it, and the garbage
// collector deletes them with the rights of the servers. Deleting an owner of
// a volume request that does not retain its volume, or of a snapshot schedule
// that deletes its snapshots, needs the rights of the volume API as deleting
// the dependent itself does (review finding SEC-8).
func TestObjectDelete_DependentsThatDestroyStorage(t *testing.T) {
	ci.Parallel(t)
	c, volumes, snapshots, cleanup := dependentsTestServer(t)
	defer cleanup()

	request := func(name string) *structs.ObjectOwnerReference {
		return ownedBy(structs.OperatorsGroup, csiVolumeRequestKind, name)
	}
	database := func(name string) *structs.ObjectOwnerReference {
		return ownedBy(testOwnerGroup, testOwnerKind, name)
	}

	// A request that retains its volume (deleting it needs the object right
	// alone) owns a schedule that deletes its snapshots.
	c.volumeRequestOwnedBy("vol", "retain")
	c.scheduleOwnedBy(snapshots, "nightly", request("vol"))
	c.volumeRequestOwnedBy("vol-orphaned", "retain")
	c.scheduleOwnedBy(snapshots, "weekly", request("vol-orphaned"))

	// An object of a custom kind owns a request that deletes its volume,
	// and another one owns such a request through an object in between.
	for _, name := range []string{"shop", "app"} {
		must.NoError(t, c.applyOwned(testOwnerGroup, testOwnerKind, name, map[string]any{}))
	}
	c.volumeRequestOwnedBy("shop-data", "delete", database("shop"))
	must.NoError(t, c.applyOwned(testOwnerGroup, testOwnerKind, "app-replica", map[string]any{}, database("app")))
	c.volumeRequestOwnedBy("app-data", "deregister", database("app-replica"))

	// A dependent that destroys nothing is no reason to refuse.
	must.NoError(t, c.applyOwned(testOwnerGroup, testOwnerKind, "blog", map[string]any{}))
	c.volumeRequestOwnedBy("blog-data", "retain", database("blog"))

	objects := mock.CreatePolicyAndToken(t, c.s.State(), 1001, "objects", `namespace "default" {
  object "norca.io/CSIVolumeRequest" { capabilities = ["read", "write"] }
  object "example.com/Database" { capabilities = ["read", "write"] }
}`)
	volumeRight := mock.CreatePolicyAndToken(t, c.s.State(), 1003, "volumes", `namespace "default" {
  object "norca.io/CSIVolumeRequest" { capabilities = ["read", "write"] }
  object "example.com/Database" { capabilities = ["read", "write"] }
  capabilities = ["csi-write-volume"]
}
plugin { policy = "read" }`)
	noVolume := &certTest{t: t, s: c.s, token: objects.SecretID}
	writer := &certTest{t: t, s: c.s, token: volumeRight.SecretID}

	// Without the volume right: refused in both modes that take the
	// dependents, and nothing is marked.
	for _, mode := range []string{"", structs.ObjectPropagationBackground, structs.ObjectPropagationForeground} {
		err := noVolume.deleteWith(structs.OperatorsGroup, csiVolumeRequestKind, "vol", mode)
		must.Eq(t, 403, errCode(t, err), must.Sprintf("mode %q", mode))
		must.ErrorContains(t, err, "csi-write-volume")
		// The caller may not read schedules: the dependent is not named.
		must.StrNotContains(t, err.Error(), "nightly")

		err = noVolume.deleteWith(testOwnerGroup, testOwnerKind, "shop", mode)
		must.Eq(t, 403, errCode(t, err), must.Sprintf("mode %q", mode))
		must.ErrorContains(t, err, `norca.io/CSIVolumeRequest "shop-data"`)

		err = noVolume.deleteWith(testOwnerGroup, testOwnerKind, "app", mode)
		must.Eq(t, 403, errCode(t, err), must.Sprintf("mode %q", mode))
		must.ErrorContains(t, err, `norca.io/CSIVolumeRequest "app-data"`)
	}
	for _, obj := range []*structs.CustomObject{
		c.object(structs.OperatorsGroup, csiVolumeRequestKind, "vol"),
		c.object(structs.OperatorsGroup, volumeSnapshotScheduleKind, "nightly"),
		c.object(testOwnerGroup, testOwnerKind, "shop"),
		c.object(structs.OperatorsGroup, csiVolumeRequestKind, "shop-data"),
		c.object(testOwnerGroup, testOwnerKind, "app"),
		c.object(testOwnerGroup, testOwnerKind, "app-replica"),
		c.object(structs.OperatorsGroup, csiVolumeRequestKind, "app-data"),
	} {
		must.NotNil(t, obj)
		must.False(t, obj.Deleting(), must.Sprintf("%s is marked for deletion", obj.Name))
	}

	// A dry run is refused the same way.
	var resp structs.CustomObjectWriteResponse
	err := c.s.RPC("CustomObject.Delete", &structs.CustomObjectDeleteRequest{
		Group: testOwnerGroup, Kind: testOwnerKind, Name: "shop", DryRun: true,
		WriteRequest: noVolume.write("default"),
	}, &resp)
	must.Eq(t, 403, errCode(t, err))

	// The orphan mode takes no dependent: the object right is enough, and
	// the schedule stays with its snapshots.
	must.NoError(t, noVolume.deleteWith(structs.OperatorsGroup, csiVolumeRequestKind, "vol-orphaned", structs.ObjectPropagationOrphan))
	c.waitGone(structs.OperatorsGroup, csiVolumeRequestKind, "vol-orphaned")

	// A dependent whose deletion destroys nothing goes with its owner.
	must.NoError(t, noVolume.deleteWith(testOwnerGroup, testOwnerKind, "blog", ""))
	c.waitGone(structs.OperatorsGroup, csiVolumeRequestKind, "blog-data")
	must.NotNil(t, c.volume("blog-data"))

	// With the volume right the same deletions go through, and the
	// dependents go with their owners: the volumes and the snapshots too.
	shopName := volumeRequestName(c.object(structs.OperatorsGroup, csiVolumeRequestKind, "shop-data"))
	must.NoError(t, writer.deleteWith(structs.OperatorsGroup, csiVolumeRequestKind, "vol", ""))
	must.NoError(t, writer.deleteWith(testOwnerGroup, testOwnerKind, "shop", ""))
	must.NoError(t, writer.deleteWith(testOwnerGroup, testOwnerKind, "app", structs.ObjectPropagationForeground))
	c.waitGone(structs.OperatorsGroup, volumeSnapshotScheduleKind, "nightly")
	c.waitGone(structs.OperatorsGroup, csiVolumeRequestKind, "shop-data")
	c.waitGone(structs.OperatorsGroup, csiVolumeRequestKind, "app-data")
	c.waitGone(testOwnerGroup, testOwnerKind, "app")
	must.Nil(t, c.volume("shop-data"))
	must.Nil(t, c.volume("app-data"))
	volumes.set(func(f *fakeVolumeCSI) { must.Eq(t, []string{"ext-" + shopName}, f.deletes) })
	must.Eq(t, []string{"snap-weekly-1", "snap-weekly-2"}, snapshots.existing())

	weekly := c.object(structs.OperatorsGroup, volumeSnapshotScheduleKind, "weekly")
	must.NotNil(t, weekly)
	must.False(t, weekly.Deleting())
}

// The check of a deletion sees the dependents that exist then. The garbage
// collector deletes a volume request or a snapshot schedule that destroys
// storage only if that check named it: one that was written, or made
// destructive, after the check stays, without its reference to the owner
// (review finding SEC-8).
func TestObjectGC_KeepsDependentsNobodyWasCheckedFor(t *testing.T) {
	ci.Parallel(t)
	c, volumes, _, cleanup := dependentsTestServer(t)
	defer cleanup()

	database := func(name string) *structs.ObjectOwnerReference {
		return ownedBy(testOwnerGroup, testOwnerKind, name)
	}
	finalizers := func(name string, add, remove []string) {
		t.Helper()
		var resp structs.CustomObjectWriteResponse
		must.NoError(t, c.s.RPC("CustomObject.UpdateFinalizers", &structs.CustomObjectFinalizersRequest{
			Group: testOwnerGroup, Kind: testOwnerKind, Name: name, Add: add, Remove: remove,
			WriteRequest: c.write("default"),
		}, &resp))
	}
	kept := func(name string) {
		t.Helper()
		var obj *structs.CustomObject
		must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
			obj = c.object(structs.OperatorsGroup, csiVolumeRequestKind, name)
			return obj == nil || obj.Deleting() || len(obj.Owners) == 0
		}), wait.Timeout(30*time.Second), wait.Gap(100*time.Millisecond)), must.Sprintf("%s is let go of", name))
		must.NotNil(t, obj, must.Sprintf("%s was deleted with its owner", name))
		must.False(t, obj.Deleting(), must.Sprintf("%s is marked for deletion", name))
		must.NotNil(t, c.volume(name))

		log, err := c.s.State().ObjectEventLog(nil, structs.OperatorsGroup, csiVolumeRequestKind, "default", name)
		must.NoError(t, err)
		must.NotNil(t, log)
		must.True(t, slices.ContainsFunc(log.Events, func(e *structs.ObjectEvent) bool {
			return e.Reason == objectEventDependentKept && e.Type == structs.ObjectEventTypeWarning
		}), must.Sprintf("%s has no event that says why it stays", name))
	}

	objects := mock.CreatePolicyAndToken(t, c.s.State(), 1001, "objects", `namespace "default" {
  object "example.com/Database" { capabilities = ["read", "write"] }
}`)
	noVolume := &certTest{t: t, s: c.s, token: objects.SecretID}

	// A request is written as the dependent of an owner that was deleted a
	// moment ago, when it had no dependent.
	must.NoError(t, c.applyOwned(testOwnerGroup, testOwnerKind, "early", map[string]any{}))
	must.NoError(t, noVolume.deleteWith(testOwnerGroup, testOwnerKind, "early", ""))
	c.waitGone(testOwnerGroup, testOwnerKind, "early")
	c.volumeRequestOwnedBy("late", "delete", database("early"))
	kept("late")

	// A request that retained its volume when its owner was deleted, and
	// does not when the owner goes: the owner waits for a finalizer.
	must.NoError(t, c.applyOwned(testOwnerGroup, testOwnerKind, "slow", map[string]any{}))
	finalizers("slow", []string{"example.com/hold"}, nil)
	c.volumeRequestOwnedBy("changed", "retain", database("slow"))
	must.NoError(t, noVolume.deleteWith(testOwnerGroup, testOwnerKind, "slow", ""))
	must.True(t, c.object(testOwnerGroup, testOwnerKind, "slow").Deleting())
	must.NoError(t, c.applyOwned(structs.OperatorsGroup, csiVolumeRequestKind, "changed",
		volumeSpec(func(m map[string]any) { m["deletion_policy"] = "delete" }), database("slow")))
	finalizers("slow", nil, []string{"example.com/hold"})
	c.waitGone(testOwnerGroup, testOwnerKind, "slow")
	kept("changed")

	// A dependent that the deleter of the owner was checked for goes, also
	// when the owner waited for a finalizer in between.
	must.NoError(t, c.applyOwned(testOwnerGroup, testOwnerKind, "checked", map[string]any{}))
	finalizers("checked", []string{"example.com/hold"}, nil)
	c.volumeRequestOwnedBy("named", "delete", database("checked"))
	namedName := volumeRequestName(c.object(structs.OperatorsGroup, csiVolumeRequestKind, "named"))
	must.NoError(t, c.deleteWith(testOwnerGroup, testOwnerKind, "checked", ""))
	marked := c.object(testOwnerGroup, testOwnerKind, "checked")
	must.Eq(t, []string{c.object(structs.OperatorsGroup, csiVolumeRequestKind, "named").ID}, marked.DeletionApproved)
	finalizers("checked", nil, []string{"example.com/hold"})
	c.waitGone(structs.OperatorsGroup, csiVolumeRequestKind, "named")
	must.Nil(t, c.volume("named"))

	volumes.set(func(f *fakeVolumeCSI) { must.Eq(t, []string{"ext-" + namedName}, f.deletes) })
}

// TestObjectDeletionDestroys checks which objects are guarded: the ones
// whose deletion takes more than the object with it.
func TestObjectDeletionDestroys(t *testing.T) {
	ci.Parallel(t)
	operators := func(kind string, spec map[string]any) *structs.CustomObject {
		return &structs.CustomObject{Group: structs.OperatorsGroup, Kind: kind, Name: "x", Spec: spec}
	}
	tests := []struct {
		name string
		obj  *structs.CustomObject
		want bool
	}{
		{"volume request that deletes its volume", operators(csiVolumeRequestKind, map[string]any{"deletion_policy": "delete"}), true},
		{"volume request that retains its volume", operators(csiVolumeRequestKind, map[string]any{"deletion_policy": "retain"}), false},
		{"snapshot schedule that deletes its snapshots", operators(volumeSnapshotScheduleKind, map[string]any{"delete_snapshots": true}), true},
		{"snapshot schedule that keeps its snapshots", operators(volumeSnapshotScheduleKind, map[string]any{}), false},
		{"tenant", operators(structs.TenantKind, map[string]any{}), true},
		{"certificate", operators(structs.CertificateKind, map[string]any{"issuer": "internal", "secret_path": "certs/x"}), true},
		{"certificate issuer", operators(structs.CertificateIssuerKind, map[string]any{"builtin": map[string]any{}}), false},
		{"certificate order", operators(structs.CertificateOrderKind, map[string]any{}), false},
		{"tenant of another group", &structs.CustomObject{Group: testOwnerGroup, Kind: structs.TenantKind, Name: "x"}, false},
		{"custom kind", &structs.CustomObject{Group: testOwnerGroup, Kind: testOwnerKind, Name: "x"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must.Eq(t, tt.want, objectDeletionDestroys(tt.obj))
		})
	}
}

// The deletion of a tenant takes down what the tenant keeps (its namespace,
// quota, ACL policies and roles), and the deletion of a certificate ends the
// certificate the servers keep issued. An owner of one is deleted with it
// only by a caller who may delete the tenant or the certificate itself, and
// the garbage collector takes only the ones the deleter was checked for
// (review finding JOG-22). Before the fix the right to write the kind of the
// owner was enough to have a tenant or a certificate collected.
func TestObjectDelete_DependentTenantAndCertificate(t *testing.T) {
	ci.Parallel(t)
	c, _, _, cleanup := dependentsTestServer(t)
	defer cleanup()
	c.s.objectGC.SetConfig(&objectgc.Config{Interval: 50 * time.Millisecond, IdleInterval: time.Second, TombstoneRetention: time.Second})

	const tenantRef, certRef = "norca.io/Tenant", "norca.io/Certificate"
	database := func(name string) *structs.ObjectOwnerReference {
		return &structs.ObjectOwnerReference{Kind: structs.ObjectKindRef(testOwnerGroup, testOwnerKind), Namespace: "default", Name: name}
	}
	tenant := func(name, owner string) {
		t.Helper()
		_, err := c.applyObject(&structs.CustomObject{Group: structs.OperatorsGroup, Kind: structs.TenantKind, Name: name,
			Spec: map[string]any{}, Owners: []*structs.ObjectOwnerReference{database(owner)}})
		must.NoError(t, err)
		waitTenant(c, name, "ready", tenantReady)
	}
	certificate := func(name, owner string) {
		t.Helper()
		must.NoError(t, c.applyOwned(structs.OperatorsGroup, structs.CertificateKind, name, map[string]any{
			"issuer": "internal", "dns_names": []any{name + ".example.com"}, "secret_path": "certs/" + name,
		}, database(owner)))
		c.waitFor("default", name, "issued", certReady)
	}

	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	_, err := c.apply("", structs.CertificateIssuerKind, "internal", map[string]any{"builtin": map[string]any{}})
	must.NoError(t, err)

	// Objects of a custom kind own a tenant and a certificate, one of them
	// through an object in between.
	for _, name := range []string{"org", "shop", "app", "loose", "early-org", "early-shop"} {
		must.NoError(t, c.applyOwned(testOwnerGroup, testOwnerKind, name, map[string]any{}))
	}
	must.NoError(t, c.applyOwned(testOwnerGroup, testOwnerKind, "app-part", map[string]any{}, database("app")))
	tenant("team-a", "org")
	certificate("shop-tls", "shop")
	certificate("app-tls", "app-part")
	certificate("loose-tls", "loose")

	const databases = `  object "example.com/Database" { capabilities = ["read", "write"] }
`
	noRight := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1001, "databases", `namespace "default" {
`+databases+`}`))
	reader := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1003, "reader", `namespace "default" {
`+databases+`  object "norca.io/Certificate" { capabilities = ["read"] }
}
namespace "*" {
  object "norca.io/Tenant" { capabilities = ["read"] }
}`))
	writer := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1005, "writer", `namespace "default" {
`+databases+`  object "norca.io/Certificate" { capabilities = ["write"] }
}
namespace "*" {
  object "norca.io/Tenant" { capabilities = ["write"] }
}`))

	// Without the right to delete the dependent itself: refused in both
	// modes that take the dependents, and nothing is marked.
	for _, mode := range []string{"", structs.ObjectPropagationBackground, structs.ObjectPropagationForeground} {
		for _, tt := range []struct{ owner, kind, dependent string }{
			{"org", tenantRef, "team-a"},
			{"shop", certRef, "shop-tls"},
			{"app", certRef, "app-tls"},
		} {
			err := noRight.deleteWith(testOwnerGroup, testOwnerKind, tt.owner, mode)
			must.Error(t, err, must.Sprintf("%s was deleted without the right on its dependent %s %q, mode %q",
				tt.owner, tt.kind, tt.dependent, mode))
			must.Eq(t, 403, errCode(t, err), must.Sprintf("%s, mode %q", tt.owner, mode))
			must.ErrorContains(t, err, "the object capability write on the kind "+tt.kind)
			// The caller may not read the dependent: it is not named.
			must.StrNotContains(t, err.Error(), tt.dependent)

			err = reader.deleteWith(testOwnerGroup, testOwnerKind, tt.owner, mode)
			must.Eq(t, 403, errCode(t, err), must.Sprintf("%s, mode %q", tt.owner, mode))
			must.ErrorContains(t, err, fmt.Sprintf("its dependent %s %q", tt.kind, tt.dependent))
		}
	}
	var resp structs.CustomObjectWriteResponse
	err = c.s.RPC("CustomObject.Delete", &structs.CustomObjectDeleteRequest{
		Group: testOwnerGroup, Kind: testOwnerKind, Name: "org", DryRun: true,
		WriteRequest: noRight.write("default"),
	}, &resp)
	must.Eq(t, 403, errCode(t, err))
	for _, obj := range []*structs.CustomObject{
		c.object(testOwnerGroup, testOwnerKind, "org"),
		c.get("", structs.TenantKind, "team-a"),
		c.object(testOwnerGroup, testOwnerKind, "shop"),
		c.get("default", structs.CertificateKind, "shop-tls"),
		c.object(testOwnerGroup, testOwnerKind, "app"),
		c.object(testOwnerGroup, testOwnerKind, "app-part"),
		c.get("default", structs.CertificateKind, "app-tls"),
	} {
		must.NotNil(t, obj)
		must.False(t, obj.Deleting(), must.Sprintf("%s is marked for deletion", obj.Name))
	}

	// The orphan mode takes no dependent: the right on the owner is enough,
	// and the certificate stays as it is.
	loose := c.get("default", structs.CertificateKind, "loose-tls")
	must.NoError(t, noRight.deleteWith(testOwnerGroup, testOwnerKind, "loose", structs.ObjectPropagationOrphan))
	c.waitGone(testOwnerGroup, testOwnerKind, "loose")

	// A tenant and a certificate that are written as the dependents of an
	// owner after the owner was deleted: nobody was checked for them, so
	// the collector lets go of them and says why.
	must.NoError(t, noRight.deleteWith(testOwnerGroup, testOwnerKind, "early-org", ""))
	must.NoError(t, noRight.deleteWith(testOwnerGroup, testOwnerKind, "early-shop", ""))
	c.waitGone(testOwnerGroup, testOwnerKind, "early-org")
	c.waitGone(testOwnerGroup, testOwnerKind, "early-shop")
	tenant("team-late", "early-org")
	certificate("late-tls", "early-shop")
	for _, late := range []struct{ namespace, kind, name, takes string }{
		{"", structs.TenantKind, "team-late", "what the tenant keeps"},
		{"default", structs.CertificateKind, "late-tls", "the certificate the servers keep issued"},
	} {
		var obj *structs.CustomObject
		must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
			obj = c.get(late.namespace, late.kind, late.name)
			return obj == nil || obj.Deleting() || len(obj.Owners) == 0
		}), wait.Timeout(30*time.Second), wait.Gap(100*time.Millisecond)), must.Sprintf("%s is let go of", late.name))
		must.NotNil(t, obj, must.Sprintf("%s was deleted with its owner", late.name))
		must.False(t, obj.Deleting(), must.Sprintf("%s is marked for deletion", late.name))
		must.SliceContains(t, c.eventReasons(late.namespace, late.kind, late.name), objectEventDependentKept)
		messages := c.eventMessages(late.namespace, late.kind, late.name, objectEventDependentKept)
		must.Len(t, 1, messages)
		must.StrContains(t, messages[0], late.takes)
	}
	ns, err := c.s.State().NamespaceByName(nil, "team-late")
	must.NoError(t, err)
	must.NotNil(t, ns)

	// With the right on the dependents the same deletions go through, the
	// dependents are recorded with the deletion, and they go with their
	// owners: the namespace of the tenant too.
	teamA := c.get("", structs.TenantKind, "team-a")
	reply, err := writer.deleteReply(testOwnerGroup, testOwnerKind, "org", structs.ObjectPropagationForeground, false)
	must.NoError(t, err)
	must.Eq(t, []string{teamA.ID}, reply.Object.DeletionApproved)
	must.NoError(t, writer.deleteWith(testOwnerGroup, testOwnerKind, "shop", ""))
	must.NoError(t, writer.deleteWith(testOwnerGroup, testOwnerKind, "app", ""))
	waitTenantGone(c, "team-a")
	c.waitGone(testOwnerGroup, testOwnerKind, "org")
	c.waitGone(structs.OperatorsGroup, structs.CertificateKind, "shop-tls")
	c.waitGone(structs.OperatorsGroup, structs.CertificateKind, "app-tls")
	ns, err = c.s.State().NamespaceByName(nil, "team-a")
	must.NoError(t, err)
	must.Nil(t, ns)

	// What was let go of, or kept by the orphan mode, is still there.
	after := c.get("default", structs.CertificateKind, "loose-tls")
	must.NotNil(t, after)
	must.False(t, after.Deleting())
	must.Eq(t, loose.Owners, after.Owners)
	for _, kept := range []*structs.CustomObject{
		c.get("", structs.TenantKind, "team-late"),
		c.get("default", structs.CertificateKind, "late-tls"),
	} {
		must.NotNil(t, kept)
		must.False(t, kept.Deleting())
	}
}

// A tenant, a certificate, or a volume request that takes its volume away is
// deleted with its owner only for a caller who may delete it itself: the
// protection of a managed object (managed_by) is asked as a direct deletion
// asks it (review finding JOG-25). Before the fix the right to write the kind
// of a managed tenant was enough to have it collected through an owner,
// though the direct deletion of the tenant was refused.
func TestObjectDelete_DependentManagedByAnother(t *testing.T) {
	ci.Parallel(t)
	c, _, _, cleanup := dependentsTestServer(t)
	defer cleanup()
	c.s.objectGC.SetConfig(&objectgc.Config{Interval: 50 * time.Millisecond, IdleInterval: time.Second, TombstoneRetention: time.Second})

	const tenantRef, requestRef = "norca.io/Tenant", "norca.io/CSIVolumeRequest"
	const managedBy = structs.ManagedByPolicyPrefix + "platform-controller"
	database := func(name string) *structs.ObjectOwnerReference {
		return &structs.ObjectOwnerReference{Kind: structs.ObjectKindRef(testOwnerGroup, testOwnerKind), Namespace: "default", Name: name}
	}
	tenant := func(name, owner string) {
		t.Helper()
		_, err := c.applyObject(&structs.CustomObject{Group: structs.OperatorsGroup, Kind: structs.TenantKind, Name: name,
			Spec: map[string]any{}, ManagedBy: managedBy, Owners: []*structs.ObjectOwnerReference{database(owner)}})
		must.NoError(t, err)
		waitTenant(c, name, "ready", tenantReady)
	}
	request := func(name, owner string) {
		t.Helper()
		_, err := c.applyObject(&structs.CustomObject{Group: structs.OperatorsGroup, Kind: csiVolumeRequestKind, Name: name,
			Spec:      volumeSpec(func(m map[string]any) { m["deletion_policy"] = "delete" }),
			ManagedBy: managedBy, Owners: []*structs.ObjectOwnerReference{database(owner)}})
		must.NoError(t, err)
		c.waitRequest(name, "created", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
			return readyReason(obj) == "True" && slices.Contains(obj.Finalizers, structs.OperatorsFinalizer)
		})
	}

	for _, name := range []string{"org", "shop", "loose"} {
		must.NoError(t, c.applyOwned(testOwnerGroup, testOwnerKind, name, map[string]any{}))
	}
	tenant("team-m", "org")
	request("shop-data", "shop")
	tenant("team-loose", "loose")

	// Every right a deletion of the dependents asks, with and without the
	// right to read them; the last token is the manager.
	rules := func(dependents string) string {
		return `namespace "default" {
  object "example.com/Database" { capabilities = ["read", "write"] }
  object "norca.io/CSIVolumeRequest" { capabilities = [` + dependents + `] }
  capabilities = ["csi-write-volume"]
}
namespace "*" {
  object "norca.io/Tenant" { capabilities = [` + dependents + `] }
}
plugin { policy = "read" }`
	}
	writer := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1001, "writer", rules(`"write"`)))
	reader := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1003, "reader", rules(`"read", "write"`)))
	manager := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1005, "platform-controller", rules(`"write"`)))

	// The direct deletion of a managed object is refused to all but its
	// manager.
	err := writer.delete("", structs.TenantKind, "team-m")
	must.Eq(t, 403, errCode(t, err))
	must.ErrorContains(t, err, "is managed by")
	err = writer.delete("default", csiVolumeRequestKind, "shop-data")
	must.Eq(t, 403, errCode(t, err))
	must.ErrorContains(t, err, "is managed by")

	// So is the deletion of an owner of it, in both modes that take the
	// dependents, and nothing is marked.
	for _, mode := range []string{"", structs.ObjectPropagationBackground, structs.ObjectPropagationForeground} {
		for _, tt := range []struct{ owner, kind, dependent string }{
			{"org", tenantRef, "team-m"},
			{"shop", requestRef, "shop-data"},
		} {
			err := writer.deleteWith(testOwnerGroup, testOwnerKind, tt.owner, mode)
			must.Error(t, err, must.Sprintf("%s was deleted by a caller who is not the manager of its dependent %s %q, mode %q",
				tt.owner, tt.kind, tt.dependent, mode))
			must.Eq(t, 403, errCode(t, err), must.Sprintf("%s, mode %q", tt.owner, mode))
			must.ErrorContains(t, err, "is managed by another identity")
			must.ErrorContains(t, err, `the propagation mode "orphan" deletes the object and keeps its dependents`)
			// The caller may not read the dependent: neither it nor its
			// manager is named.
			must.StrNotContains(t, err.Error(), tt.dependent)
			must.StrNotContains(t, err.Error(), "platform-controller")

			err = reader.deleteWith(testOwnerGroup, testOwnerKind, tt.owner, mode)
			must.Eq(t, 403, errCode(t, err), must.Sprintf("%s, mode %q", tt.owner, mode))
			must.ErrorContains(t, err, fmt.Sprintf("its dependent %s %q", tt.kind, tt.dependent))
			must.ErrorContains(t, err, fmt.Sprintf("is managed by %q", managedBy))
		}
	}
	var resp structs.CustomObjectWriteResponse
	err = c.s.RPC("CustomObject.Delete", &structs.CustomObjectDeleteRequest{
		Group: testOwnerGroup, Kind: testOwnerKind, Name: "org", DryRun: true,
		WriteRequest: writer.write("default"),
	}, &resp)
	must.Eq(t, 403, errCode(t, err))
	for _, obj := range []*structs.CustomObject{
		c.object(testOwnerGroup, testOwnerKind, "org"),
		c.get("", structs.TenantKind, "team-m"),
		c.object(testOwnerGroup, testOwnerKind, "shop"),
		c.get("default", csiVolumeRequestKind, "shop-data"),
	} {
		must.NotNil(t, obj)
		must.False(t, obj.Deleting(), must.Sprintf("%s is marked for deletion", obj.Name))
	}

	// The orphan mode takes no dependent, and asks nothing.
	must.NoError(t, writer.deleteWith(testOwnerGroup, testOwnerKind, "loose", structs.ObjectPropagationOrphan))
	c.waitGone(testOwnerGroup, testOwnerKind, "loose")

	// The manager deletes the owners, and the dependents go with them.
	teamM := c.get("", structs.TenantKind, "team-m")
	reply, err := manager.deleteReply(testOwnerGroup, testOwnerKind, "org", structs.ObjectPropagationForeground, false)
	must.NoError(t, err)
	must.Eq(t, []string{teamM.ID}, reply.Object.DeletionApproved)
	must.NoError(t, manager.deleteWith(testOwnerGroup, testOwnerKind, "shop", ""))
	waitTenantGone(c, "team-m")
	c.waitGone(testOwnerGroup, testOwnerKind, "org")
	c.waitGone(structs.OperatorsGroup, csiVolumeRequestKind, "shop-data")

	kept := c.get("", structs.TenantKind, "team-loose")
	must.NotNil(t, kept)
	must.False(t, kept.Deleting())
}

// ownedJob registers a job of the default namespace whose meta names an
// object of the default namespace as its owner, as the controller SDK
// writes it, managed by its controller.
func (c *certTest) ownedJob(id, ownerName string) *structs.Job {
	c.t.Helper()
	job := mock.Job()
	job.ID, job.Name = id, id
	job.Meta = map[string]string{
		structs.JobMetaOwner:     structs.ObjectOwnerMetaValue(testOwnerGroup, testOwnerKind, "default", ownerName),
		structs.JobMetaOwnerKind: structs.ObjectKindRef(testOwnerGroup, testOwnerKind),
		structs.JobMetaManagedBy: structs.ManagedByPolicyPrefix + "database-controller",
	}
	var resp structs.JobRegisterResponse
	must.NoError(c.t, c.s.RPC("Job.Register", &structs.JobRegisterRequest{Job: job, WriteRequest: c.write("default")}, &resp))
	return c.job(id)
}

func (c *certTest) job(id string) *structs.Job {
	c.t.Helper()
	job, err := c.s.State().JobByID(nil, "default", id)
	must.NoError(c.t, err)
	must.NotNil(c.t, job)
	return job
}

func (c *certTest) waitJobStopped(id string) {
	c.t.Helper()
	must.Wait(c.t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		return c.job(id).Stop
	}), wait.Timeout(30*time.Second), wait.Gap(100*time.Millisecond)), must.Sprintf("the job %s is stopped", id))
}

// The garbage collector stops the jobs that depend on a deleted object, with
// the token of the leader. Deleting an object in a mode that takes its
// dependents needs the right to stop those jobs, as stopping them does, and
// the collector stops only the jobs the deleter was checked for.
func TestObjectDelete_DependentJobs(t *testing.T) {
	ci.Parallel(t)
	c, _, _, cleanup := dependentsTestServer(t)
	defer cleanup()

	database := func(name string) *structs.ObjectOwnerReference {
		return ownedBy(testOwnerGroup, testOwnerKind, name)
	}
	for _, name := range []string{"web", "site", "loose", "early"} {
		must.NoError(t, c.applyOwned(testOwnerGroup, testOwnerKind, name, map[string]any{}))
	}
	must.NoError(t, c.applyOwned(testOwnerGroup, testOwnerKind, "site-part", map[string]any{}, database("site")))
	c.ownedJob("web-proxy", "web")
	c.ownedJob("site-worker", "site-part")
	c.ownedJob("loose-job", "loose")

	objects := mock.CreatePolicyAndToken(t, c.s.State(), 1001, "objects", `namespace "default" {
  object "example.com/Database" { capabilities = ["read", "write"] }
}`)
	reader := mock.CreatePolicyAndToken(t, c.s.State(), 1003, "reader", `namespace "default" {
  object "example.com/Database" { capabilities = ["read", "write"] }
  capabilities = ["list-jobs", "read-job"]
}`)
	stopper := mock.CreatePolicyAndToken(t, c.s.State(), 1005, "stopper", `namespace "default" {
  object "example.com/Database" { capabilities = ["read", "write"] }
  capabilities = ["submit-job"]
}`)
	noJob := &certTest{t: t, s: c.s, token: objects.SecretID}
	jobReader := &certTest{t: t, s: c.s, token: reader.SecretID}
	writer := &certTest{t: t, s: c.s, token: stopper.SecretID}

	// Without the right to stop the jobs: refused in the modes that take
	// the dependents, directly and through an object in between.
	for _, mode := range []string{"", structs.ObjectPropagationBackground, structs.ObjectPropagationForeground} {
		for _, name := range []string{"web", "site"} {
			err := noJob.deleteWith(testOwnerGroup, testOwnerKind, name, mode)
			must.Eq(t, 403, errCode(t, err), must.Sprintf("%s, mode %q", name, mode))
			must.ErrorContains(t, err, "submit-job")
			// The caller may not read jobs: the job is not named.
			must.StrNotContains(t, err.Error(), "web-proxy")
			must.StrNotContains(t, err.Error(), "site-worker")
		}
		err := jobReader.deleteWith(testOwnerGroup, testOwnerKind, "web", mode)
		must.Eq(t, 403, errCode(t, err), must.Sprintf("mode %q", mode))
		must.ErrorContains(t, err, `job "web-proxy"`)
		err = jobReader.deleteWith(testOwnerGroup, testOwnerKind, "site", mode)
		must.Eq(t, 403, errCode(t, err), must.Sprintf("mode %q", mode))
		must.ErrorContains(t, err, `job "site-worker"`)
	}
	var resp structs.CustomObjectWriteResponse
	err := c.s.RPC("CustomObject.Delete", &structs.CustomObjectDeleteRequest{
		Group: testOwnerGroup, Kind: testOwnerKind, Name: "web", DryRun: true,
		WriteRequest: noJob.write("default"),
	}, &resp)
	must.Eq(t, 403, errCode(t, err))
	for _, name := range []string{"web", "site", "site-part"} {
		obj := c.object(testOwnerGroup, testOwnerKind, name)
		must.NotNil(t, obj)
		must.False(t, obj.Deleting(), must.Sprintf("%s is marked for deletion", name))
	}
	must.False(t, c.job("web-proxy").Stop)
	must.False(t, c.job("site-worker").Stop)

	// The orphan mode takes no dependent: the object right is enough, and
	// the job keeps running.
	must.NoError(t, noJob.deleteWith(testOwnerGroup, testOwnerKind, "loose", structs.ObjectPropagationOrphan))
	c.waitGone(testOwnerGroup, testOwnerKind, "loose")

	// With the right to stop jobs the same deletions go through and the
	// jobs stop. The jobs are managed by their controller: nobody else may
	// stop them directly, and deleting their owner is the way to do it.
	must.NoError(t, writer.deleteWith(testOwnerGroup, testOwnerKind, "web", ""))
	must.NoError(t, writer.deleteWith(testOwnerGroup, testOwnerKind, "site", ""))
	c.waitJobStopped("web-proxy")
	c.waitJobStopped("site-worker")
	c.waitGone(testOwnerGroup, testOwnerKind, "site-part")

	// A job that is attached to an owner after the owner was deleted is
	// not stopped: nobody was checked for it. The object written with it
	// goes in the same pass of the collector, which shows the pass ran.
	must.NoError(t, writer.deleteWith(testOwnerGroup, testOwnerKind, "early", ""))
	c.waitGone(testOwnerGroup, testOwnerKind, "early")
	c.ownedJob("late-job", "early")
	must.NoError(t, c.applyOwned(testOwnerGroup, testOwnerKind, "late-part", map[string]any{}, database("early")))
	c.waitGone(testOwnerGroup, testOwnerKind, "late-part")
	must.False(t, c.job("late-job").Stop, must.Sprint("a job attached after the deletion of its owner was stopped"))
	must.False(t, c.job("loose-job").Stop, must.Sprint("the job of an owner deleted in the orphan mode was stopped"))
}

// dependentsTestStore is a state store with the kind example.com/Database
// and the objects and jobs of the dependents tests below. index is the last
// Raft index used.
type dependentsTestStore struct {
	tb    testing.TB
	store *state.StateStore
	index uint64
}

func newDependentsTestStore(tb testing.TB) *dependentsTestStore {
	tb.Helper()
	d := &dependentsTestStore{tb: tb, store: state.TestStateStore(tb), index: 1000}
	must.NoError(tb, d.store.UpsertObjectKind(structs.MsgTypeTestSetup, d.next(),
		&structs.ObjectKind{Group: testOwnerGroup, Kind: testOwnerKind, Namespaced: true, Schema: `{"type": "object"}`}))
	return d
}

func (d *dependentsTestStore) next() uint64 {
	d.index++
	return d.index
}

// object writes an object of the default namespace with its owners.
func (d *dependentsTestStore) object(name string, owners ...string) *structs.CustomObject {
	d.tb.Helper()
	obj := &structs.CustomObject{ID: uuid.Generate(), Group: testOwnerGroup, Kind: testOwnerKind,
		Namespace: "default", Name: name, Generation: 1}
	for _, owner := range owners {
		obj.Owners = append(obj.Owners, &structs.ObjectOwnerReference{
			Kind: structs.ObjectKindRef(testOwnerGroup, testOwnerKind), Namespace: "default", Name: owner})
	}
	must.NoError(d.tb, d.store.UpsertCustomObject(structs.MsgTypeTestSetup, d.next(), obj, 0))
	return obj
}

// job writes a job of the default namespace owned by the named object, with
// the kind of the owner in its meta.
func (d *dependentsTestStore) job(id, owner, ownerKind string, stop bool) *structs.Job {
	d.tb.Helper()
	job := mock.Job()
	job.ID, job.Name, job.Stop = id, id, stop
	job.Meta = map[string]string{
		structs.JobMetaOwner:     structs.ObjectOwnerMetaValue(testOwnerGroup, testOwnerKind, "default", owner),
		structs.JobMetaOwnerKind: ownerKind,
	}
	must.NoError(d.tb, d.store.UpsertJob(structs.MsgTypeTestSetup, d.next(), nil, job))
	stored, err := d.store.JobByID(nil, job.Namespace, id)
	must.NoError(d.tb, err)
	return stored
}

// TestCheckObjectDependentsDelete_Jobs checks which jobs the deletion of an
// owner is checked for: the jobs that name the owner, and the jobs of the
// objects that depend on it directly or through other objects; not stopped
// jobs, jobs of other objects, or jobs whose owner kind is another one.
func TestCheckObjectDependentsDelete_Jobs(t *testing.T) {
	ci.Parallel(t)
	d := newDependentsTestStore(t)
	kindRef := structs.ObjectKindRef(testOwnerGroup, testOwnerKind)

	web := d.object("web")
	d.object("web-part", "web")
	d.object("web-part-db", "web-part")
	d.object("shared", "other", "web-part-db")
	d.object("other")
	d.object("other-part", "other")
	// A cycle below the owner ends.
	d.object("loop-a", "web-part", "loop-b")
	d.object("loop-b", "loop-a")

	want := []*structs.Job{
		d.job("direct", "web", kindRef, false),
		d.job("via-part", "web-part", kindRef, false),
		d.job("via-grand", "web-part-db", kindRef, false),
		d.job("via-shared", "shared", kindRef, false),
		d.job("via-loop", "loop-b", kindRef, false),
	}
	d.job("stopped", "web-part", kindRef, true)
	d.job("unrelated", "other-part", kindRef, false)
	d.job("wrong-kind", "web", "example.com/Other", false)
	plain := mock.Job()
	must.NoError(t, d.store.UpsertJob(structs.MsgTypeTestSetup, d.next(), nil, plain))

	snap, err := d.store.Snapshot()
	must.NoError(t, err)
	approved, err := checkObjectDependentsDelete(snap, nil, nil, web)
	must.NoError(t, err)
	var expected []string
	for _, job := range want {
		expected = append(expected, structs.ObjectApprovedJob(job.Namespace, job.ID, job.CreateIndex))
	}
	slices.Sort(expected)
	must.Eq(t, expected, approved)

	// Another owner is checked for its own job only.
	other, err := snap.CustomObjectByName(nil, testOwnerGroup, testOwnerKind, "default", "other-part")
	must.NoError(t, err)
	approved, err = checkObjectDependentsDelete(snap, nil, nil, other)
	must.NoError(t, err)
	must.Eq(t, []string{structs.ObjectApprovedJob("default", "unrelated", d.mustJob("unrelated").CreateIndex)}, approved)
}

func (d *dependentsTestStore) mustJob(id string) *structs.Job {
	d.tb.Helper()
	job, err := d.store.JobByID(nil, "default", id)
	must.NoError(d.tb, err)
	must.NotNil(d.tb, job)
	return job
}

// BenchmarkCheckObjectDependentsDelete measures the check of a deletion of
// an owner with one dependent object and two jobs, in a store with many
// other owned objects and jobs (review finding MUK-9).
func BenchmarkCheckObjectDependentsDelete(b *testing.B) {
	for _, n := range []int{1000, 10000} {
		b.Run(fmt.Sprintf("others=%d", n), func(b *testing.B) {
			d := newDependentsTestStore(b)
			kindRef := structs.ObjectKindRef(testOwnerGroup, testOwnerKind)
			for i := range n {
				name := fmt.Sprintf("db-%d", i)
				d.object(name)
				d.object(name+"-part", name)
				d.job("job-"+name, name+"-part", kindRef, false)
			}
			web := d.object("web")
			d.object("web-part", "web")
			d.job("direct", "web", kindRef, false)
			d.job("via-part", "web-part", kindRef, false)
			snap, err := d.store.Snapshot()
			must.NoError(b, err)

			for b.Loop() {
				approved, err := checkObjectDependentsDelete(snap, nil, nil, web)
				if err != nil || len(approved) != 2 {
					b.Fatalf("approved %v, error %v", approved, err)
				}
			}
		})
	}
}
