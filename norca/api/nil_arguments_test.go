// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/api/internal/testutil"
	"github.com/shoenig/test/must"
)

// A nil argument is an error, not a panic.
func TestNilArguments(t *testing.T) {
	testutil.Parallel(t)
	c, requests := recordingServer(t, `{}`)

	_, _, err := c.SecretEngines().Write(nil, nil)
	must.ErrorContains(t, err, "missing secret engine")
	_, _, err = c.SecretEngines().CheckedWrite(nil, 1, nil)
	must.ErrorContains(t, err, "missing secret engine")
	_, _, err = c.Operator().Unseal(nil, nil)
	must.ErrorContains(t, err, "missing unseal request")
	must.SliceEmpty(t, requests())
}
