// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package externalspec

import (
	"errors"
	"testing"
	"time"

	"github.com/shoenig/test/must"
)

const sample = `
external_service "orders-db" {
  namespace = "prod"
  address   = "10.20.0.5"
  port      = 5432
  tags      = ["postgres", "primary"]
  meta      = { tier = "prod" }

  deregister_critical_after = "2h"

  check {
    type     = "tcp"
    tcp      = "10.20.0.5:5432"
    interval = "10s"
    timeout  = "2s"
  }

  check {
    id     = "http"
    type   = "http"
    http   = "https://10.20.0.5/health"
    header = { "X-Probe" = ["a", "b"] }
  }

  checked_by {
    node_pool   = "edge"
    datacenters = ["dc1", "eu-*"]
  }
}

external_service "billing-db" {
  type   = "alias"
  target = "billing.c9xk2.eu-central-1.rds.amazonaws.com"
}
`

func TestParse(t *testing.T) {
	items, err := Parse("sample", []byte(sample), false)
	must.NoError(t, err)
	must.Len(t, 2, items)

	db := items[0]
	must.Eq(t, "prod", db.Namespace)
	must.Eq(t, "orders-db", db.Service.Name)
	must.Eq(t, "address", db.Service.Kind)
	must.Eq(t, "10.20.0.5", db.Service.Address)
	must.Eq(t, 5432, db.Service.Port)
	must.Eq(t, "prod", db.Service.Meta["tier"])
	must.True(t, db.Service.Declared)
	must.Eq(t, 2*time.Hour, *db.Service.DeregisterCriticalAfter)
	must.Len(t, 2, db.Service.Checks)
	must.Eq(t, 2*time.Second, db.Service.Checks[0].Timeout)
	must.Eq(t, []string{"a", "b"}, db.Service.Checks[1].Header["X-Probe"])
	must.Eq(t, "edge", db.Service.CheckedBy.NodePool)
	must.Eq(t, []string{"dc1", "eu-*"}, db.Service.CheckedBy.Datacenters)

	alias := items[1]
	must.Eq(t, "alias", alias.Service.Kind)
	must.Eq(t, "billing.c9xk2.eu-central-1.rds.amazonaws.com", alias.Service.Target)
}

func TestFormatRoundTrip(t *testing.T) {
	items, err := Parse("sample", []byte(sample), false)
	must.NoError(t, err)

	text := Format(items)
	again, err := Parse("formatted", text, false)
	must.NoError(t, err, must.Sprintf("formatted text:\n%s", text))
	must.Eq(t, items, again, must.Sprintf("formatted text:\n%s", text))
}

func TestParseJSON(t *testing.T) {
	items, err := Parse("x.json", []byte(`[
		{"Namespace":"prod","Name":"a","Address":"10.0.0.1","Port":80},
		{"Name":"b","Kind":"alias","Target":"b.example.com"}]`), true)
	must.NoError(t, err)
	must.Len(t, 2, items)
	must.Eq(t, "prod", items[0].Namespace)
	must.Eq(t, 80, items[0].Service.Port)
	must.True(t, items[1].Service.Declared)

	_, err = Parse("x.json", []byte(`{"Address":"10.0.0.1"}`), true)
	must.Error(t, err)
}

func TestParseErrors(t *testing.T) {
	for name, src := range map[string]string{
		"empty":    ``,
		"bad type": `external_service "a" { type = "weird" }`,
		"target on address": `external_service "a" {
  address = "x"
  target  = "y"
}`,
		"address on alias": `external_service "a" {
  type    = "alias"
  address = "10.0.0.1"
}`,
		"bad duration": `external_service "a" {
  address                   = "x"
  deregister_critical_after = "soon"
}`,
		"duplicate": `external_service "a" {
  address = "x"
}
external_service "a" {
  address = "y"
}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse("e", []byte(src), false)
			must.Error(t, err)
			var pe *Error
			must.True(t, errors.As(err, &pe))
		})
	}

	_, err := Parse("e", []byte("external_service \"a\" {\n  address = \"x\"\n  nope = 1\n}\n"), false)
	var pe *Error
	must.True(t, errors.As(err, &pe))
	must.Eq(t, 3, pe.Problems[0].Line)
}
