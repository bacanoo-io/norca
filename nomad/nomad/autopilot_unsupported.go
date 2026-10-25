// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"fmt"
	"strings"

	"github.com/hashicorp/nomad/nomad/structs"
)

// validateAutopilotSupported returns an error if the autopilot configuration
// enables redundancy zones, disables upgrade migration or enables custom
// upgrades. The autopilot of this build uses the default promoter, which
// ignores these options, so they are rejected instead of silently accepted.
func validateAutopilotSupported(conf *structs.AutopilotConfig) error {
	var unsupported []string
	if conf.EnableRedundancyZones {
		unsupported = append(unsupported, "enable_redundancy_zones")
	}
	if conf.DisableUpgradeMigration {
		unsupported = append(unsupported, "disable_upgrade_migration")
	}
	if conf.EnableCustomUpgrades {
		unsupported = append(unsupported, "enable_custom_upgrades")
	}
	if len(unsupported) > 0 {
		return fmt.Errorf("autopilot options are not supported: %s", strings.Join(unsupported, ", "))
	}
	return nil
}
