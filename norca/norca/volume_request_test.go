// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/plugins/csi"
	"github.com/hashicorp/go-memdb"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

const gibibyte = int64(1) << 30

// fakeVolumeCSI is a controller plugin of a test.
type fakeVolumeCSI struct {
	mu        sync.Mutex
	creates   []string
	validates []string
	expands   []int64
	deletes   []string
	createErr error
	expandErr error

	// onExpand, when set, runs once in the next expansion, as something
	// that happens while the plugin works.
	onExpand func()
}

func (f *fakeVolumeCSI) create(vol *structs.CSIVolume, _ *structs.CSIPlugin) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, vol.Name)
	if f.createErr != nil {
		return f.createErr
	}
	vol.ExternalID = "ext-" + vol.Name
	vol.Capacity = max(vol.RequestedCapacityMin, gibibyte)
	vol.Context = map[string]string{"made-by": "plugin"}
	return nil
}

func (f *fakeVolumeCSI) validate(vol *structs.CSIVolume, _ *structs.CSIPlugin) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.validates = append(f.validates, vol.ExternalID)
	return nil
}

func (f *fakeVolumeCSI) expand(vol *structs.CSIVolume, _ *structs.CSIPlugin, capacity *csi.CapacityRange) error {
	f.mu.Lock()
	during := f.onExpand
	f.onExpand = nil
	f.mu.Unlock()
	if during != nil {
		during()
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.expandErr != nil {
		return f.expandErr
	}
	if capacity.RequiredBytes > vol.Capacity {
		f.expands = append(f.expands, capacity.RequiredBytes)
		vol.Capacity = capacity.RequiredBytes
	}
	return nil
}

func (f *fakeVolumeCSI) delete(vol *structs.CSIVolume, _ *structs.CSIPlugin) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, vol.ExternalID)
	return nil
}

func (f *fakeVolumeCSI) set(change func(f *fakeVolumeCSI)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

// volumeRequestTestServer starts a server whose operator of volume requests
// uses the fake plugin, with a healthy plugin "minnie" that can create and
// expand volumes.
func volumeRequestTestServer(t *testing.T, acl bool, cbs ...func(*Config)) (*certTest, *fakeVolumeCSI, func()) {
	t.Helper()
	fake := &fakeVolumeCSI{}
	s, root, cleanup := secretTestServer(t, acl, func(c *Config) {
		c.volumeCSI = fake
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
				SupportsCreateDelete: true,
				SupportsExpand:       true,
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

	waitOperatorKinds(t, s)

	c := &certTest{t: t, s: s}
	if root != nil {
		c.token = root.SecretID
	}
	return c, fake, cleanup
}

// request returns a volume request and its decoded status.
func (c *certTest) request(name string) (*structs.CustomObject, *volumeRequestStatus) {
	c.t.Helper()
	obj := c.get("default", csiVolumeRequestKind, name)
	if obj == nil {
		return nil, nil
	}
	st := &volumeRequestStatus{}
	must.NoError(c.t, structs.DecodeObjectPart(obj.Status, st))
	return obj, st
}

func (c *certTest) waitRequest(name, what string, cond func(*structs.CustomObject, *volumeRequestStatus) bool) *volumeRequestStatus {
	c.t.Helper()
	var st *volumeRequestStatus
	must.Wait(c.t, wait.InitialSuccess(
		wait.BoolFunc(func() bool {
			var obj *structs.CustomObject
			obj, st = c.request(name)
			return obj != nil && cond(obj, st)
		}),
		wait.Timeout(30*time.Second),
		wait.Gap(100*time.Millisecond),
	), must.Sprintf("volume request %s: %s", name, what))
	return st
}

// volumeName returns the name the volume of a request in the namespace
// default gets when the request gives none.
func (c *certTest) volumeName(request string) string {
	c.t.Helper()
	obj := c.get("default", csiVolumeRequestKind, request)
	must.NotNil(c.t, obj)
	return volumeRequestName(obj)
}

func (c *certTest) volume(id string) *structs.CSIVolume {
	c.t.Helper()
	vol, err := c.s.State().CSIVolumeByID(nil, "default", id)
	must.NoError(c.t, err)
	return vol
}

// writeVolume writes a volume to the state as a volume register does.
func (c *certTest) writeVolume(vol *structs.CSIVolume) {
	c.t.Helper()
	_, _, err := c.s.raftApply(structs.CSIVolumeRegisterRequestType, &structs.CSIVolumeRegisterRequest{
		Volumes:      []*structs.CSIVolume{vol},
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default"},
	})
	must.NoError(c.t, err)
}

func readyReason(obj *structs.CustomObject) string {
	for _, cond := range operatorConditions(obj.Status) {
		if cond.Type == structs.CertificateConditionReady {
			if cond.Status == "True" {
				return "True"
			}
			return cond.Reason
		}
	}
	return ""
}

func volumeSpec(change func(map[string]any)) map[string]any {
	spec := map[string]any{
		"plugin_id":    "minnie",
		"capacity_min": "1GiB",
		"capability":   map[string]any{"access_mode": "single-node-writer", "attachment_mode": "file-system"},
	}
	if change != nil {
		change(spec)
	}
	return spec
}

func TestVolumeRequest_CreateExpandAndDrift(t *testing.T) {
	ci.Parallel(t)
	c, fake, cleanup := volumeRequestTestServer(t, false)
	defer cleanup()

	var resp structs.SecretWriteResponse
	must.NoError(t, c.s.RPC(structs.SecretUpsertRPCMethod, secretWriteReq("csi/minnie", "", structs.SecretItems{"token": "s3cr3t"}), &resp))

	_, err := c.apply("default", csiVolumeRequestKind, "db", volumeSpec(func(m map[string]any) {
		m["mount_options"] = map[string]any{"fs_type": "ext4"}
		m["secret_path"] = "csi/minnie"
	}))
	must.NoError(t, err)
	st := c.waitRequest("db", "created", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return readyReason(obj) == "True" && st.VolumeCreateIndex > 0
	})
	dbName := c.volumeName("db")
	must.Eq(t, "ext-"+dbName, st.ExternalID)
	must.Eq(t, gibibyte, st.CapacityBytes)
	must.True(t, st.Schedulable)
	vol := c.volume("db")
	must.Eq(t, st.VolumeCreateIndex, vol.CreateIndex)
	must.Eq(t, "ext4", vol.MountOptions.FSType)
	must.Eq(t, "s3cr3t", vol.Secrets["token"])
	must.Eq(t, "plugin", vol.Context["made-by"])
	obj, _ := c.request("db")
	must.SliceContains(t, obj.Finalizers, structs.OperatorsFinalizer)

	// A raised capacity is expanded.
	_, err = c.apply("default", csiVolumeRequestKind, "db", volumeSpec(func(m map[string]any) {
		m["capacity_min"] = "2GiB"
		m["mount_options"] = map[string]any{"fs_type": "ext4"}
		m["secret_path"] = "csi/minnie"
	}))
	must.NoError(t, err)
	c.waitRequest("db", "expanded", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return st.CapacityBytes == 2*gibibyte && readyReason(obj) == "True"
	})
	must.Eq(t, 2*gibibyte, c.volume("db").RequestedCapacityMin)
	must.Zero(t, c.waitRequest("db", "no drift", func(*structs.CustomObject, *volumeRequestStatus) bool { return true }).Drifts)

	// Changed by hand through the volume API: set back.
	changed := c.volume("db").Copy()
	changed.MountOptions = &structs.CSIMountOptions{FSType: "xfs"}
	changed.RequestedCapabilities = append(changed.RequestedCapabilities, &structs.CSIVolumeCapability{
		AccessMode: structs.CSIVolumeAccessModeMultiNodeReader, AttachmentMode: structs.CSIVolumeAttachmentModeFilesystem})
	c.writeVolume(changed)
	st = c.waitRequest("db", "drift set back", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return st.Drifts == 1 && st.LastDrift != ""
	})
	vol = c.volume("db")
	must.Eq(t, "ext4", vol.MountOptions.FSType)
	must.Len(t, 1, vol.RequestedCapabilities)
	must.Eq(t, st.VolumeCreateIndex, vol.CreateIndex)
	fake.set(func(f *fakeVolumeCSI) {
		must.Eq(t, []string{dbName}, f.creates)
		must.Eq(t, []int64{2 * gibibyte}, f.expands)
	})

	// A created volume that is gone is not created again.
	_, _, err = c.s.raftApply(structs.CSIVolumeDeregisterRequestType, &structs.CSIVolumeDeregisterRequest{
		VolumeIDs: []string{"db"}, WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default"},
	})
	must.NoError(t, err)
	c.waitRequest("db", "volume lost", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return readyReason(obj) == "VolumeLost"
	})
	time.Sleep(500 * time.Millisecond)
	must.Nil(t, c.volume("db"))
	fake.set(func(f *fakeVolumeCSI) { must.Len(t, 1, f.creates) })
}

func TestVolumeRequest_ConflictAdoptAndRegister(t *testing.T) {
	ci.Parallel(t)
	c, fake, cleanup := volumeRequestTestServer(t, false)
	defer cleanup()

	c.writeVolume(&structs.CSIVolume{
		ID: "shared", Name: "shared", Namespace: "default", PluginID: "minnie", ExternalID: "ext-old",
		Capacity: gibibyte, RequestedCapacityMin: gibibyte,
		RequestedCapabilities: []*structs.CSIVolumeCapability{{
			AccessMode: structs.CSIVolumeAccessModeSingleNodeWriter, AttachmentMode: structs.CSIVolumeAttachmentModeFilesystem}},
	})
	existing := c.volume("shared")

	// Not its own: left alone.
	_, err := c.apply("default", csiVolumeRequestKind, "shared", volumeSpec(nil))
	must.NoError(t, err)
	c.waitRequest("shared", "conflict", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return readyReason(obj) == volumeRequestConditionConflict && objectConditionTrue(obj.Status, volumeRequestConditionConflict)
	})
	must.Eq(t, existing.ModifyIndex, c.volume("shared").ModifyIndex)

	// Adopted on request.
	_, err = c.apply("default", csiVolumeRequestKind, "shared", volumeSpec(func(m map[string]any) { m["adopt"] = true }))
	must.NoError(t, err)
	st := c.waitRequest("shared", "adopted", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return readyReason(obj) == "True" && !objectConditionTrue(obj.Status, volumeRequestConditionConflict)
	})
	must.Eq(t, existing.CreateIndex, st.VolumeCreateIndex)
	must.Eq(t, "ext-old", st.ExternalID)

	// A volume that appears while the request was creating it is its own.
	_, err = c.apply("default", csiVolumeRequestKind, "racing", volumeSpec(nil))
	must.NoError(t, err)
	c.waitRequest("racing", "created", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return readyReason(obj) == "True"
	})
	obj, st := c.request("racing")
	st.VolumeCreateIndex, st.Creating = 0, true
	encoded, err := structs.EncodeObjectPart(st)
	must.NoError(t, err)
	lost := obj.Copy()
	lost.Status = encoded
	_, err = c.s.writeOperatorObject(lost)
	must.NoError(t, err)
	st = c.waitRequest("racing", "own volume found again", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return st.VolumeCreateIndex != 0 && !st.Creating && readyReason(obj) == "True"
	})
	must.Eq(t, c.volume("racing").CreateIndex, st.VolumeCreateIndex)

	// Mode register: the volume of the storage is checked and registered.
	_, err = c.apply("default", csiVolumeRequestKind, "outside", volumeSpec(func(m map[string]any) {
		m["mode"] = "register"
		m["external_id"] = "vol-0abc"
		m["context"] = map[string]any{"zone": "z1"}
		delete(m, "capacity_min")
	}))
	must.NoError(t, err)
	c.waitRequest("outside", "registered", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return readyReason(obj) == "True"
	})
	vol := c.volume("outside")
	must.Eq(t, "vol-0abc", vol.ExternalID)
	must.Eq(t, "z1", vol.Context["zone"])
	outsideName := c.volumeName("outside")
	fake.set(func(f *fakeVolumeCSI) {
		must.Eq(t, []string{"vol-0abc"}, f.validates)
		must.SliceNotContains(t, f.creates, outsideName)
	})
}

func TestVolumeRequest_FailureAndDeletion(t *testing.T) {
	ci.Parallel(t)
	c, fake, cleanup := volumeRequestTestServer(t, false, func(c *Config) {
		c.volumeRequestRetryBase = 200 * time.Millisecond
	})
	defer cleanup()

	fake.set(func(f *fakeVolumeCSI) { f.createErr = errors.New("quota of the storage exceeded") })
	for name, policy := range map[string]string{"keep-me": "retain", "forget-me": "deregister", "drop-me": "delete"} {
		_, err := c.apply("default", csiVolumeRequestKind, name, volumeSpec(func(m map[string]any) { m["deletion_policy"] = policy }))
		must.NoError(t, err)
	}
	st := c.waitRequest("drop-me", "failed", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return st.Failures > 0 && objectConditionTrue(obj.Status, volumeRequestConditionFailed) &&
			objectConditionTrue(obj.Status, structs.CertificateConditionDegraded)
	})
	must.NotEq(t, "", st.RetryAt)
	obj, _ := c.request("drop-me")
	must.StrContains(t, volumeRequestFailure(obj.Status), "quota of the storage exceeded")

	fake.set(func(f *fakeVolumeCSI) { f.createErr = nil })
	for _, name := range []string{"keep-me", "forget-me", "drop-me"} {
		c.waitRequest(name, "created after the failure", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
			return readyReason(obj) == "True" && st.Failures == 0
		})
	}

	dropName := c.volumeName("drop-me")
	for _, name := range []string{"keep-me", "forget-me", "drop-me"} {
		must.NoError(t, c.delete("default", csiVolumeRequestKind, name))
	}
	for _, name := range []string{"keep-me", "forget-me", "drop-me"} {
		must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
			return c.get("default", csiVolumeRequestKind, name) == nil
		}), wait.Timeout(20*time.Second), wait.Gap(100*time.Millisecond)), must.Sprintf("%s gone", name))
	}
	must.NotNil(t, c.volume("keep-me"))
	must.Nil(t, c.volume("forget-me"))
	must.Nil(t, c.volume("drop-me"))
	fake.set(func(f *fakeVolumeCSI) { must.Eq(t, []string{"ext-" + dropName}, f.deletes) })
}

func TestVolumeRequest_WriteChecks(t *testing.T) {
	ci.Parallel(t)
	c, _, cleanup := volumeRequestTestServer(t, true)
	defer cleanup()

	for name, tc := range map[string]struct {
		spec map[string]any
		want string
	}{
		"register without external id": {volumeSpec(func(m map[string]any) { m["mode"] = "register" }), "needs external_id"},
		"create with external id":      {volumeSpec(func(m map[string]any) { m["external_id"] = "x" }), "external_id is for mode"},
		"delete a registered volume": {volumeSpec(func(m map[string]any) {
			m["mode"], m["external_id"], m["deletion_policy"] = "register", "x", "delete"
		}), "use \"deregister\""},
		"bad capacity":  {volumeSpec(func(m map[string]any) { m["capacity_min"] = "lots" }), "capacity_min"},
		"max below min": {volumeSpec(func(m map[string]any) { m["capacity_max"] = "512MiB" }), "less than capacity_min"},
		"no capability": {volumeSpec(func(m map[string]any) { delete(m, "capability") }), "capability"},
		"bad access mode": {volumeSpec(func(m map[string]any) {
			m["capability"] = map[string]any{"access_mode": "everyone", "attachment_mode": "file-system"}
		}), "access_mode"},
		"two capabilities, one bad": {volumeSpec(func(m map[string]any) {
			m["capability"] = []any{
				map[string]any{"access_mode": "single-node-writer", "attachment_mode": "file-system"},
				map[string]any{"access_mode": "single-node-writer", "attachment_mode": "tape"},
			}
		}), "attachment_mode"},
		"unknown field": {volumeSpec(func(m map[string]any) { m["size"] = "1GiB" }), "size"},
	} {
		_, err := c.apply("default", csiVolumeRequestKind, "bad", tc.spec)
		must.ErrorContains(t, err, tc.want, must.Sprintf("%s", name))
	}

	_, err := c.apply("default", csiVolumeRequestKind, "data", volumeSpec(nil))
	must.NoError(t, err)
	_, err = c.apply("default", csiVolumeRequestKind, "data", volumeSpec(func(m map[string]any) { m["plugin_id"] = "other" }))
	must.ErrorContains(t, err, "plugin_id can't be changed")
	must.Eq(t, 403, errCode(t, c.status("default", csiVolumeRequestKind, "data", map[string]any{"capacityBytes": 1})))

	objects := mock.CreatePolicyAndToken(t, c.s.State(), 1001, "objects",
		`namespace "default" { object "norca.io/CSIVolumeRequest" { capabilities = ["read", "write"] } }`)
	volumes := mock.CreatePolicyAndToken(t, c.s.State(), 1003, "volumes", `namespace "default" {
  object "norca.io/CSIVolumeRequest" { capabilities = ["read", "write"] }
  capabilities = ["csi-write-volume"]
}
plugin { policy = "read" }`)

	noVolume := &certTest{t: t, s: c.s, token: objects.SecretID}
	_, err = noVolume.apply("default", csiVolumeRequestKind, "w1", volumeSpec(nil))
	must.Eq(t, 403, errCode(t, err))
	must.ErrorContains(t, err, "csi-write-volume")

	writer := &certTest{t: t, s: c.s, token: volumes.SecretID}
	_, err = writer.apply("default", csiVolumeRequestKind, "w2", volumeSpec(nil))
	must.NoError(t, err)
	_, err = writer.apply("default", csiVolumeRequestKind, "w3", volumeSpec(func(m map[string]any) { m["secret_path"] = "csi/minnie" }))
	must.Eq(t, 403, errCode(t, err))
	must.ErrorContains(t, err, "right to read its secret")
}

func TestVolumeRequestSpec(t *testing.T) {
	ci.Parallel(t)

	obj := &structs.CustomObject{ID: "0a1b2c3d-4e5f-6a7b-8c9d-0e1f2a3b4c5d", Namespace: "prod", Name: "db"}
	spec := &volumeRequestSpec{}
	must.NoError(t, structs.DecodeObjectPart(map[string]any{
		"plugin_id":    "ebs",
		"capacity_min": "10GiB",
		"capacity_max": "20GiB",
		"capability":   map[string]any{"access_mode": "single-node-writer", "attachment_mode": "file-system"},
		"topology_request": map[string]any{
			"required": map[string]any{"segments": map[string]any{"zone": "a"}},
			"preferred": []any{
				map[string]any{"segments": map[string]any{"zone": "b"}},
				map[string]any{"segments": map[string]any{"zone": "c"}},
			},
		},
	}, spec))
	must.NoError(t, spec.validate())
	vol := spec.volume(obj, structs.CSISecrets{"k": "v"})
	must.Eq(t, "db", vol.ID)
	must.Eq(t, "prod-db-0a1b2c3d", vol.Name)
	spec.DisplayName = "the-db"
	must.Eq(t, "the-db", spec.volume(obj, nil).Name)
	spec.DisplayName = ""
	must.Eq(t, "prod", vol.Namespace)
	must.Eq(t, 10*gibibyte, vol.RequestedCapacityMin)
	must.Eq(t, 20*gibibyte, vol.RequestedCapacityMax)
	must.Len(t, 1, vol.RequestedCapabilities)
	must.Len(t, 1, vol.RequestedTopologies.Required)
	must.Len(t, 2, vol.RequestedTopologies.Preferred)
	must.NoError(t, vol.Validate())

	// What is compared for drift.
	other := vol.Copy()
	must.Len(t, 0, volumeDiff(vol, other, false))
	other.Context = map[string]string{"from": "plugin"}
	must.Len(t, 0, volumeDiff(other, vol, false))
	must.Eq(t, []string{"context"}, volumeDiff(other, vol, true))
	other.MountOptions = &structs.CSIMountOptions{}
	must.Len(t, 0, volumeDiff(other, vol, false), must.Sprint("empty mount options are none"))
	other.Secrets = nil
	other.RequestedCapacityMin = 1
	diff := volumeDiff(other, vol, false)
	slices.Sort(diff)
	must.Eq(t, []string{"capacity", "secrets"}, diff)
}

// The name of the volume of a request that gives none tells requests apart
// whatever their namespaces and names are: "a-b"/"c" and "a"/"b-c" don't
// share a volume of the storage (review finding SEC-20). It fits the CSI
// limit of 128 bytes with the longest namespace and name.
func TestVolumeRequestName(t *testing.T) {
	ci.Parallel(t)

	first := &structs.CustomObject{ID: uuid.Generate(), Namespace: "a-b", Name: "c"}
	second := &structs.CustomObject{ID: uuid.Generate(), Namespace: "a", Name: "b-c"}
	must.NotEq(t, volumeRequestName(first), volumeRequestName(second),
		must.Sprint("two requests get the name of one volume"))
	must.Eq(t, volumeRequestName(first), volumeRequestName(first.Copy()))
	must.StrHasPrefix(t, "a-b-c-", volumeRequestName(first))

	long := &structs.CustomObject{ID: uuid.Generate(), Namespace: strings.Repeat("n", 128), Name: strings.Repeat("o", 128)}
	must.LessEq(t, 128, len(volumeRequestName(long)))
	must.StrHasSuffix(t, "-"+strings.ReplaceAll(long.ID, "-", "")[:8], volumeRequestName(long))
}

func TestVolumeRequest_DeletionWaitsForClaims(t *testing.T) {
	ci.Parallel(t)
	c, _, cleanup := volumeRequestTestServer(t, false)
	defer cleanup()

	_, err := c.apply("default", csiVolumeRequestKind, "busy", volumeSpec(func(m map[string]any) { m["deletion_policy"] = "deregister" }))
	must.NoError(t, err)
	c.waitRequest("busy", "created", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return readyReason(obj) == "True"
	})

	// An allocation claims the volume.
	alloc := mock.BatchAlloc()
	alloc.ClientStatus = structs.AllocClientStatusRunning
	index, err := c.s.State().LatestIndex()
	must.NoError(t, err)
	must.NoError(t, c.s.State().UpsertAllocs(structs.MsgTypeTestSetup, index+100, []*structs.Allocation{alloc}))
	claim := &structs.CSIVolumeClaim{
		AllocationID: alloc.ID, NodeID: alloc.NodeID, Mode: structs.CSIVolumeClaimWrite,
		AccessMode: structs.CSIVolumeAccessModeSingleNodeWriter, AttachmentMode: structs.CSIVolumeAttachmentModeFilesystem,
		State: structs.CSIVolumeClaimStateTaken,
	}
	must.NoError(t, c.s.State().CSIVolumeClaim(index+101, "default", "busy", claim))

	must.NoError(t, c.delete("default", csiVolumeRequestKind, "busy"))
	c.waitRequest("busy", "held by the claim", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return obj.Deleting() && readyReason(obj) == "VolumeInUse"
	})
	must.NotNil(t, c.volume("busy"))

	// The claim is released: the volume and the request go.
	released := *claim
	released.Mode, released.State = structs.CSIVolumeClaimGC, structs.CSIVolumeClaimStateReadyToFree
	must.NoError(t, c.s.State().CSIVolumeClaim(index+102, "default", "busy", &released))
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		return c.get("default", csiVolumeRequestKind, "busy") == nil
	}), wait.Timeout(70*time.Second), wait.Gap(200*time.Millisecond)))
	must.Nil(t, c.volume("busy"))
}

// The topologies of a volume can't change, so the topology request of a
// request can't either (review finding VOL-8).
func TestVolumeRequest_TopologyRequestImmutable(t *testing.T) {
	ci.Parallel(t)
	c, _, cleanup := volumeRequestTestServer(t, false)
	defer cleanup()

	zone := func(z string) func(map[string]any) {
		return func(m map[string]any) {
			m["topology_request"] = map[string]any{"required": map[string]any{"segments": map[string]any{"zone": z}}}
		}
	}
	_, err := c.apply("default", csiVolumeRequestKind, "plain", volumeSpec(nil))
	must.NoError(t, err)
	_, err = c.apply("default", csiVolumeRequestKind, "plain", volumeSpec(zone("a")))
	must.ErrorContains(t, err, "topology_request can't be changed")

	_, err = c.apply("default", csiVolumeRequestKind, "zoned", volumeSpec(zone("a")))
	must.NoError(t, err)
	_, err = c.apply("default", csiVolumeRequestKind, "zoned", volumeSpec(zone("b")))
	must.ErrorContains(t, err, "topology_request can't be changed")
	_, err = c.apply("default", csiVolumeRequestKind, "zoned", volumeSpec(func(m map[string]any) {
		zone("a")(m)
		m["capacity_min"] = "2GiB"
	}))
	must.NoError(t, err)
}

// The blocks whose shape the schema leaves open (capability, the topologies)
// refuse keys they don't know (review finding VOL-11).
func TestVolumeRequestSpec_UnknownBlockKeys(t *testing.T) {
	ci.Parallel(t)

	capability := map[string]any{"access_mode": "single-node-writer", "attachment_mode": "file-system"}
	withKey := func(m map[string]any, key string) map[string]any {
		out := map[string]any{key: "x"}
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	topology := map[string]any{"segments": map[string]any{"zone": "a"}}

	for name, tc := range map[string]struct {
		spec map[string]any
		want string
	}{
		"capability block": {map[string]any{"capability": withKey(capability, "fs_type")}, "fs_type"},
		"capability list":  {map[string]any{"capability": []any{capability, withKey(capability, "fs_type")}}, "fs_type"},
		"required topology": {map[string]any{"capability": capability,
			"topology_request": map[string]any{"required": withKey(topology, "zone")}}, "zone"},
		"preferred topologies": {map[string]any{"capability": capability,
			"topology_request": map[string]any{"preferred": []any{topology, withKey(topology, "rack")}}}, "rack"},
	} {
		err := structs.DecodeObjectPart(tc.spec, &volumeRequestSpec{})
		must.ErrorContains(t, err, tc.want, must.Sprintf("%s", name))
	}

	// What is known is taken, as a block or as a list.
	spec := &volumeRequestSpec{}
	must.NoError(t, structs.DecodeObjectPart(map[string]any{
		"capability":       []any{capability, capability},
		"topology_request": map[string]any{"required": topology, "preferred": []any{topology}},
	}, spec))
	must.Len(t, 2, spec.Capability)
	must.Len(t, 1, spec.TopologyRequest.Required)
	must.Len(t, 1, spec.TopologyRequest.Preferred)
}

// Deleting a request whose policy takes the volume away needs the rights of
// the volume API, as writing it does (review finding VOL-2).
func TestVolumeRequest_DeleteChecks(t *testing.T) {
	ci.Parallel(t)
	c, _, cleanup := volumeRequestTestServer(t, true)
	defer cleanup()

	for name, policy := range map[string]string{
		"kept": "retain", "forgotten": "deregister", "dropped": "delete", "dropped-too": "delete",
	} {
		_, err := c.apply("default", csiVolumeRequestKind, name, volumeSpec(func(m map[string]any) { m["deletion_policy"] = policy }))
		must.NoError(t, err)
	}

	objects := mock.CreatePolicyAndToken(t, c.s.State(), 1001, "objects",
		`namespace "default" { object "norca.io/CSIVolumeRequest" { capabilities = ["read", "write"] } }`)
	volumes := mock.CreatePolicyAndToken(t, c.s.State(), 1003, "volumes", `namespace "default" {
  object "norca.io/CSIVolumeRequest" { capabilities = ["read", "write"] }
  capabilities = ["csi-write-volume"]
}
plugin { policy = "read" }`)

	noVolume := &certTest{t: t, s: c.s, token: objects.SecretID}
	for _, name := range []string{"forgotten", "dropped"} {
		err := noVolume.delete("default", csiVolumeRequestKind, name)
		must.Eq(t, 403, errCode(t, err), must.Sprintf("%s", name))
		must.ErrorContains(t, err, "csi-write-volume")
		must.False(t, c.get("default", csiVolumeRequestKind, name).Deleting())
	}
	// With retain the volume is not touched: the right to write the object
	// is enough.
	must.NoError(t, noVolume.delete("default", csiVolumeRequestKind, "kept"))

	writer := &certTest{t: t, s: c.s, token: volumes.SecretID}
	for _, name := range []string{"forgotten", "dropped"} {
		must.NoError(t, writer.delete("default", csiVolumeRequestKind, name))
	}
	must.NoError(t, c.delete("default", csiVolumeRequestKind, "dropped-too"))
}

// claim makes a running allocation that claims the volume for writing.
func (c *certTest) claim(id string) *structs.CSIVolumeClaim {
	c.t.Helper()
	alloc := mock.BatchAlloc()
	alloc.ClientStatus = structs.AllocClientStatusRunning
	index, err := c.s.State().LatestIndex()
	must.NoError(c.t, err)
	must.NoError(c.t, c.s.State().UpsertAllocs(structs.MsgTypeTestSetup, index+100, []*structs.Allocation{alloc}))
	claim := &structs.CSIVolumeClaim{
		AllocationID: alloc.ID, NodeID: alloc.NodeID, Mode: structs.CSIVolumeClaimWrite,
		AccessMode: structs.CSIVolumeAccessModeSingleNodeWriter, AttachmentMode: structs.CSIVolumeAttachmentModeFilesystem,
		State: structs.CSIVolumeClaimStateTaken,
	}
	must.NoError(c.t, c.s.State().CSIVolumeClaim(index+101, "default", id, claim))
	return claim
}

// A request without mount options expands its volume while a job uses it
// (review finding VOL-5).
func TestVolumeRequest_ExpandInUseWithoutMountOptions(t *testing.T) {
	ci.Parallel(t)
	c, fake, cleanup := volumeRequestTestServer(t, false)
	defer cleanup()

	_, err := c.apply("default", csiVolumeRequestKind, "live", volumeSpec(nil))
	must.NoError(t, err)
	c.waitRequest("live", "created", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return readyReason(obj) == "True"
	})
	c.claim("live")
	must.True(t, c.volume("live").InUse())

	_, err = c.apply("default", csiVolumeRequestKind, "live", volumeSpec(func(m map[string]any) { m["capacity_min"] = "2GiB" }))
	must.NoError(t, err)
	st := c.waitRequest("live", "expanded or failed", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return st.Failures > 0 || (st.CapacityBytes == 2*gibibyte && readyReason(obj) == "True")
	})
	obj, _ := c.request("live")
	must.Zero(t, st.Failures, must.Sprintf("the update failed: %s", volumeRequestFailure(obj.Status)))
	must.Eq(t, 2*gibibyte, c.volume("live").Capacity)
	must.MapLen(t, 1, c.volume("live").WriteClaims)
	fake.set(func(f *fakeVolumeCSI) { must.Eq(t, []int64{2 * gibibyte}, f.expands) })
}

// Requests of the same name in two namespaces get a volume each: the name
// the plugin creates the volume by has the namespace in it (review finding
// VOL-1).
func TestVolumeRequest_NamespacesDoNotShareAVolume(t *testing.T) {
	ci.Parallel(t)
	c, fake, cleanup := volumeRequestTestServer(t, false)
	defer cleanup()

	created := func(ns, name string) *structs.CSIVolume {
		t.Helper()
		var vol *structs.CSIVolume
		must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
			obj := c.get(ns, csiVolumeRequestKind, name)
			if obj == nil || readyReason(obj) != "True" {
				return false
			}
			var err error
			vol, err = c.s.State().CSIVolumeByID(nil, ns, name)
			return err == nil && vol != nil
		}), wait.Timeout(30*time.Second), wait.Gap(100*time.Millisecond)), must.Sprintf("%s/%s created", ns, name))
		return vol
	}

	for _, ns := range []string{"staging", "prod"} {
		c.allow(ns, nil)
		_, err := c.apply(ns, csiVolumeRequestKind, "db", volumeSpec(func(m map[string]any) { m["deletion_policy"] = "delete" }))
		must.NoError(t, err)
	}
	staging, prod := created("staging", "db"), created("prod", "db")
	must.Eq(t, volumeRequestName(c.get("staging", csiVolumeRequestKind, "db")), staging.Name)
	must.Eq(t, volumeRequestName(c.get("prod", csiVolumeRequestKind, "db")), prod.Name)
	must.StrHasPrefix(t, "staging-db-", staging.Name)
	must.StrHasPrefix(t, "prod-db-", prod.Name)
	must.NotEq(t, staging.ExternalID, prod.ExternalID,
		must.Sprint("two namespaces share one volume of the storage"))

	// Deleting one leaves the volume of the other alone.
	must.NoError(t, c.delete("staging", csiVolumeRequestKind, "db"))
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		return c.get("staging", csiVolumeRequestKind, "db") == nil
	}), wait.Timeout(20*time.Second), wait.Gap(100*time.Millisecond)))
	fake.set(func(f *fakeVolumeCSI) { must.SliceNotContains(t, f.deletes, prod.ExternalID) })

	// A name that is given is sent as it is. If the storage answers with a
	// volume that another request or a volume registered by hand has, the
	// request fails instead of taking it as its own.
	c.allow("other", nil)
	_, err := c.apply("default", csiVolumeRequestKind, "first", volumeSpec(func(m map[string]any) { m["display_name"] = "shared-name" }))
	must.NoError(t, err)
	first := created("default", "first")
	must.Eq(t, "shared-name", first.Name)
	_, err = c.apply("other", csiVolumeRequestKind, "second", volumeSpec(func(m map[string]any) {
		m["display_name"] = "shared-name"
		m["deletion_policy"] = "delete"
	}))
	must.NoError(t, err)
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		obj := c.get("other", csiVolumeRequestKind, "second")
		return obj != nil && (objectConditionTrue(obj.Status, volumeRequestConditionFailed) || readyReason(obj) == "True")
	}), wait.Timeout(30*time.Second), wait.Gap(100*time.Millisecond)))
	second := c.get("other", csiVolumeRequestKind, "second")
	must.StrContains(t, volumeRequestFailure(second.Status), "default/first")
	vol, err := c.s.State().CSIVolumeByID(nil, "other", "second")
	must.NoError(t, err)
	must.Nil(t, vol)

	// Deleting the request that failed does not delete the volume of the
	// other.
	must.NoError(t, c.delete("other", csiVolumeRequestKind, "second"))
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		return c.get("other", csiVolumeRequestKind, "second") == nil
	}), wait.Timeout(20*time.Second), wait.Gap(100*time.Millisecond)))
	fake.set(func(f *fakeVolumeCSI) { must.SliceNotContains(t, f.deletes, first.ExternalID) })
}

// A changed spec is tried at once: the wait after the failures of the old
// spec is not its wait (review finding VOL-9).
func TestVolumeRequest_SpecChangeEndsTheWait(t *testing.T) {
	ci.Parallel(t)
	c, fake, cleanup := volumeRequestTestServer(t, false, func(c *Config) {
		c.volumeRequestRetryBase = time.Hour
	})
	defer cleanup()

	fake.set(func(f *fakeVolumeCSI) { f.createErr = errors.New("the storage has no room for 1GiB") })
	_, err := c.apply("default", csiVolumeRequestKind, "fixed", volumeSpec(nil))
	must.NoError(t, err)
	c.waitRequest("fixed", "failed", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return st.Failures == 1 && st.RetryAt != ""
	})

	fake.set(func(f *fakeVolumeCSI) { f.createErr = nil })
	_, err = c.apply("default", csiVolumeRequestKind, "fixed", volumeSpec(func(m map[string]any) { m["capacity_min"] = "512MiB" }))
	must.NoError(t, err)
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		obj, st := c.request("fixed")
		return readyReason(obj) == "True" && st.Failures == 0 && st.RetryAt == ""
	}), wait.Timeout(10*time.Second), wait.Gap(100*time.Millisecond)),
		must.Sprint("the corrected spec waits for the retry time of the old one"))
	obj, _ := c.request("fixed")
	must.False(t, objectConditionTrue(obj.Status, volumeRequestConditionFailed))
	must.NotNil(t, c.volume("fixed"))
}

// controllerHealthy sets the health of the controller of the plugin
// "minnie".
func (c *certTest) controllerHealthy(healthy bool) {
	c.t.Helper()
	iter, err := c.s.State().Nodes(nil)
	must.NoError(c.t, err)
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		node := raw.(*structs.Node).Copy()
		info := node.CSIControllerPlugins["minnie"]
		if info == nil {
			continue
		}
		info.Healthy = healthy
		index, err := c.s.State().LatestIndex()
		must.NoError(c.t, err)
		must.NoError(c.t, c.s.State().UpsertNode(structs.MsgTypeTestSetup, index+100, node))
	}
}

// A request that waits for the retry of a failed create is not ready, also
// after its plugin was away in between (review finding VOL-6).
func TestVolumeRequest_NotReadyWhileRetrying(t *testing.T) {
	ci.Parallel(t)
	c, fake, cleanup := volumeRequestTestServer(t, false, func(c *Config) {
		c.volumeRequestRetryBase = time.Hour
	})
	defer cleanup()

	fake.set(func(f *fakeVolumeCSI) { f.createErr = errors.New("quota of the storage exceeded") })
	_, err := c.apply("default", csiVolumeRequestKind, "late", volumeSpec(nil))
	must.NoError(t, err)
	c.waitRequest("late", "failed", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return st.Failures == 1 && readyReason(obj) == volumeRequestConditionFailed
	})

	c.controllerHealthy(false)
	c.waitRequest("late", "plugin away", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return readyReason(obj) == "PluginUnavailable"
	})
	c.controllerHealthy(true)
	c.waitRequest("late", "plugin back", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return readyReason(obj) != "PluginUnavailable"
	})

	obj, st := c.request("late")
	must.Nil(t, c.volume("late"))
	must.Eq(t, volumeRequestConditionFailed, readyReason(obj), must.Sprint("ready without a volume"))
	must.True(t, objectConditionTrue(obj.Status, volumeRequestConditionFailed))
	must.True(t, objectConditionTrue(obj.Status, structs.CertificateConditionDegraded))
	must.StrContains(t, volumeRequestFailure(obj.Status), "quota of the storage exceeded")
	must.Eq(t, 1, st.Failures)
	must.NotEq(t, "", st.RetryAt)
}

// What the request or its secret asks for is an update, also when its first
// attempt failed; only what was changed on the volume is drift (review
// finding VOL-7).
func TestVolumeRequest_UpdatesAreNotDrift(t *testing.T) {
	ci.Parallel(t)
	c, fake, cleanup := volumeRequestTestServer(t, false, func(c *Config) {
		c.volumeRequestRetryBase = 200 * time.Millisecond
	})
	defer cleanup()

	secret := func(value string) {
		t.Helper()
		var resp structs.SecretWriteResponse
		must.NoError(t, c.s.RPC(structs.SecretUpsertRPCMethod, secretWriteReq("csi/minnie", "", structs.SecretItems{"token": value}), &resp))
	}
	spec := func(capacity string) map[string]any {
		return volumeSpec(func(m map[string]any) {
			m["capacity_min"] = capacity
			m["secret_path"] = "csi/minnie"
		})
	}
	reasons := func() map[string]int {
		t.Helper()
		log, err := c.s.State().ObjectEventLog(nil, structs.OperatorsGroup, csiVolumeRequestKind, "default", "db")
		must.NoError(t, err)
		out := map[string]int{}
		if log != nil {
			for _, e := range log.Events {
				out[e.Reason]++
			}
		}
		return out
	}

	secret("one")
	_, err := c.apply("default", csiVolumeRequestKind, "db", spec("1GiB"))
	must.NoError(t, err)
	c.waitRequest("db", "created", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return readyReason(obj) == "True"
	})

	// The first attempt of a change of the spec fails, the next one works.
	fake.set(func(f *fakeVolumeCSI) { f.expandErr = errors.New("the storage is busy") })
	_, err = c.apply("default", csiVolumeRequestKind, "db", spec("2GiB"))
	must.NoError(t, err)
	c.waitRequest("db", "expansion failed", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return st.Failures > 0
	})
	fake.set(func(f *fakeVolumeCSI) { f.expandErr = nil })
	st := c.waitRequest("db", "expanded", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return st.CapacityBytes == 2*gibibyte && readyReason(obj) == "True"
	})
	must.Zero(t, st.Drifts, must.Sprint("the retry of a failed update was counted as drift"))
	must.Eq(t, "", st.LastDrift)
	must.Zero(t, reasons()["DriftCorrected"])

	// The secret changes in the secret store: the volume follows it. A pass
	// reads the secret; another request written wakes the operator.
	secret("two")
	_, err = c.apply("default", csiVolumeRequestKind, "wake", volumeSpec(nil))
	must.NoError(t, err)
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		return c.volume("db").Secrets["token"] == "two"
	}), wait.Timeout(30*time.Second), wait.Gap(100*time.Millisecond)))
	st = c.waitRequest("db", "ready", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return readyReason(obj) == "True"
	})
	must.Zero(t, st.Drifts, must.Sprint("a changed secret was counted as drift"))
	must.Zero(t, reasons()["DriftCorrected"])

	// Changed on the volume by hand: that is drift.
	changed := c.volume("db").Copy()
	changed.MountOptions = &structs.CSIMountOptions{FSType: "xfs"}
	c.writeVolume(changed)
	c.waitRequest("db", "drift set back", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return st.Drifts == 1 && st.LastDrift != ""
	})
	must.Eq(t, 1, reasons()["DriftCorrected"])
}

// A volume that appears while a request is creating its own is the request's
// only if it is the volume the request would have made; and a request that
// has not called the plugin yet is not creating (review finding VOL-4).
func TestVolumeRequest_CreatingTakesOnlyItsOwnVolume(t *testing.T) {
	ci.Parallel(t)
	c, fake, cleanup := volumeRequestTestServer(t, false, func(c *Config) {
		c.volumeRequestRetryBase = time.Hour
	})
	defer cleanup()

	byHand := func(id, name, plugin string) *structs.CSIVolume {
		t.Helper()
		c.writeVolume(&structs.CSIVolume{
			ID: id, Name: name, Namespace: "default", PluginID: plugin, ExternalID: "by-hand-" + id,
			Capacity: gibibyte, RequestedCapacityMin: gibibyte,
			RequestedCapabilities: []*structs.CSIVolumeCapability{{
				AccessMode: structs.CSIVolumeAccessModeSingleNodeWriter, AttachmentMode: structs.CSIVolumeAttachmentModeFilesystem}},
		})
		return c.volume(id)
	}
	conflict := func(name string) {
		t.Helper()
		st := c.waitRequest(name, "conflict or taken", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
			return readyReason(obj) == volumeRequestConditionConflict || st.VolumeCreateIndex != 0
		})
		must.Zero(t, st.VolumeCreateIndex, must.Sprintf("%s took a volume it did not make as its own", name))
	}

	// The plugin of the request is not there: nothing was sent to a plugin,
	// so the request is not creating, and a volume made by hand in the
	// meantime is not its own.
	_, err := c.apply("default", csiVolumeRequestKind, "waiting", volumeSpec(func(m map[string]any) {
		m["plugin_id"] = "ghost"
		m["deletion_policy"] = "delete"
	}))
	must.NoError(t, err)
	st := c.waitRequest("waiting", "no plugin", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return readyReason(obj) == "PluginNotFound"
	})
	must.False(t, st.Creating, must.Sprint("creating is set though no plugin was called"))
	waiting := byHand("waiting", c.volumeName("waiting"), "minnie")
	conflict("waiting")

	// The create failed and waits for its retry. A volume of another name
	// that is registered by hand is not what the request was creating.
	fake.set(func(f *fakeVolumeCSI) { f.createErr = errors.New("the storage does not answer") })
	_, err = c.apply("default", csiVolumeRequestKind, "retrying", volumeSpec(func(m map[string]any) { m["deletion_policy"] = "delete" }))
	must.NoError(t, err)
	st = c.waitRequest("retrying", "failed", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return st.Failures == 1
	})
	must.True(t, st.Creating)
	retrying := byHand("retrying", "somebody-elses", "minnie")
	conflict("retrying")

	// Deleting the requests leaves the volumes made by hand alone.
	for _, name := range []string{"waiting", "retrying"} {
		must.NoError(t, c.delete("default", csiVolumeRequestKind, name))
	}
	for _, name := range []string{"waiting", "retrying"} {
		must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
			return c.get("default", csiVolumeRequestKind, name) == nil
		}), wait.Timeout(20*time.Second), wait.Gap(100*time.Millisecond)), must.Sprintf("%s gone", name))
	}
	must.Eq(t, waiting.ModifyIndex, c.volume("waiting").ModifyIndex)
	must.Eq(t, retrying.ModifyIndex, c.volume("retrying").ModifyIndex)
	fake.set(func(f *fakeVolumeCSI) { must.Len(t, 0, f.deletes) })
}

// A create whose answer was lost may have made the volume. A request with
// the policy delete that is deleted then asks the plugin for the volume once
// more and deletes it (review finding VOL-10).
func TestVolumeRequest_DeletionAfterLostCreateAnswer(t *testing.T) {
	ci.Parallel(t)
	c, fake, cleanup := volumeRequestTestServer(t, false, func(c *Config) {
		c.volumeRequestRetryBase = time.Hour
	})
	defer cleanup()

	gone := func(name string) {
		t.Helper()
		must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
			return c.get("default", csiVolumeRequestKind, name) == nil
		}), wait.Timeout(20*time.Second), wait.Gap(100*time.Millisecond)), must.Sprintf("%s gone", name))
	}

	// The storage made the volumes, the answers did not arrive.
	fake.set(func(f *fakeVolumeCSI) { f.createErr = errors.New("context deadline exceeded") })
	for name, policy := range map[string]string{"orphan": "delete", "left": "deregister"} {
		_, err := c.apply("default", csiVolumeRequestKind, name, volumeSpec(func(m map[string]any) { m["deletion_policy"] = policy }))
		must.NoError(t, err)
	}
	for _, name := range []string{"orphan", "left"} {
		st := c.waitRequest(name, "failed", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
			return st.Failures == 1
		})
		must.True(t, st.Creating)
	}

	fake.set(func(f *fakeVolumeCSI) { f.createErr = nil })
	orphanName := c.volumeName("orphan")
	must.NoError(t, c.delete("default", csiVolumeRequestKind, "orphan"))
	must.NoError(t, c.delete("default", csiVolumeRequestKind, "left"))
	gone("orphan")
	gone("left")
	must.Nil(t, c.volume("orphan"))
	fake.set(func(f *fakeVolumeCSI) {
		must.Eq(t, []string{"ext-" + orphanName}, f.deletes, must.Sprint("the volume of the lost answer is left in the storage"))
	})

	// The plugin can't be asked when the request is deleted: the request
	// goes, and says what may be left.
	fake.set(func(f *fakeVolumeCSI) { f.createErr = errors.New("context deadline exceeded") })
	_, err := c.apply("default", csiVolumeRequestKind, "unknown", volumeSpec(func(m map[string]any) { m["deletion_policy"] = "delete" }))
	must.NoError(t, err)
	c.waitRequest("unknown", "failed", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return st.Failures == 1
	})
	must.NoError(t, c.delete("default", csiVolumeRequestKind, "unknown"))
	gone("unknown")
	fake.set(func(f *fakeVolumeCSI) { must.Len(t, 1, f.deletes) })
}

// A claim made while the plugin expands the volume is kept when the update
// is written (review finding VOL-3).
func TestVolumeRequest_UpdateKeepsClaimsMadeMeanwhile(t *testing.T) {
	ci.Parallel(t)
	c, fake, cleanup := volumeRequestTestServer(t, false)
	defer cleanup()

	_, err := c.apply("default", csiVolumeRequestKind, "busy", volumeSpec(nil))
	must.NoError(t, err)
	c.waitRequest("busy", "created", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return readyReason(obj) == "True"
	})

	// An allocation claims the volume while the plugin expands it.
	var claim *structs.CSIVolumeClaim
	fake.set(func(f *fakeVolumeCSI) { f.onExpand = func() { claim = c.claim("busy") } })
	_, err = c.apply("default", csiVolumeRequestKind, "busy", volumeSpec(func(m map[string]any) { m["capacity_min"] = "2GiB" }))
	must.NoError(t, err)
	st := c.waitRequest("busy", "expanded or failed", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return st.Failures > 0 || (st.CapacityBytes == 2*gibibyte && readyReason(obj) == "True")
	})
	obj, _ := c.request("busy")
	must.Zero(t, st.Failures, must.Sprintf("the update failed: %s", volumeRequestFailure(obj.Status)))

	vol := c.volume("busy")
	must.Eq(t, 2*gibibyte, vol.Capacity)
	must.NotNil(t, claim)
	must.MapContainsKey(t, vol.WriteClaims, claim.AllocationID, must.Sprint("the claim made during the expansion is lost"))
	must.MapContainsKey(t, vol.WriteAllocs, claim.AllocationID)
	must.True(t, vol.InUse())
	must.Eq(t, 1, c.waitRequest("busy", "the claim shows", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return st.WriteAllocs == 1
	}).WriteAllocs)

	// A volume that is deregistered while the plugin works is not written
	// back.
	_, err = c.apply("default", csiVolumeRequestKind, "leaving", volumeSpec(nil))
	must.NoError(t, err)
	c.waitRequest("leaving", "created", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return readyReason(obj) == "True"
	})
	deregistered := make(chan error, 1)
	fake.set(func(f *fakeVolumeCSI) {
		f.onExpand = func() {
			_, _, err := c.s.raftApply(structs.CSIVolumeDeregisterRequestType, &structs.CSIVolumeDeregisterRequest{
				VolumeIDs:    []string{"leaving"},
				WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default"},
			})
			deregistered <- err
		}
	})
	_, err = c.apply("default", csiVolumeRequestKind, "leaving", volumeSpec(func(m map[string]any) { m["capacity_min"] = "3GiB" }))
	must.NoError(t, err)
	select {
	case err := <-deregistered:
		must.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("the volume was not expanded")
	}
	c.waitRequest("leaving", "volume lost", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return readyReason(obj) == "VolumeLost"
	})
	must.Nil(t, c.volume("leaving"))
}

// The register of the operator is a check-and-set on the volume it read: a
// claim committed between its read and its register is not written over.
// The register is refused, nothing fails, and the next pass makes the update
// from the volume as it is then (the window left by the fix of VOL-3).
func TestVolumeRequest_RegisterIsCheckAndSet(t *testing.T) {
	ci.Parallel(t)

	// before runs once, right before the next register of the operator.
	var mu sync.Mutex
	var before func(vol *structs.CSIVolume)
	registers := 0
	set := func(f func(vol *structs.CSIVolume)) {
		mu.Lock()
		defer mu.Unlock()
		before, registers = f, 0
	}
	c, fake, cleanup := volumeRequestTestServer(t, false, func(c *Config) {
		c.volumeRequestBeforeRegister = func(vol *structs.CSIVolume) {
			mu.Lock()
			f := before
			before = nil
			registers++
			mu.Unlock()
			if f != nil {
				f(vol)
			}
		}
	})
	defer cleanup()

	failedEvents := func(name string) int {
		t.Helper()
		log, err := c.s.State().ObjectEventLog(nil, structs.OperatorsGroup, csiVolumeRequestKind, "default", name)
		must.NoError(t, err)
		n := 0
		if log != nil {
			for _, e := range log.Events {
				if e.Reason == "Failed" {
					n++
				}
			}
		}
		return n
	}

	_, err := c.apply("default", csiVolumeRequestKind, "busy", volumeSpec(nil))
	must.NoError(t, err)
	c.waitRequest("busy", "created", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return readyReason(obj) == "True"
	})

	// An allocation claims the volume after the operator has read it for
	// the last time, before its register is applied.
	var claim *structs.CSIVolumeClaim
	set(func(*structs.CSIVolume) { claim = c.claim("busy") })
	_, err = c.apply("default", csiVolumeRequestKind, "busy", volumeSpec(func(m map[string]any) { m["capacity_min"] = "2GiB" }))
	must.NoError(t, err)
	st := c.waitRequest("busy", "updated or failed", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return st.Failures > 0 || (c.volume("busy").RequestedCapacityMin == 2*gibibyte && readyReason(obj) == "True")
	})
	obj, _ := c.request("busy")
	must.Zero(t, st.Failures, must.Sprintf("the update failed: %s", volumeRequestFailure(obj.Status)))
	must.Eq(t, "", st.RetryAt)
	must.Zero(t, failedEvents("busy"))

	vol := c.volume("busy")
	must.NotNil(t, claim)
	must.MapContainsKey(t, vol.WriteClaims, claim.AllocationID, must.Sprint("the claim made before the register is lost"))
	must.MapContainsKey(t, vol.WriteAllocs, claim.AllocationID)
	must.Eq(t, 2*gibibyte, vol.Capacity)
	must.Eq(t, 2*gibibyte, vol.RequestedCapacityMin)
	mu.Lock()
	must.Eq(t, 2, registers, must.Sprint("the refused register and the one of the next pass"))
	mu.Unlock()
	// The pass that does it again asks the plugin again: an expansion to a
	// size the volume has is answered without doing anything.
	fake.set(func(f *fakeVolumeCSI) { must.Eq(t, []int64{2 * gibibyte, 2 * gibibyte}, f.expands) })

	// A volume of the ID that is registered by hand after the create, right
	// before the register of the operator, is not written over either.
	set(func(*structs.CSIVolume) {
		c.writeVolume(&structs.CSIVolume{
			ID: "late", Name: "by-hand", Namespace: "default", PluginID: "minnie", ExternalID: "ext-by-hand",
			Capacity: gibibyte, RequestedCapacityMin: gibibyte,
			RequestedCapabilities: []*structs.CSIVolumeCapability{{
				AccessMode: structs.CSIVolumeAccessModeSingleNodeWriter, AttachmentMode: structs.CSIVolumeAttachmentModeFilesystem}},
		})
	})
	_, err = c.apply("default", csiVolumeRequestKind, "late", volumeSpec(nil))
	must.NoError(t, err)
	st = c.waitRequest("late", "conflict", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return readyReason(obj) == volumeRequestConditionConflict || st.VolumeCreateIndex != 0 || st.Failures > 0
	})
	must.Zero(t, st.VolumeCreateIndex)
	must.Zero(t, st.Failures)
	must.Zero(t, failedEvents("late"))
	must.Eq(t, "by-hand", c.volume("late").Name)
	must.Eq(t, "ext-by-hand", c.volume("late").ExternalID)
}

// secondVolumeOperator returns an operator of volume requests of the test's
// server with a plugin of its own: a pass the test drives by hand, beside
// the loop of the server (whose plugin the test keeps failing).
func secondVolumeOperator(s *Server) (*volumeRequestOperator, *fakeVolumeCSI) {
	fake := &fakeVolumeCSI{}
	o := newVolumeRequestOperator(s, s.logger)
	o.csi = fake
	return o, fake
}

// A request that was deleted after the pass read it is not acted on: no
// volume is made for it, also when no status write stands before the call
// of the plugin (the intent is there from a failed attempt).
func TestVolumeRequest_StaleRequestIsNotActedOn(t *testing.T) {
	ci.Parallel(t)
	c, fake, cleanup := volumeRequestTestServer(t, false, func(c *Config) {
		c.volumeRequestRetryBase = time.Hour
	})
	defer cleanup()

	fake.set(func(f *fakeVolumeCSI) { f.createErr = errors.New("the storage is away") })
	_, err := c.apply("default", csiVolumeRequestKind, "stale", volumeSpec(nil))
	must.NoError(t, err)
	c.waitRequest("stale", "creating, failed once", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
		return st.Creating && st.Failures == 1 && st.RetryAt != ""
	})

	// What a pass read, a moment before the request is deleted. Its retry
	// is due in that pass.
	read, _ := c.request("stale")
	must.NoError(t, c.delete("default", csiVolumeRequestKind, "stale"))

	o, plugin := secondVolumeOperator(c.s)
	p := &volumeRequestPass{now: time.Now().UTC().Add(2 * time.Hour), store: c.s.State(), ws: memdb.NewWatchSet()}
	o.reconcileRequest(p, read)
	plugin.set(func(f *fakeVolumeCSI) {
		must.Eq(t, 0, len(f.creates), must.Sprint("a volume was made for a request that was deleted"))
	})
	must.Nil(t, c.volume("stale"))
}

// A server that is no longer the leader makes no call for the requests of
// its pass.
func TestVolumeRequest_PassEndsWithTheLeadership(t *testing.T) {
	ci.Parallel(t)
	c, fake, cleanup := volumeRequestTestServer(t, false, func(c *Config) {
		c.volumeRequestRetryBase = time.Second
	})
	defer cleanup()

	fake.set(func(f *fakeVolumeCSI) { f.createErr = errors.New("the storage is away") })
	for _, name := range []string{"left-a", "left-b"} {
		_, err := c.apply("default", csiVolumeRequestKind, name, volumeSpec(nil))
		must.NoError(t, err)
	}
	for _, name := range []string{"left-a", "left-b"} {
		c.waitRequest(name, "creating, failed", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
			return st.Creating && st.Failures >= 1 && st.RetryAt != ""
		})
	}

	must.NoError(t, c.s.Shutdown())
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool { return !c.s.IsLeader() }),
		wait.Timeout(10*time.Second), wait.Gap(20*time.Millisecond)))

	// The retries of both requests are due in the pass: nothing but the
	// lost leadership keeps it from calling the plugin.
	due := time.Now()
	for _, name := range []string{"left-a", "left-b"} {
		_, st := c.request(name)
		retryAt, err := time.Parse(time.RFC3339, st.RetryAt)
		must.NoError(t, err)
		if retryAt.After(due) {
			due = retryAt
		}
	}
	time.Sleep(time.Until(due) + time.Second)

	o, plugin := secondVolumeOperator(c.s)
	_, err := o.reconcile(memdb.NewWatchSet())
	must.NoError(t, err)
	plugin.set(func(f *fakeVolumeCSI) {
		must.Eq(t, 0, len(f.creates), must.Sprint("a deposed leader called the plugin"))
	})
}

// A status that can't be decoded says which volume is the request's own: it
// is not written over with an empty one, which would take the volume for
// somebody else's, and nothing is done to the volume until it can be
// decoded. A deleted request that retains its volume goes; one that would
// delete it waits (review finding MUK-15).
func TestVolumeRequest_UndecodableStatusIsKept(t *testing.T) {
	ci.Parallel(t)
	c, fake, cleanup := volumeRequestTestServer(t, false)
	defer cleanup()

	for name, policy := range map[string]string{"held": "delete", "kept": "retain"} {
		_, err := c.apply("default", csiVolumeRequestKind, name, volumeSpec(func(m map[string]any) { m["deletion_policy"] = policy }))
		must.NoError(t, err)
	}
	created := map[string]uint64{}
	for _, name := range []string{"held", "kept"} {
		st := c.waitRequest(name, "created", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
			return readyReason(obj) == "True" && st.VolumeCreateIndex > 0
		})
		created[name] = st.VolumeCreateIndex

		// Only the servers write the status: it is written as by a
		// server of another version, whose counter does not fit an int.
		for i := 0; ; i++ {
			must.Less(t, 20, i, must.Sprint("status of the request could not be written"))
			cur := c.get("default", csiVolumeRequestKind, name)
			changed := cur.Copy()
			changed.Status = structs.CopyObjectMap(cur.Status)
			changed.Status["drifts"] = 1e30
			resp, _, err := c.s.raftApply(structs.CustomObjectUpsertRequestType, &structs.CustomObjectUpsertStateRequest{
				Object: changed, ExpectedModifyIndex: cur.ModifyIndex,
				WriteRequest: structs.WriteRequest{Region: "global"},
			})
			if respErr, ok := resp.(error); ok && err == nil {
				err = respErr
			}
			if err == nil {
				break
			}
			must.True(t, structs.IsObjectConflictError(err), must.Sprintf("%v", err))
		}
	}

	kept := func(obj *structs.CustomObject) bool {
		return fmt.Sprint(obj.Status["drifts"]) == "1e+30" &&
			fmt.Sprint(obj.Status["volumeCreateIndex"]) == fmt.Sprint(created[obj.Name])
	}
	var obj *structs.CustomObject
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		obj = c.get("default", csiVolumeRequestKind, "held")
		return readyReason(obj) == "InvalidStatus" || !kept(obj)
	}), wait.Timeout(30*time.Second), wait.Gap(100*time.Millisecond)))
	must.True(t, kept(obj), must.Sprintf("the status was written over: %v", obj.Status))
	must.Eq(t, "InvalidStatus", readyReason(obj))
	must.True(t, objectConditionTrue(obj.Status, structs.CertificateConditionDegraded))

	must.NoError(t, c.delete("default", csiVolumeRequestKind, "kept"))
	must.NoError(t, c.delete("default", csiVolumeRequestKind, "held"))
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		return c.get("default", csiVolumeRequestKind, "kept") == nil
	}), wait.Timeout(20*time.Second), wait.Gap(100*time.Millisecond)), must.Sprint("kept gone"))
	must.NotNil(t, c.volume("kept"))
	time.Sleep(500 * time.Millisecond)
	held := c.get("default", csiVolumeRequestKind, "held")
	must.NotNil(t, held, must.Sprint("a request whose volume is not known went without deleting it"))
	must.True(t, kept(held), must.Sprintf("the status was written over: %v", held.Status))
	must.NotNil(t, c.volume("held"))
	fake.set(func(f *fakeVolumeCSI) { must.Len(t, 0, f.deletes) })
}

// A volume request whose spec its kind refuses can still be deleted (review
// finding ATM-24o). With a spec that can't be decoded the volume stays,
// whatever the deletion policy was: nothing is deleted on the word of a spec
// that can't be read. A spec that can be read is followed.
func TestVolumeRequest_DeleteInvalidSpec(t *testing.T) {
	ci.Parallel(t)

	tests := []struct {
		name  string
		spoil func(spec map[string]any)
		stays bool
	}{
		{name: "a spec that can't be decoded", spoil: func(spec map[string]any) { spec["capacity_min"] = 5 }, stays: true},
		{name: "a spec with a field the kind does not know", spoil: func(spec map[string]any) { spec["unknown"] = true }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, fake, cleanup := volumeRequestTestServer(t, false)
			defer cleanup()
			_, err := c.apply("default", csiVolumeRequestKind, "data", volumeSpec(func(m map[string]any) { m["deletion_policy"] = "delete" }))
			must.NoError(t, err)
			c.waitRequest("data", "created", func(obj *structs.CustomObject, st *volumeRequestStatus) bool {
				return readyReason(obj) == "True" && st.VolumeCreateIndex > 0
			})
			must.NotNil(t, c.volume("data"))

			c.spoilSpec("default", csiVolumeRequestKind, "data", tt.spoil)
			must.NoError(t, c.delete("default", csiVolumeRequestKind, "data"))
			c.goneObject("default", csiVolumeRequestKind, "data")
			must.Eq(t, tt.stays, c.volume("data") != nil)
			fake.set(func(f *fakeVolumeCSI) { must.Eq(t, tt.stays, len(f.deletes) == 0) })
		})
	}
}
