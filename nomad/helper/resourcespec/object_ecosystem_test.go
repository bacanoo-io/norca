// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package resourcespec

import (
	"testing"
	"time"

	"github.com/shoenig/test/must"
)

func TestParseObjectKind_Ecosystem(t *testing.T) {
	src := `
kind "LoadBalancer" {
  group   = "norca.io"
  schema  = "{}"
  version = 2

  migration {
    spec   = "self.spec.merge({'class': 'nomad'})"
    status = "{}"
  }

  webhook "validate" {
    url            = "https://lb.example.com:8443/validate"
    ca_cert        = "-----BEGIN CERTIFICATE-----"
    timeout        = "3s"
    failure_policy = "ignore"
    operations     = ["create", "update", "delete"]
  }

  webhook "default" {
    mode = "default"
    url  = "https://lb.example.com:8443/default"
  }

  limits {
    write_rate                = 50
    write_burst               = 100
    identity_write_rate       = 5.5
    max_objects_per_namespace = 10
    max_watches_per_identity  = 4
  }
}`
	kind, err := ParseObjectKind("lb.kind.hcl", []byte(src), false)
	must.NoError(t, err)
	must.Eq(t, uint64(2), kind.Version)
	must.Eq(t, "{}", kind.Migration.Status)
	must.Len(t, 2, kind.Webhooks)
	v := kind.Webhooks[0]
	must.Eq(t, "validate", v.Name)
	must.Eq(t, 3*time.Second, v.Timeout)
	must.Eq(t, "ignore", v.FailurePolicy)
	must.Eq(t, []string{"create", "update", "delete"}, v.Operations)
	must.Eq(t, "default", kind.Webhooks[1].Mode)
	must.Eq(t, 5.5, kind.Limits.IdentityWriteRate)
	must.Eq(t, 100, kind.Limits.WriteBurst)
	must.Eq(t, 4, kind.Limits.MaxWatchesPerIdentity)

	_, err = ParseObjectKind("lb.kind.hcl", []byte(`
kind "X" {
  group  = "a.b"
  schema = "{}"
  webhook "w" {
    url     = "https://a"
    timeout = "soon"
  }
}`), false)
	must.ErrorContains(t, err, "invalid timeout")
}
