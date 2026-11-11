// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build linux

package executor

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/plugins/drivers"
	devconfig "github.com/opencontainers/cgroups/devices/config"
	"github.com/shoenig/test/must"
)

// TestAllowedDeviceRules checks the cgroup rules of raw_exec allowed_devices:
// a device path gives its type and numbers, "*" every device, anything else
// is refused.
func TestAllowedDeviceRules(t *testing.T) {
	ci.Parallel(t)

	rules, err := allowedDeviceRules(nil)
	must.NoError(t, err)
	must.SliceEmpty(t, rules)

	// /dev/null is the character device 1:3 everywhere.
	rules, err = allowedDeviceRules([]*drivers.DeviceConfig{
		{HostPath: "/dev/null", TaskPath: "/dev/null", Permissions: "rw"},
		{HostPath: AllDevices, TaskPath: AllDevices},
	})
	must.NoError(t, err)
	must.Len(t, 2, rules)
	must.Eq(t, &devconfig.Rule{Type: devconfig.CharDevice, Major: 1, Minor: 3, Permissions: "rw", Allow: true}, rules[0])
	must.Eq(t, &devconfig.Rule{Type: devconfig.WildcardDevice, Major: devconfig.Wildcard, Minor: devconfig.Wildcard, Permissions: "rwm", Allow: true}, rules[1])

	_, err = allowedDeviceRules([]*drivers.DeviceConfig{{HostPath: "/dev/does-not-exist"}})
	must.Error(t, err)
	_, err = allowedDeviceRules([]*drivers.DeviceConfig{{HostPath: t.TempDir()}})
	must.Error(t, err)
}
