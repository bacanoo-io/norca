// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func TestFSM_SealConfig(t *testing.T) {
	ci.Parallel(t)
	fsm := testFSM(t)

	buf, err := structs.Encode(structs.SealConfigUpsertRequestType|structs.IgnoreUnknownTypeFlag,
		structs.SealConfigUpsertRequest{Config: &structs.SealConfig{
			Seals: []*structs.SealInfo{{Type: structs.SealTypeAEAD}},
			Rewrap: &structs.SealRewrap{ID: "rw1", To: []*structs.SealInfo{
				{Type: structs.SealTypeShares, KeyID: "k1", Threshold: 2, Shares: 3, KeyCheck: []byte("check")},
			}},
		}})
	must.NoError(t, err)
	must.Nil(t, fsm.Apply(makeLog(buf)))

	buf, err = structs.Encode(structs.SealRewrapDoneRequestType|structs.IgnoreUnknownTypeFlag,
		structs.SealRewrapDoneRequest{RewrapID: "rw1", ServerID: "s1"})
	must.NoError(t, err)
	must.Nil(t, fsm.Apply(makeLog(buf)))

	cfg, err := fsm.State().SealConfig(nil)
	must.NoError(t, err)
	must.NotNil(t, cfg)
	must.Eq(t, []string{"s1"}, cfg.Rewrap.Done)
	must.Eq(t, []byte("check"), cfg.Rewrap.To[0].KeyCheck)

	fsm2 := testSnapshotRestore(t, fsm)
	got, err := fsm2.State().SealConfig(nil)
	must.NoError(t, err)
	must.Eq(t, cfg, got)
}
