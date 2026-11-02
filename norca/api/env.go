// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import "os"

// EnvPrefix is the prefix of the environment variables that configure the
// client, e.g. NORCA_ADDR or NORCA_TOKEN.
const EnvPrefix = "NORCA_"

// getEnv returns the value of the client environment variable with the given
// suffix (e.g. "ADDR"), that is NORCA_<suffix>. The main module reads the CLI
// settings the same way in helper/clienv.
func getEnv(suffix string) string {
	return os.Getenv(EnvPrefix + suffix)
}
