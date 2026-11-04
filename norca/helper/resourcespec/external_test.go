// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package resourcespec

import (
	"testing"

	"github.com/shoenig/test/must"
)

func TestParseExternalService(t *testing.T) {
	item, err := ParseExternalService("orders-db.external.hcl", []byte(`
external_service "orders-db" {
  namespace = "prod"
  address   = "10.20.0.5"
  port      = 5432

  check {
    type = "tcp"
    tcp  = "10.20.0.5:5432"
  }

  checked_by {
    node_pool = "edge"
  }
}
`))
	must.NoError(t, err)
	must.Eq(t, "prod", item.Namespace)
	must.Eq(t, "orders-db", item.Service.Name)
	must.Eq(t, 5432, item.Service.Port)
	must.Eq(t, "edge", item.Service.CheckedBy.NodePool)
	// the servers declare it; the file does not carry the flag
	must.False(t, item.Service.Declared)

	_, err = ParseExternalService("two.external.hcl", []byte(`
external_service "a" {
  address = "10.0.0.1"
}
external_service "b" {
  address = "10.0.0.2"
}
`))
	must.ErrorContains(t, err, "one external_service block")

	_, err = ParseExternalService("bad.external.hcl", []byte("external_service \"a\" {\n  nope = 1\n}\n"))
	must.ErrorContains(t, err, "line 2")
}
