// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package csimanager

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/hashicorp/nomad/client/pluginmanager"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/plugins/csi"
	"github.com/hashicorp/nomad/testutil"
)

var _ Manager = (*MockCSIManager)(nil)
var _ VolumeManager = (*MockVolumeManager)(nil)

// MockCSIManager is a test double for Manager. Every plugin ID resolves to
// the same MockVolumeManager (VM). Errors can be injected for the next call
// of WaitForPlugin and ManagerForPlugin; an injected error is returned once
// and then cleared.
type MockCSIManager struct {
	VM *MockVolumeManager

	NextWaitForPluginErr    error
	NextManagerForPluginErr error

	lock sync.Mutex
}

// PluginManager is not implemented by the mock and returns nil.
func (m *MockCSIManager) PluginManager() pluginmanager.PluginManager {
	return nil
}

// WaitForPlugin returns immediately, with the injected error if one is set.
func (m *MockCSIManager) WaitForPlugin(_ context.Context, pluginType, pluginID string) error {
	m.lock.Lock()
	defer m.lock.Unlock()
	err := m.NextWaitForPluginErr
	m.NextWaitForPluginErr = nil
	return err
}

// ManagerForPlugin returns VM for any plugin ID, or the injected error if one
// is set.
func (m *MockCSIManager) ManagerForPlugin(_ context.Context, pluginID string) (VolumeManager, error) {
	m.lock.Lock()
	defer m.lock.Unlock()
	if err := m.NextManagerForPluginErr; err != nil {
		m.NextManagerForPluginErr = nil
		return nil, err
	}
	if m.VM == nil {
		return nil, fmt.Errorf("no volume manager for plugin %q", pluginID)
	}
	return m.VM, nil
}

// Shutdown is a no-op.
func (m *MockCSIManager) Shutdown() {}

// MockExpandVolumeCall records the arguments of a call to
// MockVolumeManager.ExpandVolume.
type MockExpandVolumeCall struct {
	VolID     string
	RemoteID  string
	AllocID   string
	UsageOpts *UsageOptions
	Capacity  *csi.CapacityRange
}

// MockVolumeManager is a test double for VolumeManager. It keeps track of
// mounted volumes in Mounts (keyed by the mount source path it hands out),
// counts calls in CallCounter (if set) by method name, records the last
// ExpandVolume call, and returns injected errors from the Next*Err fields.
// An injected error is returned once and then cleared.
type MockVolumeManager struct {
	// CallCounter, if not nil, is incremented with the method name on each
	// call to MountVolume, UnmountVolume, HasMount and ExpandVolume.
	CallCounter *testutil.CallCounter

	// Mounts holds the mount sources that are currently mounted.
	Mounts map[string]bool

	NextMountVolumeErr   error
	NextUnmountVolumeErr error
	NextHasMountErr      error
	NextExpandVolumeErr  error

	// LastExpandVolumeCall holds the arguments of the most recent call to
	// ExpandVolume.
	LastExpandVolumeCall *MockExpandVolumeCall

	lock sync.Mutex
}

// mountSource builds the fake path the mock reports a volume as mounted at.
func (m *MockVolumeManager) mountSource(volID, allocID string, usageOpts *UsageOptions) string {
	return filepath.ToSlash(filepath.Join("test-alloc-dir", allocID, volID, usageOpts.ToFS()))
}

func (m *MockVolumeManager) inc(name string) {
	if m.CallCounter != nil {
		m.CallCounter.Inc(name)
	}
}

// MountVolume records the volume as mounted and returns its mount info.
func (m *MockVolumeManager) MountVolume(_ context.Context, vol *structs.CSIVolume, alloc *structs.Allocation, usageOpts *UsageOptions, publishContext map[string]string) (*MountInfo, error) {
	m.lock.Lock()
	defer m.lock.Unlock()
	m.inc("MountVolume")

	if err := m.NextMountVolumeErr; err != nil {
		m.NextMountVolumeErr = nil
		return nil, err
	}

	source := m.mountSource(vol.ID, alloc.ID, usageOpts)
	if m.Mounts == nil {
		m.Mounts = make(map[string]bool)
	}
	m.Mounts[source] = true

	return &MountInfo{Source: source}, nil
}

// UnmountVolume removes the volume from the recorded mounts.
func (m *MockVolumeManager) UnmountVolume(_ context.Context, volID, remoteID, allocID string, usageOpts *UsageOptions) error {
	m.lock.Lock()
	defer m.lock.Unlock()
	m.inc("UnmountVolume")

	if err := m.NextUnmountVolumeErr; err != nil {
		m.NextUnmountVolumeErr = nil
		return err
	}

	delete(m.Mounts, m.mountSource(volID, allocID, usageOpts))
	return nil
}

// HasMount reports whether the mount source in mountInfo is recorded as
// mounted.
func (m *MockVolumeManager) HasMount(_ context.Context, mountInfo *MountInfo) (bool, error) {
	m.lock.Lock()
	defer m.lock.Unlock()
	m.inc("HasMount")

	if err := m.NextHasMountErr; err != nil {
		m.NextHasMountErr = nil
		return false, err
	}

	if mountInfo == nil {
		return false, nil
	}
	return m.Mounts[mountInfo.Source], nil
}

// ExpandVolume records the call and returns the required capacity of the
// request. The capacity is returned even along with an injected error, so
// that callers which tolerate some errors still see a value.
func (m *MockVolumeManager) ExpandVolume(_ context.Context, volID, remoteID, allocID string, usageOpts *UsageOptions, capacity *csi.CapacityRange) (int64, error) {
	m.lock.Lock()
	defer m.lock.Unlock()
	m.inc("ExpandVolume")

	m.LastExpandVolumeCall = &MockExpandVolumeCall{
		VolID:     volID,
		RemoteID:  remoteID,
		AllocID:   allocID,
		UsageOpts: usageOpts,
		Capacity:  capacity,
	}

	var newCapacity int64
	if capacity != nil {
		newCapacity = capacity.RequiredBytes
	}

	err := m.NextExpandVolumeErr
	m.NextExpandVolumeErr = nil
	return newCapacity, err
}

// ExternalID returns a fixed ID for the mock node.
func (m *MockVolumeManager) ExternalID() string {
	return "mock-volume-manager"
}
