// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build linux

package resources

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/client/lib/cpustats"
	"github.com/shoenig/test/must"
)

func TestBandwidth(t *testing.T) {
	SetSpecs(cpustats.Compute{TotalCompute: 4 * 2500, NumCores: 4})

	// a quarter of a core
	v, err := Bandwidth(625)
	must.NoError(t, err)
	must.Eq(t, uint64(25000), v)

	// 20 MHz on a 2.5 GHz core would be 800 µs, below what the kernel accepts
	v, err = Bandwidth(20)
	must.NoError(t, err)
	must.Eq(t, uint64(minQuota), v)
}
