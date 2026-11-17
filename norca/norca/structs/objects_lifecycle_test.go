// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"strings"
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

// The fields of an owner reference are bounded (review finding SRV-11): they
// are stored with the object and are not part of its size limit.
func TestObjectOwnerReference_Validate(t *testing.T) {
	for _, ref := range []*ObjectOwnerReference{
		{Kind: "db.example.com/Cluster", Name: "a"},
		{Kind: "db.example.com/Cluster", Namespace: "prod", Name: "a.b-c_d", ID: "5a2c7e0c-8e0f-4f1a-9a57-3d0c2b5c9a11"},
		{Kind: ObjectOwnerKindJob, Namespace: "prod", Name: "batch/dispatch-1700000000-abcdef"},
		{Kind: "Variable", Namespace: "prod", Name: "app/config"},
	} {
		must.NoError(t, ref.Validate(), must.Sprintf("%+v", ref))
	}

	long := strings.Repeat("a", 1<<20)
	for name, ref := range map[string]*ObjectOwnerReference{
		"long object name":    {Kind: "db.example.com/Cluster", Name: long},
		"invalid object name": {Kind: "db.example.com/Cluster", Name: "a b"},
		"long job name":       {Kind: ObjectOwnerKindJob, Name: long},
		"control character":   {Kind: ObjectOwnerKindJob, Name: "a\x00b"},
		"long namespace":      {Kind: "db.example.com/Cluster", Namespace: long, Name: "a"},
		"invalid namespace":   {Kind: ObjectOwnerKindJob, Namespace: "a/b", Name: "a"},
		"long ID":             {Kind: "db.example.com/Cluster", Name: "a", ID: long},
	} {
		must.Error(t, ref.Validate(), must.Sprint(name))
	}

	obj := &CustomObject{Name: "x"}
	for i := 0; i < maxObjectOwners; i++ {
		obj.Owners = append(obj.Owners, &ObjectOwnerReference{Kind: "db.example.com/Cluster", Namespace: long, Name: long})
	}
	must.Error(t, obj.ValidateMetadata())
}
