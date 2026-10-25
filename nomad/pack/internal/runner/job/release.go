// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package job

import (
	"github.com/hashicorp/nomad/api"

	"github.com/hashicorp/nomad/pack/terminal"
)

// FormatJobDiff prints a job diff like the job plan does, for the plans of
// pack releases.
func FormatJobDiff(diff *api.JobDiff, verbose bool, ui terminal.UI) {
	if diff != nil {
		formatJobDiff(*diff, verbose, ui)
	}
}
