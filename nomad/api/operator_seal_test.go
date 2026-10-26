// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"testing"
	"time"

	"github.com/hashicorp/nomad/api/internal/testutil"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

func TestOperator_SealRewrapUnseal(t *testing.T) {
	testutil.Parallel(t)

	c, s := makeClient(t, nil, nil)
	defer s.Stop()

	// A dev agent uses the implicit aead seal.
	status, _, err := c.Operator().SealStatus("", nil)
	must.NoError(t, err)
	must.Nil(t, status.Config)
	must.Len(t, 1, status.Servers)
	must.False(t, status.Servers[0].Sealed)
	must.Eq(t, []string{SealTypeAEAD}, status.Servers[0].Seals)

	resp, wm, err := c.Keyring().Rewrap(&KeyringRewrapRequest{To: SealTypeShares, Shares: 3, Threshold: 2}, nil)
	must.NoError(t, err)
	assertWriteMeta(t, wm)
	must.Len(t, 3, resp.Shares)
	must.NotEq(t, "", resp.RewrapID)

	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		status, _, err = c.Operator().SealStatus("", nil)
		return err == nil && status.Config != nil && status.Config.Rewrap == nil &&
			len(status.Config.Seals) == 1 && status.Config.Seals[0].Type == SealTypeShares
	}), wait.Timeout(10*time.Second), wait.Gap(50*time.Millisecond)))
	must.Eq(t, 2, status.Config.Seals[0].Threshold)

	// The running server is unsealed: a share is accepted and ignored.
	st, _, err := c.Operator().Unseal(&UnsealRequest{Key: resp.Shares[0]}, nil)
	must.NoError(t, err)
	must.False(t, st.Sealed)

	_, _, err = c.Operator().Unseal(&UnsealRequest{Key: "not-a-share"}, nil)
	must.Error(t, err)
}
