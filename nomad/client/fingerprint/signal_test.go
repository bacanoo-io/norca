// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package fingerprint

import (
	"testing"

	"github.com/bacanoo-io/norca/nomad/ci"
	"github.com/bacanoo-io/norca/nomad/helper/testlog"
	"github.com/bacanoo-io/norca/nomad/nomad/structs"
)

func TestSignalFingerprint(t *testing.T) {
	ci.Parallel(t)

	fp := NewSignalFingerprint(testlog.HCLogger(t))
	node := &structs.Node{
		Attributes: make(map[string]string),
	}

	response := assertFingerprintOK(t, fp, node)
	assertNodeAttributeContains(t, response.Attributes, "os.signals")
}
