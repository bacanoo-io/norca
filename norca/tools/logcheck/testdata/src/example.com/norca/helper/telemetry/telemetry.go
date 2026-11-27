// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import hclog "github.com/hashicorp/go-hclog"

func f(logger hclog.Logger) {
	logger.Info("span", "trace_id", "abc", "span_id", "def")
}
