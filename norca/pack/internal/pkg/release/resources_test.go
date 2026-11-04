// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package release

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/shoenig/test/must"
)

func TestExternalServiceTemplate(t *testing.T) {
	must.Eq(t, api.ReleaseResourceExternalService, TemplateType("templates/orders-db.external.hcl.tpl"))
	must.True(t, IsResourceTemplate("templates/orders-db.external.hcl.tpl"))

	res, err := ParseResource("templates/orders-db.external.hcl.tpl", `
external_service "orders-db" {
  address = "10.20.0.5"
  port    = 5432
}
`)
	must.NoError(t, err)
	must.Eq(t, api.ReleaseResourceExternalService, res.Type)
	must.Eq(t, "orders-db", res.ExternalService.Service.Name)

	// the services come after the admission policies and before the objects
	// and jobs
	resources := []*api.ReleaseResource{
		{Type: api.ReleaseResourceJob},
		{Type: api.ReleaseResourceObject},
		res,
		{Type: api.ReleaseResourceAdmissionPolicy},
	}
	SortResources(resources)
	var order []string
	for _, r := range resources {
		order = append(order, r.Type)
	}
	must.Eq(t, []string{"admission-policy", "external-service", "object", "job"}, order)
}
