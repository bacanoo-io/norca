// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package clienv reads the environment variables that configure the CLI,
// e.g. NORCA_ADDR or NORCA_CLI_NO_COLOR. The api module reads the API client
// settings the same way in api/env.go.
package clienv

import (
	"os"
)

// Prefix is the prefix of the Norca CLI environment variables.
const Prefix = "NORCA_"

// Name returns the name of the variable with the given suffix, e.g.
// Name("ADDR") is "NORCA_ADDR".
func Name(suffix string) string {
	return Prefix + suffix
}

// Get returns the value of NORCA_<suffix>. An empty result means the
// variable is unset or empty.
func Get(suffix string) string {
	return os.Getenv(Name(suffix))
}

// GetWithName is Get, and also returns the name of the variable the value
// came from. The name is empty if the variable is unset or empty.
func GetWithName(suffix string) (value, name string) {
	if v := Get(suffix); v != "" {
		return v, Name(suffix)
	}
	return "", ""
}

// Lookup reports whether NORCA_<suffix> is present in the environment, even
// with an empty value, and returns its value. Use it for variables whose
// presence alone has a meaning.
func Lookup(suffix string) (string, bool) {
	return os.LookupEnv(Name(suffix))
}

// LookupName is Lookup for a full variable name.
func LookupName(name string) (string, bool) {
	return os.LookupEnv(name)
}
