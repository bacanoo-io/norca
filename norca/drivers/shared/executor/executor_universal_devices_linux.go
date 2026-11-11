// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build linux

package executor

import (
	"fmt"
	"os"

	"github.com/bacanoo-io/norca/norca/plugins/drivers"
	devconfig "github.com/opencontainers/cgroups/devices/config"
	"golang.org/x/sys/unix"
)

// allowedDeviceRules turns the devices of a command of the universal executor
// (raw_exec allowed_devices, Norca) into cgroup device rules, added to the
// devices every task may use: "*" allows every device, a path the character
// or block device at that path. Without a cgroup device filter (cgroups v1,
// no_cgroups) they change nothing.
func allowedDeviceRules(devs []*drivers.DeviceConfig) ([]*devconfig.Rule, error) {
	var rules []*devconfig.Rule
	for _, d := range devs {
		if d == nil {
			continue
		}
		perms := d.Permissions
		if perms == "" {
			perms = "rwm"
		}
		if d.HostPath == AllDevices {
			rules = append(rules, &devconfig.Rule{
				Type:        devconfig.WildcardDevice,
				Major:       devconfig.Wildcard,
				Minor:       devconfig.Wildcard,
				Permissions: devconfig.Permissions(perms),
				Allow:       true,
			})
			continue
		}
		var st unix.Stat_t
		if err := unix.Stat(d.HostPath, &st); err != nil {
			return nil, fmt.Errorf("allowed device %s: %w", d.HostPath, err)
		}
		var typ devconfig.Type
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFCHR:
			typ = devconfig.CharDevice
		case unix.S_IFBLK:
			typ = devconfig.BlockDevice
		default:
			return nil, fmt.Errorf("allowed device %s: %w", d.HostPath, os.ErrInvalid)
		}
		rules = append(rules, &devconfig.Rule{
			Type:        typ,
			Major:       int64(unix.Major(uint64(st.Rdev))),
			Minor:       int64(unix.Minor(uint64(st.Rdev))),
			Permissions: devconfig.Permissions(perms),
			Allow:       true,
		})
	}
	return rules, nil
}
