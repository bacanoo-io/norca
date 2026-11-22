// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package fetch

import (
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/rigger/internal/targz/targztest"
)

// A sparse file in the layer of an OCI module would fill the disk: the bound
// of a layer counts its bytes, not what they write.
func TestUntar_Sparse(t *testing.T) {
	t.Parallel()

	if err := untar(targztest.SparseTar("big", 8<<20), t.TempDir()); err == nil || !strings.Contains(err.Error(), "sparse") {
		t.Errorf("untar() = %v, want the sparse file refused", err)
	}
}
