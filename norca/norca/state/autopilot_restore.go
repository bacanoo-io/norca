// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"fmt"

	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// AutopilotConfigRestore is used to restore the autopilot configuration from
// a snapshot, with the indexes it had.
func (r *StateRestore) AutopilotConfigRestore(cfg *structs.AutopilotConfig) error {
	if err := r.txn.Insert("autopilot-config", cfg); err != nil {
		return fmt.Errorf("inserting autopilot config failed: %w", err)
	}
	return nil
}
