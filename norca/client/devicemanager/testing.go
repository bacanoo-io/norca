// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package devicemanager

import (
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/plugins/base"
	"github.com/bacanoo-io/norca/norca/plugins/device"
)

type ReserveFn func(d *structs.AllocatedDeviceResource) (*device.ContainerReservation, error)
type AllStatsFn func() []*device.DeviceGroupStats
type DeviceStatsFn func(d *structs.AllocatedDeviceResource) (*device.DeviceGroupStats, error)

func NoopReserve(*structs.AllocatedDeviceResource) (*device.ContainerReservation, error) {
	return nil, nil
}

func NoopAllStats() []*device.DeviceGroupStats {
	return nil
}

func NoopDeviceStats(*structs.AllocatedDeviceResource) (*device.DeviceGroupStats, error) {
	return nil, nil
}

func NoopMockManager() *MockManager {
	return &MockManager{
		ReserveF:     NoopReserve,
		AllStatsF:    NoopAllStats,
		DeviceStatsF: NoopDeviceStats,
	}
}

type MockManager struct {
	ReserveF     ReserveFn
	AllStatsF    AllStatsFn
	DeviceStatsF DeviceStatsFn
}

func (m *MockManager) Run()                                 {}
func (m *MockManager) Shutdown()                            {}
func (m *MockManager) PluginType() string                   { return base.PluginTypeDevice }
func (m *MockManager) AllStats() []*device.DeviceGroupStats { return m.AllStatsF() }

func (m *MockManager) Reserve(d *structs.AllocatedDeviceResource) (*device.ContainerReservation, error) {
	return m.ReserveF(d)
}

func (m *MockManager) DeviceStats(d *structs.AllocatedDeviceResource) (*device.DeviceGroupStats, error) {
	return m.DeviceStatsF(d)
}
