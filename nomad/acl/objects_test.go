// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package acl

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

func TestObjectACL_Parse(t *testing.T) {
	ci.Parallel(t)

	p, err := Parse(`
namespace "prod" {
  object "db.example.com/PostgresCluster" {
    capabilities = ["read", "write"]
  }
  object "db.example.com/Backup" {
    policy = "read"
  }
}
namespace "*" {
  object "db.example.com/*" {
    capabilities = ["read", "write", "write-status", "write-events", "manage-finalizers"]
  }
}`)
	must.NoError(t, err)
	must.Len(t, 2, p.Namespaces)
	must.Len(t, 2, p.Namespaces[0].Objects)
	must.Eq(t, "db.example.com/PostgresCluster", p.Namespaces[0].Objects[0].Kind)
	must.Eq(t, []string{"read"}, p.Namespaces[0].Objects[1].Capabilities)

	invalid := []string{
		`namespace "prod" { object "db.example.com/X" { capabilities = ["destroy"] } }`,
		`namespace "prod" { object "db.example.com/X" { policy = "scale" } }`,
		`namespace "prod" { object "no-kind" { policy = "read" } }`,
		`namespace "prod" { object { policy = "read" } }`,
		`namespace "prod" { object "db.example.com/X" {} }`,
	}
	for _, rules := range invalid {
		_, err := Parse(rules)
		must.Error(t, err, must.Sprint(rules))
	}
}

func TestObjectACL_Allow(t *testing.T) {
	ci.Parallel(t)

	user, err := Parse(`
namespace "prod" {
  object "db.example.com/PostgresCluster" {
    capabilities = ["read", "write"]
  }
  object "db.example.com/Secret" {
    policy = "deny"
  }
}
operator { policy = "read" }`)
	must.NoError(t, err)
	controller, err := Parse(`
namespace "*" {
  object "db.example.com/*" {
    capabilities = ["read", "write-status"]
  }
}`)
	must.NoError(t, err)

	userACL, err := NewACL(false, []*Policy{user})
	must.NoError(t, err)
	ctrlACL, err := NewACL(false, []*Policy{controller})
	must.NoError(t, err)

	const pg = "db.example.com/PostgresCluster"

	must.True(t, userACL.AllowObjectOperation("prod", pg, ObjectCapabilityRead))
	must.True(t, userACL.AllowObjectOperation("prod", pg, ObjectCapabilityWrite))
	must.False(t, userACL.AllowObjectOperation("prod", pg, ObjectCapabilityWriteStatus))
	must.False(t, userACL.AllowObjectOperation("dev", pg, ObjectCapabilityRead))
	must.False(t, userACL.AllowObjectOperation("prod", "db.example.com/Other", ObjectCapabilityRead))
	must.False(t, userACL.AllowObjectOperation("prod", "db.example.com/Secret", ObjectCapabilityRead))
	must.False(t, userACL.AllowObjectOperation(ObjectClusterNamespace, pg, ObjectCapabilityRead))

	must.True(t, ctrlACL.AllowObjectOperation("prod", pg, ObjectCapabilityRead))
	must.True(t, ctrlACL.AllowObjectOperation("dev", pg, ObjectCapabilityWriteStatus))
	must.False(t, ctrlACL.AllowObjectOperation("dev", pg, ObjectCapabilityWrite))
	must.False(t, ctrlACL.AllowObjectOperation("dev", "lb.example.com/LoadBalancer", ObjectCapabilityRead))
	// Cluster-scoped objects use the rules of namespace "*".
	must.True(t, ctrlACL.AllowObjectOperation(ObjectClusterNamespace, pg, ObjectCapabilityRead))
	// A glob subscription key is granted only if a rule covers it.
	must.True(t, ctrlACL.AllowObjectOperation("*", "db.example.com/*", ObjectCapabilityRead))
	must.False(t, ctrlACL.AllowObjectOperation("*", "*", ObjectCapabilityRead))

	must.True(t, userACL.AllowObjectOperationInAnyNamespace(pg, ObjectCapabilityRead))
	must.False(t, userACL.AllowObjectOperationInAnyNamespace("db.example.com/Secret", ObjectCapabilityRead))
	must.True(t, ctrlACL.AllowObjectOperationInAnyNamespace("db.example.com/Anything", ObjectCapabilityRead))

	// Kind definitions are readable with operator read or with any object
	// capability on the kind; writing them requires operator write.
	must.True(t, userACL.AllowObjectKindRead("lb.example.com/LoadBalancer"))
	must.False(t, userACL.AllowObjectKindWrite())
	must.True(t, ctrlACL.AllowObjectKindRead(pg))
	must.False(t, ctrlACL.AllowObjectKindRead("lb.example.com/LoadBalancer"))
	must.False(t, ctrlACL.AllowObjectKindWrite())

	var disabled *ACL
	must.True(t, disabled.AllowObjectOperation("prod", pg, ObjectCapabilityWrite))
	must.True(t, disabled.AllowObjectKindWrite())
	must.True(t, ManagementACL.AllowObjectOperation("prod", pg, ObjectCapabilityWriteStatus))
	must.True(t, ManagementACL.AllowObjectKindWrite())
}
