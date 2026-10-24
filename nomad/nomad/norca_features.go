// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"github.com/hashicorp/go-version"
)

// minNorcaFeaturesVersion is the minimum server version required before any
// Norca-specific feature (new Raft message types, new state objects) is
// enabled in a region. All Norca features gate on this single value via
// ServersMeetMinimumVersion.
//
// TODO: the fork's versioning scheme is not decided yet. Until it is, this is
// the base version, so the gate only rejects servers older than v1.6.5.
var minNorcaFeaturesVersion = version.Must(version.NewVersion("1.6.5"))
