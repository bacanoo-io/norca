// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package backup

import (
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
)

// SealsOf returns the keyring seals that wrap the keys of a region, for the
// record and the manifest of a backup: the seals of the region and those of
// a rewrap in progress (the keys carry wraps of both), without key checks.
// A region without a seal configuration uses the aead seal.
func SealsOf(store *state.StateStore) []*structs.SealInfo {
	cfg, err := store.SealConfig(nil)
	if err != nil || cfg == nil {
		return []*structs.SealInfo{{Type: structs.SealTypeAEAD}}
	}
	var out []*structs.SealInfo
	for _, s := range cfg.Seals {
		out = append(out, s.Stub())
	}
	if cfg.Rewrap != nil {
		for _, s := range cfg.Rewrap.To {
			out = append(out, s.Stub())
		}
	}
	return out
}
