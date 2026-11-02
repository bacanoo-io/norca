// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build !linux
// +build !linux

package allocrunner

import (
	clientconfig "github.com/bacanoo-io/norca/norca/client/config"
	"github.com/bacanoo-io/norca/norca/client/pluginmanager/drivermanager"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/plugins/drivers"
	hclog "github.com/hashicorp/go-hclog"
)

// noopNetworkManager implements the drivers.DriverNetoworkManager interface to
// provide a no-op manager for systems that don't support network isolation.
type noopNetworkManager struct{}

func (*noopNetworkManager) CreateNetwork(_ string, _ *drivers.NetworkCreateRequest) (*drivers.NetworkIsolationSpec, bool, error) {
	return nil, false, nil
}

func (*noopNetworkManager) DestroyNetwork(_ string, _ *drivers.NetworkIsolationSpec) error {
	return nil
}

// TODO: Support windows shared networking
func newNetworkManager(alloc *structs.Allocation, driverManager drivermanager.Manager) (nm drivers.DriverNetworkManager, err error) {
	return &noopNetworkManager{}, nil
}

func newNetworkConfigurator(log hclog.Logger, alloc *structs.Allocation, config *clientconfig.Config) (NetworkConfigurator, error) {
	return &hostNetworkConfigurator{}, nil
}
