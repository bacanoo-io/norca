// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"net/http"
	"slices"

	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// A tenant makes ACL policies and roles with the rights of the servers, so
// writing one is as strong as a management token: the object ACL of the kind
// decides who may (object write on norca.io/Tenant), and an operator grants
// it knowingly. What is checked here is the shape of the spec, that two
// tenants never keep one namespace, and that the namespace of a tenant does
// not change.

// checkTenantApply checks a tenant written through the object API. obj has
// the defaults of its schema; existing is the stored object or nil.
func checkTenantApply(snap *state.StateSnapshot, existing, obj *structs.CustomObject) error {
	spec, err := decodeTenantSpec(obj.Name, obj.Spec)
	if err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "invalid tenant: %v", err)
	}
	if err := spec.validate(obj.Name); err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "invalid tenant: %v", err)
	}
	if existing != nil {
		if before := tenantNamespace(existing); before != "" && before != spec.Namespace.Name {
			return structs.NewErrRPCCodedf(http.StatusBadRequest,
				"the namespace of a tenant can't be changed (%q to %q): delete the tenant and create it again", before, spec.Namespace.Name)
		}
	}

	tenants, err := snap.CustomObjects(nil, structs.OperatorsGroup, structs.TenantKind, "", state.SortDefault)
	if err != nil {
		return err
	}
	for raw := tenants.Next(); raw != nil; raw = tenants.Next() {
		other := raw.(*structs.CustomObject)
		if other.Name != obj.Name && tenantNamespace(other) == spec.Namespace.Name {
			return structs.NewErrRPCCodedf(http.StatusConflict,
				"the namespace %q belongs to the tenant %q", spec.Namespace.Name, other.Name)
		}
	}

	// The servers delete what the tenant created before the object goes.
	if !obj.Deleting() && !slices.Contains(obj.Finalizers, structs.OperatorsFinalizer) {
		obj.Finalizers = append(obj.Finalizers, structs.OperatorsFinalizer)
	}
	return nil
}

// checkTenantStatusWrite refuses status writes through the object API: the
// status of a tenant is the servers'.
func checkTenantStatusWrite() error {
	return structs.NewErrRPCCodedf(http.StatusForbidden, "the status of a tenant is written by the servers only")
}

// tenantOwners maps the keys of the resources tenants created, in the form of
// the keys of release resources (type/namespace/name), to the tenant.
func tenantOwners(snap *state.StateSnapshot) (map[string]string, error) {
	iter, err := snap.CustomObjects(nil, structs.OperatorsGroup, structs.TenantKind, "", state.SortDefault)
	if err != nil {
		return nil, err
	}
	owners := map[string]string{}
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		obj := raw.(*structs.CustomObject)
		status := &tenantStatus{}
		if err := structs.DecodeObjectPart(obj.Status, status); err != nil {
			continue
		}
		for _, ref := range status.Resources {
			owners[ref.key()] = obj.Name
		}
	}
	return owners, nil
}
