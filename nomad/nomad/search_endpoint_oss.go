// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

//go:build !ent
// +build !ent

package nomad

import (
	"fmt"

	memdb "github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
)

var (
	// allContexts are the available contexts which are searched to find matches
	// for a given prefix
	allContexts = append(append([]structs.Context{}, ossContexts...), structs.Quotas)
)

// contextToIndex returns the index name to lookup in the state store.
func contextToIndex(ctx structs.Context) string {
	switch ctx {
	// Handle cases where context name and state store table name do not match
	case structs.Variables:
		return state.TableVariables
	case structs.Quotas:
		return state.TableQuotaSpecs
	default:
		return string(ctx)
	}
}

// getEnterpriseMatch returns the ID of objects that are not covered by the
// generic matcher, such as quota specifications.
func getEnterpriseMatch(match interface{}) (id string, ok bool) {
	switch m := match.(type) {
	case *structs.QuotaSpec:
		return m.Name, true
	default:
		return "", false
	}
}

// getEnterpriseResourceIter is used to retrieve an iterator over an enterprise
// only table.
func getEnterpriseResourceIter(context structs.Context, _ *acl.ACL, namespace, prefix string, ws memdb.WatchSet, store *state.StateStore) (memdb.ResultIterator, error) {
	if context == structs.Quotas {
		return store.QuotaSpecsByNamePrefix(ws, prefix, state.SortDefault)
	}

	// If we have made it here then it is an error since we have exhausted all
	// open source contexts.
	return nil, fmt.Errorf("context must be one of %v or 'all' for all contexts; got %q", allContexts, context)
}

// getEnterpriseFuzzyResourceIter is used to retrieve an iterator over an enterprise
// only table.
func getEnterpriseFuzzyResourceIter(context structs.Context, _ *acl.ACL, _ string, _ memdb.WatchSet, _ *state.StateStore) (memdb.ResultIterator, error) {
	return nil, fmt.Errorf("context must be one of %v or 'all' for all contexts; got %q", allContexts, context)
}

func filteredSearchContextsEnt(aclObj *acl.ACL, namespace string, context structs.Context) bool {
	if context == structs.Quotas {
		return aclObj.AllowQuotaRead()
	}
	return true
}
