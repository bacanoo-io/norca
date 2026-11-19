// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
)

// The identity of an allocation of a dispatched or periodic child names the
// parent job (Allocation.ToIdentityClaims), and so does the SPIFFE ID of its
// certificate. SignCertificate compared the claim with the job of the
// allocation and refused every child (review finding MUK-28).
func TestMTLS_SignChildJob(t *testing.T) {
	ci.Parallel(t)
	srv, cleanup := TestServer(t, func(c *Config) { c.NumSchedulers = 0 })
	defer cleanup()
	testutil.WaitForLeader(t, srv.RPC)
	waitForMTLSRoot(t, srv)

	alloc, token := mtlsTestAlloc(t, srv, func(job *structs.Job) {
		job.ParentID = "batch-parent"
	})

	var reply structs.MTLSSignResponse
	err := srv.RPC("MTLS.SignCertificate", &structs.MTLSSignRequest{
		AllocID:       alloc.ID,
		TaskName:      "web",
		IdentityToken: token,
		CSR:           newTestCSR(t),
		WriteRequest:  structs.WriteRequest{Region: "global"},
	}, &reply)
	must.NoError(t, err)
	must.Eq(t, "spiffe://global.norca/ns/default/job/batch-parent/group/web", reply.SPIFFEID)
}
