// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package clienv

import (
	"testing"

	"github.com/shoenig/test/must"
)

func TestGet(t *testing.T) {
	t.Setenv("NORCA_ADDR", "")
	t.Setenv("NOMAD_ADDR", "")
	v, name := GetWithName("ADDR")
	must.Eq(t, "", v)
	must.Eq(t, "", name)

	t.Setenv("NOMAD_ADDR", "http://nomad:4646")
	v, name = GetWithName("ADDR")
	must.Eq(t, "http://nomad:4646", v)
	must.Eq(t, "NOMAD_ADDR", name)

	t.Setenv("NORCA_ADDR", "http://norca:4646")
	v, name = GetWithName("ADDR")
	must.Eq(t, "http://norca:4646", v)
	must.Eq(t, "NORCA_ADDR", name)
	must.Eq(t, "http://norca:4646", Get("ADDR"))
}

func TestLookup(t *testing.T) {
	t.Setenv("NOMAD_SKIP_VERIFY", "1")
	v, ok := Lookup("SKIP_VERIFY")
	must.True(t, ok)
	must.Eq(t, "1", v)

	// A present but empty NORCA_ variable wins over NOMAD_.
	t.Setenv("NORCA_SKIP_VERIFY", "")
	v, ok = Lookup("SKIP_VERIFY")
	must.True(t, ok)
	must.Eq(t, "", v)

	_, ok = Lookup("CLIENV_TEST_UNSET")
	must.False(t, ok)
}

func TestLookupName(t *testing.T) {
	t.Setenv("NOMAD_PACK_PLAN_EXIT_CODE_ERROR", "3")
	v, ok := LookupName("NORCA_PACK_PLAN_EXIT_CODE_ERROR")
	must.True(t, ok)
	must.Eq(t, "3", v)

	v, ok = LookupName("NOMAD_PACK_PLAN_EXIT_CODE_ERROR")
	must.True(t, ok)
	must.Eq(t, "3", v)

	t.Setenv("NORCA_PACK_PLAN_EXIT_CODE_ERROR", "4")
	v, _ = LookupName("NOMAD_PACK_PLAN_EXIT_CODE_ERROR")
	must.Eq(t, "4", v)

	t.Setenv("CLIENV_TEST_OTHER", "x")
	v, ok = LookupName("CLIENV_TEST_OTHER")
	must.True(t, ok)
	must.Eq(t, "x", v)
}

func TestNames(t *testing.T) {
	must.Eq(t, "NORCA_TOKEN", Name("TOKEN"))
	must.Eq(t, "NOMAD_TOKEN", CompatName("TOKEN"))
}
