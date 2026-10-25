// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package resourcespec

import (
	"testing"

	"github.com/shoenig/test/must"
)

func TestParseNamespaceAndQuota(t *testing.T) {
	ns, err := ParseNamespace([]byte(`
name        = "team"
description = "Team namespace"
quota       = "team-quota"
meta { environment = "production" }
`))
	must.NoError(t, err)
	must.Eq(t, "team", ns.Name)
	must.Eq(t, "team-quota", ns.Quota)
	must.Eq(t, "production", ns.Meta["environment"])

	q, err := ParseQuota([]byte(`
name = "team-quota"
limit {
  region = "global"
  region_limit { cpu = 2000 memory = 1024 }
}
`))
	must.NoError(t, err)
	must.Eq(t, "team-quota", q.Name)
	must.Eq(t, 2000, *q.Limits[0].RegionLimit.CPU)
}

func TestParseVariableWithSecrets(t *testing.T) {
	v, secrets, err := ParseVariableWithSecrets([]byte(`
path = "config/redis"
items { user = "admin" }
secret_items = ["password"]
`))
	must.NoError(t, err)
	must.Eq(t, "config/redis", v.Path)
	must.Eq(t, map[string]string{"user": "admin"}, v.Items)
	must.Eq(t, []string{"password"}, secrets)

	_, _, err = ParseVariableWithSecrets([]byte(`
path = "x"
items { password = "plain" }
secret_items = ["password"]
`))
	must.ErrorContains(t, err, "must not have a value")
}

func TestParseACLPolicy(t *testing.T) {
	p, err := ParseACLPolicy("read.acl.hcl", []byte(`
policy "redis-read" {
  description = "read"
  rules = <<EOT
namespace "default" { policy = "read" }
EOT
  job_acl {
    job_id = "redis"
  }
}
`))
	must.NoError(t, err)
	must.Eq(t, "redis-read", p.Name)
	must.StrContains(t, p.Rules, `policy = "read"`)
	must.Eq(t, "redis", p.JobACL.JobID)
}

func TestParseAdmissionPolicyAndObjects(t *testing.T) {
	ap, err := ParseAdmissionPolicy("p.admission.hcl", []byte(`
name = "signed"
match {
  namespaces = ["prod"]
  resources  = ["release"]
}
trusted_keys = ["-----BEGIN PUBLIC KEY-----\nabc\n-----END PUBLIC KEY-----\n"]
`))
	must.NoError(t, err)
	must.Eq(t, []string{"release"}, ap.Match.Resources)
	must.Len(t, 1, ap.TrustedKeys)

	k, err := ParseObjectKind("w.kind.hcl", []byte(`
kind "Widget" {
  group  = "example.com"
  schema = jsonencode({ type = "object" })
}
`), false)
	must.NoError(t, err)
	must.Eq(t, "example.com/Widget", k.KindRef())
	must.True(t, k.Namespaced)

	o, err := ParseCustomObject("w.object.hcl", []byte(`
kind = "example.com/Widget"
name = "w1"
spec { size = 2 }
`), false)
	must.NoError(t, err)
	must.Eq(t, "w1", o.Name)
	must.Eq[any](t, float64(2), o.Spec["size"])
}
