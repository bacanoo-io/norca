// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

func TestSealConfig_StubAndRealSeal(t *testing.T) {
	ci.Parallel(t)

	var nilCfg *SealConfig
	must.False(t, nilCfg.RealSeal())
	must.False(t, (&SealConfig{Seals: []*SealInfo{{Type: SealTypeAEAD}}}).RealSeal())

	cfg := &SealConfig{
		Seals: []*SealInfo{{Type: SealTypeAEAD}},
		Rewrap: &SealRewrap{
			ID: "rw1",
			To: []*SealInfo{{Type: SealTypeShares, KeyID: "k2", KeyCheck: []byte("check")}},
			Keys: []*SealKeyWraps{{Seal: SealTypeShares, KeyID: "k2", Wraps: []*SealWrappedKey{
				{Seal: SealWrapKeyring, KeyID: "root", Ciphertext: []byte("wrapped")},
			}}},
			Sealed: []string{"s3"},
		},
	}
	must.True(t, cfg.RealSeal())

	// The key wraps and key checks never leave the server.
	stub := cfg.Stub()
	must.Nil(t, stub.Rewrap.Keys)
	must.Nil(t, stub.Rewrap.To[0].KeyCheck)
	must.Eq(t, []string{"s3"}, stub.Rewrap.Sealed)
	must.Len(t, 1, cfg.Rewrap.Keys, must.Sprint("the original is not modified"))

	// Copies are deep.
	cp := cfg.Copy()
	cp.Rewrap.Keys[0].Wraps[0].Ciphertext[0] = 'X'
	cp.Rewrap.Sealed[0] = "other"
	must.Eq(t, []byte("wrapped"), cfg.Rewrap.Keys[0].Wraps[0].Ciphertext)
	must.Eq(t, "s3", cfg.Rewrap.Sealed[0])
}
