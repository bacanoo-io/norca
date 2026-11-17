// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"testing"
	"time"

	"github.com/shoenig/test/must"
)

// The exported fields invite a struct literal; it must work like one made
// by NewExponentialBackoff.
func TestExponentialBackoff_StructLiteral(t *testing.T) {
	r := &ExponentialBackoff[string]{Base: time.Second, Max: 4 * time.Second}
	must.Eq(t, 0, r.NumRequeues("a"))
	r.Forget("a")
	must.Eq(t, time.Second, r.When("a"))
	must.Eq(t, 2*time.Second, r.When("a"))
	must.Eq(t, 4*time.Second, r.When("a"))
	must.Eq(t, 4*time.Second, r.When("a"))
	must.Eq(t, 4, r.NumRequeues("a"))
	r.Forget("a")
	must.Eq(t, time.Second, r.When("a"))
}
