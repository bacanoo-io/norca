// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"testing"

	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func TestStateStore_SealConfig(t *testing.T) {
	ci.Parallel(t)
	s := testStateStore(t)

	cfg, err := s.SealConfig(nil)
	must.NoError(t, err)
	must.Nil(t, cfg)

	ws := memdb.NewWatchSet()
	_, err = s.SealConfig(ws)
	must.NoError(t, err)

	in := &structs.SealConfig{
		Seals: []*structs.SealInfo{{Type: structs.SealTypeAEAD}},
		Rewrap: &structs.SealRewrap{
			ID: "rw1",
			To: []*structs.SealInfo{{Type: structs.SealTypeShares, KeyID: "k1", Threshold: 2, Shares: 3}},
		},
	}
	must.NoError(t, s.UpsertSealConfig(structs.SealConfigUpsertRequestType, 10, in))
	must.True(t, watchFired(ws))
	must.Eq(t, uint64(0), in.CreateIndex, must.Sprint("the input is not modified"))

	cfg, err = s.SealConfig(nil)
	must.NoError(t, err)
	must.Eq(t, uint64(10), cfg.CreateIndex)
	must.Eq(t, uint64(10), cfg.ModifyIndex)
	must.Eq(t, "rw1", cfg.Rewrap.ID)

	// Progress of the rewrap; other rewraps and repeats are ignored.
	must.NoError(t, s.SealRewrapDone(structs.SealRewrapDoneRequestType, 11, "rw1", "b"))
	must.NoError(t, s.SealRewrapDone(structs.SealRewrapDoneRequestType, 12, "rw1", "a"))
	must.NoError(t, s.SealRewrapDone(structs.SealRewrapDoneRequestType, 13, "rw1", "a"))
	must.NoError(t, s.SealRewrapDone(structs.SealRewrapDoneRequestType, 14, "other", "c"))
	cfg, err = s.SealConfig(nil)
	must.NoError(t, err)
	must.Eq(t, []string{"a", "b"}, cfg.Rewrap.Done)
	must.Eq(t, uint64(12), cfg.ModifyIndex)

	// A sealed server is recorded apart; once it takes part it moves to
	// done, and a done server never goes back to sealed.
	must.NoError(t, s.SealRewrapReport(structs.SealRewrapDoneRequestType, 15, "rw1", "c", true))
	must.NoError(t, s.SealRewrapReport(structs.SealRewrapDoneRequestType, 16, "rw1", "a", true))
	cfg, err = s.SealConfig(nil)
	must.NoError(t, err)
	must.Eq(t, []string{"c"}, cfg.Rewrap.Sealed)
	must.NoError(t, s.SealRewrapReport(structs.SealRewrapDoneRequestType, 17, "rw1", "c", false))
	cfg, err = s.SealConfig(nil)
	must.NoError(t, err)
	must.Eq(t, []string{"a", "b", "c"}, cfg.Rewrap.Done)
	must.SliceEmpty(t, cfg.Rewrap.Sealed)

	// Completion.
	must.NoError(t, s.UpsertSealConfig(structs.SealConfigUpsertRequestType, 20, &structs.SealConfig{
		Seals: cfg.Rewrap.To, LastRewrapID: "rw1",
	}))
	cfg, err = s.SealConfig(nil)
	must.NoError(t, err)
	must.Nil(t, cfg.Rewrap)
	must.Eq(t, uint64(10), cfg.CreateIndex)
	must.Eq(t, uint64(20), cfg.ModifyIndex)
	must.Eq(t, "k1", cfg.Seals[0].KeyID)

	index, err := s.Index(TableSealConfig)
	must.NoError(t, err)
	must.Eq(t, uint64(20), index)
}
