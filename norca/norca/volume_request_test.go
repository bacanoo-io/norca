// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/plugins/csi"
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
	must.Eq(t, "ext-db", st.ExternalID)
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
		must.Eq(t, []string{"db"}, f.creates)
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
	fake.set(func(f *fakeVolumeCSI) {
		must.Eq(t, []string{"vol-0abc"}, f.validates)
		must.SliceNotContains(t, f.creates, "outside")
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
	fake.set(func(f *fakeVolumeCSI) { must.Eq(t, []string{"ext-drop-me"}, f.deletes) })
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

	obj := &structs.CustomObject{Namespace: "prod", Name: "db"}
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
	must.Eq(t, "db", vol.Name)
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
