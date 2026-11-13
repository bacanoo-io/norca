// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build linux

package resources

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bacanoo-io/norca/norca/client/lib/cgutil"
	"github.com/hashicorp/go-hclog"
	"github.com/opencontainers/cgroups"
	// device management for cgroups v1 and v2 is a separate package that must
	// be linked in; without it, setting the v2 device rules below fails
	_ "github.com/opencontainers/cgroups/devices"
	"github.com/opencontainers/cgroups/fs2"
)

type containment struct {
	lock   sync.RWMutex
	cgroup *cgroups.Cgroup
	logger hclog.Logger
}

func Contain(logger hclog.Logger, cgroup *cgroups.Cgroup) *containment {
	return &containment{
		cgroup: cgroup,
		logger: logger.Named("containment"),
	}
}

func (c *containment) Apply(pid int) error {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.logger.Trace("create containment for", "cgroup", c.cgroup, "pid", pid)

	// for v2 use manager to create and enter the cgroup
	if cgutil.UseV2 {
		mgr, err := fs2.NewManager(c.cgroup, "")
		if err != nil {
			return fmt.Errorf("failed to create v2 cgroup manager for containment: %w", err)
		}

		// add the pid to the cgroup
		if err = mgr.Apply(pid); err != nil {
			return fmt.Errorf("failed to apply v2 cgroup containment: %w", err)
		}

		// in v2 it is important to set the device resource configuration
		if err = mgr.Set(c.cgroup.Resources); err != nil {
			return fmt.Errorf("failed to set v2 cgroup resources: %w", err)
		}

		return nil
	}

	// for v1 a random cgroup was created already; just enter it
	if err := cgutil.EnterPidV1(c.cgroup.Path, pid); err != nil {
		return fmt.Errorf("failed to add pid to v1 cgroup: %w", err)
	}

	return nil
}

func (c *containment) Cleanup() error {
	c.lock.Lock()
	defer c.lock.Unlock()

	// the current pid is of the executor, who manages the task process cleanup
	executorPID := os.Getpid()
	c.logger.Trace("cleanup on", "cgroup", c.cgroup, "executor_pid", executorPID)

	// destroy the task processes
	destroyer := cgutil.NewGroupKiller(c.logger, executorPID)
	return destroyer.KillGroup(c.cgroup)
}

func (c *containment) GetPIDs() PIDs {
	c.lock.Lock()
	defer c.lock.Unlock()

	m := make(PIDs)
	if c.cgroup == nil {
		return m
	}

	// get the cgroup path under containment
	var path string
	if cgutil.UseV2 {
		path = filepath.Join(cgutil.CgroupRoot, c.cgroup.Path)
	} else {
		path = c.cgroup.Path
	}

	// find the pids in the cgroup under containment
	pids, err := cgroups.GetAllPids(path)
	if err != nil {
		c.logger.Debug("failed to get pids", "cgroup", c.cgroup, "error", err)
		return m
	}

	for _, pid := range pids {
		m[pid] = NewPID(pid)
	}

	return m
}

// CPUTime returns the CPU time of the cgroup under containment. Only the
// cgroup of v2 accounts for it: in v1 the containment is a freezer cgroup.
//
// The cgroup counts every process that ran in it, so a task that keeps
// starting processes shorter than the scan of the pid collector is measured
// in full; the sum of the processes found by the scan misses them.
func (c *containment) CPUTime() (user, system time.Duration, ok bool) {
	c.lock.RLock()
	defer c.lock.RUnlock()

	if c.cgroup == nil || !cgutil.UseV2 {
		return 0, 0, false
	}
	f, err := os.Open(filepath.Join(cgutil.CgroupRoot, c.cgroup.Path, "cpu.stat"))
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	return parseCPUStat(bufio.NewScanner(f))
}

// parseCPUStat reads user_usec and system_usec of the cpu.stat file of a v2
// cgroup; it is false when either is missing.
func parseCPUStat(lines *bufio.Scanner) (user, system time.Duration, ok bool) {
	var hasUser, hasSystem bool
	for lines.Scan() {
		key, value, found := strings.Cut(strings.TrimSpace(lines.Text()), " ")
		if !found {
			continue
		}
		usec, err := strconv.ParseInt(value, 10, 64)
		if err != nil || usec < 0 {
			continue
		}
		switch key {
		case "user_usec":
			user, hasUser = time.Duration(usec)*time.Microsecond, true
		case "system_usec":
			system, hasSystem = time.Duration(usec)*time.Microsecond, true
		}
	}
	return user, system, hasUser && hasSystem
}
