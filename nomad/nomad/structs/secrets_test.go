// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

func TestSecretMeta_ValidateRotate(t *testing.T) {
	ci.Parallel(t)

	ok := &SecretMeta{RotateEvery: time.Hour, Rotate: &SecretRotate{Job: "rotate-db"}}
	must.NoError(t, ok.Validate())

	// A rotation job without an interval would never be dispatched.
	err := (&SecretMeta{Rotate: &SecretRotate{Job: "rotate-db"}}).Validate()
	must.ErrorContains(t, err, "requires rotate_every")

	for _, job := range []string{"", "with space", "a/b", "nul\000", strings.Repeat("j", 129)} {
		err := (&SecretMeta{RotateEvery: time.Hour, Rotate: &SecretRotate{Job: job}}).Validate()
		must.Error(t, err, must.Sprintf("job %q", job))
	}

	// The copy is deep.
	cp := ok.Copy()
	cp.Rotate.Job = "other"
	must.Eq(t, "rotate-db", ok.Rotate.Job)
	must.Eq(t, "rotate-db", (&Secret{SecretMeta: *ok}).Stub().Rotate.Job)
}
