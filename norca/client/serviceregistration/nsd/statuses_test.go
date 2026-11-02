// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nsd

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/client/serviceregistration"
	"github.com/bacanoo-io/norca/norca/client/serviceregistration/checks/checkstore"
	"github.com/bacanoo-io/norca/norca/client/state"
	"github.com/bacanoo-io/norca/norca/helper/testlog"
	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

var _ serviceregistration.CheckStatusGetter = (*StatusGetter)(nil)

func TestStatusGetter_Get(t *testing.T) {
	ci.Parallel(t)
	logger := testlog.HCLogger(t)

	db := state.NewMemDB(logger)
	s := checkstore.NewStore(logger, db)

	// setup some sample check results
	id1, id2, id3 := uuid.Short(), uuid.Short(), uuid.Short()
	must.NoError(t, s.Set("allocation1", &structs.CheckQueryResult{
		ID:     structs.CheckID(id1),
		Status: "passing",
	}))
	must.NoError(t, s.Set("allocation1", &structs.CheckQueryResult{
		ID:     structs.CheckID(id2),
		Status: "failing",
	}))
	must.NoError(t, s.Set("allocation2", &structs.CheckQueryResult{
		ID:     structs.CheckID(id3),
		Status: "passing",
	}))

	getter := StatusGetter{shim: s}
	snap, err := getter.Get()
	must.NoError(t, err)
	must.MapEq(t, map[string]string{
		id1: "passing",
		id2: "failing",
		id3: "passing",
	}, snap)
}
