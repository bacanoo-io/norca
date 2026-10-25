// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import "os"

const (
	// EnvPrefix is the prefix of the environment variables that configure
	// the client, e.g. NORCA_ADDR or NORCA_TOKEN.
	EnvPrefix = "NORCA_"

	// EnvPrefixCompat is the prefix of the environment variables inherited
	// from Nomad, e.g. NOMAD_ADDR. They are still read when the NORCA_
	// variable of the same name is unset or empty.
	EnvPrefixCompat = "NOMAD_"
)

// getEnv returns the value of the client environment variable with the given
// suffix (e.g. "ADDR"): NORCA_<suffix> if it is set to a non-empty value,
// otherwise NOMAD_<suffix>. The main module mirrors this precedence in
// helper/clienv; keep the two in sync.
func getEnv(suffix string) string {
	if v := os.Getenv(EnvPrefix + suffix); v != "" {
		return v
	}
	return os.Getenv(EnvPrefixCompat + suffix)
}
