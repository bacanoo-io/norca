// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

// UnsupportedBlock is a configuration block that is still parsed only so
// that the agent can reject it at startup with a clear message. Its contents
// are ignored.
type UnsupportedBlock struct{}

// unsupportedOptions returns an error message for each agent configuration
// option that is parsed but has no effect in this build. They are rejected at
// startup instead of being silently ignored.
func (c *Config) unsupportedOptions() []string {
	var msgs []string

	if c.Audit != nil && c.Audit.Enabled != nil && *c.Audit.Enabled {
		msgs = append(msgs, "audit logging is not supported: remove audit.enabled = true")
	}

	if c.Sentinel != nil {
		msgs = append(msgs, "sentinel is not supported: remove the sentinel block")
	}

	if c.Server != nil {
		if c.Server.RedundancyZone != "" {
			msgs = append(msgs, "server.redundancy_zone is not supported")
		}
		if c.Server.UpgradeVersion != "" {
			msgs = append(msgs, "server.upgrade_version is not supported")
		}
		if c.Server.LicensePath != "" {
			msgs = append(msgs, "server.license_path is not supported: remove it, no license is needed")
		}
	}

	if ap := c.Autopilot; ap != nil {
		if ap.EnableRedundancyZones != nil && *ap.EnableRedundancyZones {
			msgs = append(msgs, "autopilot.enable_redundancy_zones is not supported")
		}
		if ap.DisableUpgradeMigration != nil && *ap.DisableUpgradeMigration {
			msgs = append(msgs, "autopilot.disable_upgrade_migration is not supported")
		}
		if ap.EnableCustomUpgrades != nil && *ap.EnableCustomUpgrades {
			msgs = append(msgs, "autopilot.enable_custom_upgrades is not supported")
		}
	}

	return msgs
}
