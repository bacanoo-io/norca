// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package clienv

import (
	"testing"

	"github.com/shoenig/test/must"
)

func TestGet(t *testing.T) {
	t.Setenv("NORCA_ADDR", "")
	v, name := GetWithName("ADDR")
	must.Eq(t, "", v)
	must.Eq(t, "", name)

	t.Setenv("NORCA_ADDR", "http://norca:4646")
	v, name = GetWithName("ADDR")
	must.Eq(t, "http://norca:4646", v)
	must.Eq(t, "NORCA_ADDR", name)
	must.Eq(t, "http://norca:4646", Get("ADDR"))
}

func TestLookup(t *testing.T) {
	_, ok := Lookup("CLIENV_TEST_UNSET")
	must.False(t, ok)

	// A present but empty variable is found.
	t.Setenv("NORCA_SKIP_VERIFY", "")
	v, ok := Lookup("SKIP_VERIFY")
	must.True(t, ok)
	must.Eq(t, "", v)

	t.Setenv("NORCA_SKIP_VERIFY", "1")
	v, ok = Lookup("SKIP_VERIFY")
	must.True(t, ok)
	must.Eq(t, "1", v)
}

func TestLookupName(t *testing.T) {
	t.Setenv("NORCA_PACK_PLAN_EXIT_CODE_ERROR", "3")
	v, ok := LookupName("NORCA_PACK_PLAN_EXIT_CODE_ERROR")
	must.True(t, ok)
	must.Eq(t, "3", v)

	t.Setenv("CLIENV_TEST_OTHER", "x")
	v, ok = LookupName("CLIENV_TEST_OTHER")
	must.True(t, ok)
	must.Eq(t, "x", v)
}

func TestNames(t *testing.T) {
	must.Eq(t, "NORCA_TOKEN", Name("TOKEN"))
}
