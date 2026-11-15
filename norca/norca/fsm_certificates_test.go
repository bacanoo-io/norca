// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

func TestFSM_CertificateAuthorities(t *testing.T) {
	ci.Parallel(t)

	fsm := testFSM(t)
	active := &structs.CertificateAuthority{
		ID: "ca-2", Region: "global", Cert: "cert-2", Active: true,
		SigningKey: []byte("encrypted"), SigningKeyID: "key-1",
		NotAfter: time.Unix(2000, 0).UTC(),
		Revoked:  []*structs.CertificateRevocation{{Serial: "0a", Certificate: "default/web", NotAfter: time.Unix(1500, 0).UTC()}},
	}
	retired := &structs.CertificateAuthority{ID: "ca-1", Region: "global", Cert: "cert-1", SigningKey: []byte("old"), SigningKeyID: "key-1"}
	gone := &structs.CertificateAuthority{ID: "ca-0", Region: "global", Cert: "cert-0"}

	apply := func(req *structs.CertificateAuthoritiesUpsertRequest) {
		t.Helper()
		buf, err := structs.Encode(structs.CertificateAuthoritiesUpsertRequestType, req)
		must.NoError(t, err)
		must.Nil(t, fsm.Apply(makeLog(buf)))
	}
	apply(&structs.CertificateAuthoritiesUpsertRequest{Authorities: []*structs.CertificateAuthority{active, retired, gone}})
	apply(&structs.CertificateAuthoritiesUpsertRequest{DeleteIDs: []string{"ca-0", "missing"}})

	check := func(fsm *nomadFSM) {
		t.Helper()
		all, err := fsm.State().CertificateAuthorities(nil)
		must.NoError(t, err)
		must.Len(t, 2, all)
		must.Eq(t, "ca-1", all[0].ID)
		must.Eq(t, "ca-2", all[1].ID)
		must.Eq(t, "encrypted", string(all[1].SigningKey))
		must.Eq(t, "0a", all[1].Revoked[0].Serial)
		must.True(t, all[1].Active)

		// The root key that encrypts the keys of the CAs stays in use.
		inUse, err := fsm.State().IsRootKeyMetaInUse("key-1")
		must.NoError(t, err)
		must.True(t, inUse)
		uses, err := fsm.State().RootKeyMetaUses("key-1")
		must.NoError(t, err)
		must.Eq(t, 2, uses.CertificateAuthorities)
		inUse, err = fsm.State().IsRootKeyMetaInUse("key-2")
		must.NoError(t, err)
		must.False(t, inUse)
	}
	check(fsm)

	// A snapshot carries the CAs with their encrypted keys.
	check(testSnapshotRestore(t, fsm))
}
