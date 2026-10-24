// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"slices"

	"github.com/hashicorp/go-version"
	"github.com/hashicorp/serf/serf"
)

// norcaVersionTag is the Serf tag carrying a server's Norca product version.
// The "build" tag keeps the Nomad compatibility version, which Nomad's own
// feature gates compare against.
const norcaVersionTag = "norca_vsn"

// minNorcaFeaturesVersion is the minimum Norca version required before any
// Norca-specific feature (new Raft message types, new state objects) is
// enabled in a region. All Norca features gate on this single value via
// ServersMeetMinimumNorcaVersion.
var minNorcaFeaturesVersion = version.Must(version.NewVersion("0.1.0"))

// ServersMeetMinimumNorcaVersion returns whether all Nomad servers in the
// region (or in all regions for AllRegions) run Norca at least
// minNorcaFeaturesVersion. Servers that do not advertise a Norca version, such
// as upstream Nomad servers during a migration, never meet it. The
// checkFailedServers parameter specifies whether failed servers are checked
// too.
func ServersMeetMinimumNorcaVersion(members []serf.Member, region string, checkFailedServers bool) bool {
	for _, member := range members {
		valid, parts := isNomadServer(member)
		if !valid ||
			(parts.Region != region && region != AllRegions) ||
			!(parts.Status == serf.StatusAlive || (checkFailedServers && parts.Status == serf.StatusFailed)) {
			continue
		}
		if parts.NorcaVersion == nil {
			return false
		}
		// Ignore prerelease metadata, as ServersMeetMinimumVersion does:
		// 0.1.0-dev must pass a 0.1.0 gate
		versionsMatch := slices.Equal(minNorcaFeaturesVersion.Segments(), parts.NorcaVersion.Segments())
		if parts.NorcaVersion.LessThan(minNorcaFeaturesVersion) && !versionsMatch {
			return false
		}
	}
	return true
}
