// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build linux

package resources

import (
	"errors"
	"sync"
	"time"

	"github.com/bacanoo-io/norca/norca/client/lib/cpustats"
)

type MicroSecond uint64

type Percent float64

type Utilization struct {
	Memory uint64
	Swap   uint64
	Cache  uint64

	System          Percent
	User            Percent
	Percent         Percent
	ThrottlePeriods uint64
	ThrottleTime    uint64
	Ticks           Percent
}

type TrackCPU struct {
	prevTime   time.Time
	prevUser   MicroSecond
	prevSystem MicroSecond
	prevTotal  MicroSecond
}

// Percent returns the percentage of time spent in user, system, total CPU usage.
func (t *TrackCPU) Percent(user, system, total MicroSecond) (Percent, Percent, Percent) {
	now := time.Now()

	if t.prevUser == 0 && t.prevSystem == 0 {
		t.prevUser = user
		t.prevSystem = system
		t.prevTotal = total
		return 0.0, 0.0, 0.0
	}

	elapsed := now.Sub(t.prevTime).Microseconds()
	userPct := t.percent(t.prevUser, user, elapsed)
	systemPct := t.percent(t.prevSystem, system, elapsed)
	totalPct := t.percent(t.prevTotal, total, elapsed)
	t.prevUser = user
	t.prevSystem = system
	t.prevTotal = total
	t.prevTime = now
	return userPct, systemPct, totalPct
}

func (t *TrackCPU) percent(t1, t2 MicroSecond, elapsed int64) Percent {
	delta := t2 - t1
	if elapsed <= 0 || delta <= 0.0 {
		return 0.0
	}
	return Percent(float64(delta)/float64(elapsed)) * 100.0
}

type Specs struct {
	MHz   uint64
	Cores int
}

func (s Specs) Ticks() uint64 {
	return uint64(s.Cores) * s.MHz
}

var (
	lock  sync.Mutex
	specs Specs
)

func SetSpecs(compute cpustats.Compute) {
	// without the CPU fingerprint there is no per-core speed; Bandwidth
	// then refuses to start tasks rather than divide by zero
	if !compute.Usable() {
		return
	}
	perCore := uint64(compute.PerCoreMHz())

	lock.Lock()
	specs.MHz = perCore
	specs.Cores = compute.NumCores
	lock.Unlock()
}

func GetSpecs() Specs {
	lock.Lock()
	s := specs
	lock.Unlock()

	return s
}

// Bandwidth computes the CPU bandwidth given a mhz value from task config.
// We assume the bandwidth per-core base is 100_000 which is the default.
func Bandwidth(mhz uint64) (uint64, error) {
	speed := GetSpecs().MHz
	if speed == 0 {
		return 0, errors.New("the CPU speed of the node is unknown")
	}
	v := (mhz * 100000) / speed
	// The kernel does not accept a cpu.max quota below 1 ms per period
	// (EINVAL), which a small cpu value on a fast core gives.
	return max(v, minQuota), nil
}

// minQuota is the smallest cpu.max quota the kernel accepts, in µs.
const minQuota = 1000
