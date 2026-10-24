// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package checkstore

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/client/state"
	"github.com/hashicorp/nomad/helper/testlog"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func TestShim_Reset(t *testing.T) {
	ci.Parallel(t)
	logger := testlog.HCLogger(t)

	db := state.NewMemDB(logger)
	s := NewStore(logger, db)

	ok := newQR("id1", success)
	ok.Role = structs.CheckRoleStartup
	ok.Output = "nomad: http ok"
	must.NoError(t, s.Set("alloc1", ok))
	must.NoError(t, s.Set("alloc1", newQR("id2", failure)))
	must.NoError(t, s.Set("alloc1", newQR("id3", success)))

	// unknown ids are ignored
	must.NoError(t, s.Reset("alloc1", []structs.CheckID{"id1", "id2", "missing"}))
	must.NoError(t, s.Reset("other", []structs.CheckID{"id1"}))

	m := s.List("alloc1")
	must.Eq(t, pending, m["id1"].Status)
	must.Eq(t, structs.CheckRoleStartup, m["id1"].Role)
	must.Eq(t, "nomad: waiting to run", m["id1"].Output)
	must.Eq(t, pending, m["id2"].Status)
	must.Eq(t, success, m["id3"].Status)

	// the original result was not modified in place
	must.Eq(t, success, ok.Status)

	// reset is persisted
	restored := NewStore(logger, db).List("alloc1")
	must.Eq(t, pending, restored["id1"].Status)
	must.Eq(t, success, restored["id3"].Status)

	// a new result replaces the pending one
	must.NoError(t, s.Set("alloc1", newQR("id1", success)))
	must.Eq(t, success, s.List("alloc1")["id1"].Status)
}
