// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package fingerprint

import (
	"errors"
	"testing"

	"github.com/bacanoo-io/norca/nomad/ci"
	"github.com/bacanoo-io/norca/nomad/helper/testlog"
	"github.com/bacanoo-io/norca/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func TestNetworkPolicyFingerprint(t *testing.T) {
	ci.Parallel(t)

	f := NewNetworkPolicyFingerprint(testlog.HCLogger(t)).(*NetworkPolicyFingerprint)

	f.detector = func() error { return nil }
	resp := new(FingerprintResponse)
	must.NoError(t, f.Fingerprint(&FingerprintRequest{}, resp))
	must.Eq(t, "true", resp.Attributes[structs.NetworkPolicyNftablesAttr])

	f.detector = func() error { return errors.New("operation not permitted") }
	resp = new(FingerprintResponse)
	must.NoError(t, f.Fingerprint(&FingerprintRequest{}, resp))
	_, ok := resp.Attributes[structs.NetworkPolicyNftablesAttr]
	must.True(t, ok) // removal is signalled with an empty value
	must.Eq(t, "", resp.Attributes[structs.NetworkPolicyNftablesAttr])
}
