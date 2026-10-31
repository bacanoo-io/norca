// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"testing"

	"github.com/shoenig/test/must"
)

// Groups that differ only in dots and dashes have distinct owner references,
// so a job owned by an object of one kind is not collected with the object
// of the other kind, even without the owner kind meta.
func TestObjectOwnerMetaValue_GroupsDoNotCollide(t *testing.T) {
	must.Eq(t, "prod/crd/db-example-com/Cluster/a", ObjectOwnerMetaValue("db.example.com", "Cluster", "prod", "a"))
	must.Eq(t, "prod/crd/db--example-com/Cluster/a", ObjectOwnerMetaValue("db-example.com", "Cluster", "prod", "a"))

	seen := map[string]string{}
	for _, group := range []string{"db.example.com", "db-example-com", "db-example.com", "db.example-com", "a-b.c", "a.b-c", "a--b"} {
		must.NoError(t, ValidateObjectKindName(group, "Cluster"))
		v := ObjectOwnerMetaValue(group, "Cluster", "prod", "a")
		other, dup := seen[v]
		must.False(t, dup, must.Sprintf("%s and %s map to %s", group, other, v))
		seen[v] = group
	}

	meta := map[string]string{JobMetaOwner: ObjectOwnerMetaValue("db-example-com", "Cluster", "prod", "a")}
	must.True(t, JobOwnedBy(meta, "db-example-com", "Cluster", "prod", "a"))
	must.False(t, JobOwnedBy(meta, "db.example.com", "Cluster", "prod", "a"))
}
