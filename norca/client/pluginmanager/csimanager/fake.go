// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package csimanager

import (
	"context"
	"errors"
	"path"
	"sync"

	"github.com/bacanoo-io/norca/norca/client/pluginmanager"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/plugins/csi"
)

// Test fakes of Manager and VolumeManager for packages that drive the CSI
// manager (the alloc runner's CSI hook, the client CSI endpoint). They are
// in a regular file so that those packages' tests can use them.

var (
	_ Manager       = (*FakeManager)(nil)
	_ VolumeManager = (*FakeVolumeManager)(nil)
)

// Faults makes chosen calls of a fake fail: FailNext(method, err) makes the
// next call of that method return err, once. The zero value injects
// nothing and is safe for concurrent use.
type Faults struct {
	mu      sync.Mutex
	pending map[string]error
}

// FailNext arms a one-shot error for the next call of method.
func (f *Faults) FailNext(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pending == nil {
		f.pending = map[string]error{}
	}
	f.pending[method] = err
}

// fault returns and disarms the error armed for method, if any.
func (f *Faults) fault(method string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	err := f.pending[method]
	delete(f.pending, method)
	return err
}

// FakeManager resolves every plugin to the same FakeVolumeManager. Its own
// calls (WaitForPlugin, ManagerForPlugin) can fail through Faults.
type FakeManager struct {
	Faults
	Volumes *FakeVolumeManager
}

// NewFakeManager returns a FakeManager with an empty FakeVolumeManager.
func NewFakeManager() *FakeManager {
	return &FakeManager{Volumes: &FakeVolumeManager{}}
}

// PluginManager is not used by the callers under test.
func (m *FakeManager) PluginManager() pluginmanager.PluginManager { return nil }

// WaitForPlugin never waits.
func (m *FakeManager) WaitForPlugin(context.Context, string, string) error {
	return m.fault("WaitForPlugin")
}

// ManagerForPlugin hands out Volumes whatever the plugin.
func (m *FakeManager) ManagerForPlugin(_ context.Context, pluginID string) (VolumeManager, error) {
	if err := m.fault("ManagerForPlugin"); err != nil {
		return nil, err
	}
	if m.Volumes == nil {
		return nil, errors.New("fake CSI manager has no volume manager for " + pluginID)
	}
	return m.Volumes, nil
}

// Shutdown has nothing to release.
func (m *FakeManager) Shutdown() {}

// ExpandRequest is one ExpandVolume call as the FakeVolumeManager saw it.
type ExpandRequest struct {
	VolID     string
	RemoteID  string
	AllocID   string
	UsageOpts *UsageOptions
	Capacity  *csi.CapacityRange
}

// FakeVolumeManager mounts volumes in memory. OnCall, if set, is told the
// name of every MountVolume, UnmountVolume, HasMount and ExpandVolume call
// (e.g. testutil.CallCounter.Inc); Faults can make any of them fail once.
type FakeVolumeManager struct {
	Faults
	OnCall func(method string)

	mu         sync.Mutex
	mounted    map[string]struct{}
	expansions []ExpandRequest
}

// FakeMountSource is where the fake reports a volume of an allocation as
// mounted, for the given usage.
func FakeMountSource(allocID, volID string, usage *UsageOptions) string {
	return path.Join("fake-mounts", volID+"@"+allocID, usage.ToFS())
}

// MarkMounted records mount sources as mounted, as if an earlier
// MountVolume had produced them.
func (v *FakeVolumeManager) MarkMounted(sources ...string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.ensure()
	for _, source := range sources {
		v.mounted[source] = struct{}{}
	}
}

// Expansions returns the ExpandVolume calls seen so far, oldest first.
func (v *FakeVolumeManager) Expansions() []ExpandRequest {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]ExpandRequest(nil), v.expansions...)
}

// begin counts the call and returns the armed fault, if any.
func (v *FakeVolumeManager) begin(method string) error {
	if v.OnCall != nil {
		v.OnCall(method)
	}
	return v.fault(method)
}

func (v *FakeVolumeManager) ensure() {
	if v.mounted == nil {
		v.mounted = map[string]struct{}{}
	}
}

func (v *FakeVolumeManager) MountVolume(_ context.Context, vol *structs.CSIVolume, alloc *structs.Allocation, usage *UsageOptions, _ map[string]string) (*MountInfo, error) {
	if err := v.begin("MountVolume"); err != nil {
		return nil, err
	}
	source := FakeMountSource(alloc.ID, vol.ID, usage)
	v.MarkMounted(source)
	return &MountInfo{Source: source}, nil
}

func (v *FakeVolumeManager) UnmountVolume(_ context.Context, volID, _, allocID string, usage *UsageOptions) error {
	if err := v.begin("UnmountVolume"); err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.mounted, FakeMountSource(allocID, volID, usage))
	return nil
}

func (v *FakeVolumeManager) HasMount(_ context.Context, info *MountInfo) (bool, error) {
	if err := v.begin("HasMount"); err != nil {
		return false, err
	}
	if info == nil {
		return false, nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	_, ok := v.mounted[info.Source]
	return ok, nil
}

// ExpandVolume grants the required capacity. The capacity is reported even
// when a fault is armed, for callers that tolerate some errors.
func (v *FakeVolumeManager) ExpandVolume(_ context.Context, volID, remoteID, allocID string, usage *UsageOptions, capacity *csi.CapacityRange) (int64, error) {
	err := v.begin("ExpandVolume")
	v.mu.Lock()
	v.expansions = append(v.expansions, ExpandRequest{
		VolID: volID, RemoteID: remoteID, AllocID: allocID,
		UsageOpts: usage, Capacity: capacity,
	})
	v.mu.Unlock()
	var granted int64
	if capacity != nil {
		granted = capacity.RequiredBytes
	}
	return granted, err
}

func (v *FakeVolumeManager) ExternalID() string { return "fake-csi-node" }
