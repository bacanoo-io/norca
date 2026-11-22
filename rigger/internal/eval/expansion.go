// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
)

// maxBlocks bounds the blocks the dynamic blocks of one evaluation make:
// nested ones multiply, so a few lines make billions.
const maxBlocks = 100_000

// expansion counts the blocks dynamic blocks make, as they are made. A nil
// expansion counts nothing: a body that was walked under a bound already.
type expansion struct {
	blocks int
	// over is the diagnostic of the block over the bound, set once.
	over *hcl.Diagnostic
}

// take counts one more block, and reports whether it is within the bound.
func (x *expansion) take(rng hcl.Range) bool {
	if x == nil {
		return true
	}

	if x.over != nil {
		return false
	}

	if x.blocks >= maxBlocks {
		x.over = &hcl.Diagnostic{
			Severity: hcl.DiagError, Summary: "Too many blocks",
			Detail:  fmt.Sprintf("The dynamic blocks make more than %d blocks, the limit of the engine.", maxBlocks),
			Subject: rng.Ptr(),
		}

		return false
	}

	x.blocks++

	return true
}

// exceeded is the error of the bound, once it is exceeded.
func (x *expansion) exceeded() hcl.Diagnostics {
	if x == nil || x.over == nil {
		return nil
	}

	return hcl.Diagnostics{x.over}
}
