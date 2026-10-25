// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package clienv reads the environment variables that configure the CLI,
// e.g. NORCA_ADDR or NORCA_CLI_NO_COLOR. Every variable has a NORCA_ name and
// a NOMAD_ name inherited from Nomad; the NORCA_ variable wins and the NOMAD_
// variable is the fallback. The api module implements the same precedence for
// the API client settings in api/env.go.
//
// Only the CLI's own settings use this package. Variables that Norca injects
// into workloads (NOMAD_ALLOC_ID, NOMAD_TASK_DIR, ...) and the jobspec
// variable prefix NOMAD_VAR_ keep their Nomad names.
package clienv

import (
	"os"
	"strings"
)

const (
	// Prefix is the prefix of the Norca CLI environment variables.
	Prefix = "NORCA_"

	// CompatPrefix is the prefix of the Nomad CLI environment variables,
	// which are still accepted as a fallback.
	CompatPrefix = "NOMAD_"
)

// Name returns the documented name of the variable with the given suffix,
// e.g. Name("ADDR") is "NORCA_ADDR".
func Name(suffix string) string {
	return Prefix + suffix
}

// CompatName returns the Nomad name of the variable with the given suffix,
// e.g. CompatName("ADDR") is "NOMAD_ADDR".
func CompatName(suffix string) string {
	return CompatPrefix + suffix
}

// Get returns NORCA_<suffix> if it is set to a non-empty value, otherwise
// NOMAD_<suffix>. An empty result means neither is set to a value.
func Get(suffix string) string {
	v, _ := GetWithName(suffix)
	return v
}

// GetWithName is Get, and also returns the name of the variable the value
// came from. The name is empty if neither variable is set to a value.
func GetWithName(suffix string) (value, name string) {
	for _, n := range []string{Name(suffix), CompatName(suffix)} {
		if v := os.Getenv(n); v != "" {
			return v, n
		}
	}
	return "", ""
}

// Lookup reports whether NORCA_<suffix> or NOMAD_<suffix> is present in the
// environment, even with an empty value, and returns the value of the first
// one present in that order. Use it for variables whose presence alone has a
// meaning.
func Lookup(suffix string) (string, bool) {
	if v, ok := os.LookupEnv(Name(suffix)); ok {
		return v, true
	}
	return os.LookupEnv(CompatName(suffix))
}

// LookupName is Lookup for a full variable name. A name with the NORCA_ or
// NOMAD_ prefix is looked up as both, NORCA_ first; any other name is looked
// up as is.
func LookupName(name string) (string, bool) {
	for _, p := range []string{Prefix, CompatPrefix} {
		if suffix, ok := strings.CutPrefix(name, p); ok {
			return Lookup(suffix)
		}
	}
	return os.LookupEnv(name)
}
