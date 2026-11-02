// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"testing"

	"github.com/shoenig/test/must"
)

func TestGetEnv(t *testing.T) {
	t.Setenv("NORCA_ADDR", "")
	must.Eq(t, "", getEnv("ADDR"))

	t.Setenv("NORCA_ADDR", "http://norca:4646")
	must.Eq(t, "http://norca:4646", getEnv("ADDR"))
}

func TestDefaultConfig_NorcaEnv(t *testing.T) {
	t.Setenv("NORCA_ADDR", "http://norca:4646")
	t.Setenv("NORCA_REGION", "norca-region")
	t.Setenv("NORCA_TOKEN", "norca-token")
	t.Setenv("NORCA_SKIP_VERIFY", "false")
	t.Setenv("NORCA_HTTP_AUTH", "user:pass")
	t.Setenv("NORCA_TLS_SERVER_NAME", "server.global.norca")

	config := DefaultConfig()
	must.Eq(t, "http://norca:4646", config.Address)
	must.Eq(t, "norca-region", config.Region)
	must.Eq(t, "norca-token", config.SecretID)
	must.False(t, config.TLSConfig.Insecure)
	must.Eq(t, "server.global.norca", config.TLSConfig.TLSServerName)
	must.NotNil(t, config.HttpAuth)
	must.Eq(t, "user", config.HttpAuth.Username)
	must.Eq(t, "pass", config.HttpAuth.Password)
}
