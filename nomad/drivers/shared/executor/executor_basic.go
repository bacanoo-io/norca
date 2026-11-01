// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build !linux

package executor

import (
	"os/exec"

	"github.com/bacanoo-io/norca/nomad/client/lib/resources"
	"github.com/bacanoo-io/norca/nomad/plugins/drivers"
	hclog "github.com/hashicorp/go-hclog"
)

func NewExecutorWithIsolation(logger hclog.Logger, cpuTotalTicks uint64) Executor {
	logger = logger.Named("executor")
	logger.Error("isolation executor is not supported on this platform, using default")
	return NewExecutor(logger, cpuTotalTicks)
}

func (e *UniversalExecutor) configureResourceContainer(_ int) error { return nil }

func (e *UniversalExecutor) getAllPids() (resources.PIDs, error) {
	return getAllPidsByScanning()
}

func (e *UniversalExecutor) start(command *ExecCommand) error {
	return e.childCmd.Start()
}

func withNetworkIsolation(f func() error, _ *drivers.NetworkIsolationSpec) error {
	return f()
}

func setCmdUser(*exec.Cmd, string) error { return nil }
