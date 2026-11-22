// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/rigger/catalog"
	"github.com/bacanoo-io/norca/rigger/index"
)

// Rigger is the endpoint of Rigger, the package manager, in the servers
// (design/rig-integration.md 4.5 and 4.6). The registries themselves are
// objects of the kind rigger.norca.io/Registry, written through the object
// API; this endpoint syncs and searches them.
type Rigger struct {
	srv *Server
	ctx *RPCContext
}

// NewRiggerEndpoint returns the endpoint of Rigger.
func NewRiggerEndpoint(srv *Server, ctx *RPCContext) *Rigger {
	return &Rigger{srv: srv, ctx: ctx}
}

// RiggerRegistryNameRequest names a registry.
type RiggerRegistryNameRequest struct {
	Name string
	structs.WriteRequest
}

// RiggerRegistryResponse is a registry after a sync.
type RiggerRegistryResponse struct {
	Registry *RiggerRegistry
	structs.WriteMeta
}

// RiggerRegistrySearchRequest searches the index of a registry at the
// commit of its last good sync.
type RiggerRegistrySearchRequest struct {
	Name string
	// Kind is module or stack; empty finds both.
	Kind string
	// Text is found in "namespace/name".
	Text          string
	Offset, Limit int
	structs.QueryOptions
}

// RiggerRegistrySearchResponse is a page of a search.
type RiggerRegistrySearchResponse struct {
	Items []catalog.Item
	// Total is how many match in all; Commit is the commit searched.
	Total  int
	Commit string
	structs.QueryMeta
}

// SyncRegistry fetches the source of a registry now, reads its index and
// writes what it found into its status. It needs write on the kind of
// registries. The sync runs on the leader.
func (r *Rigger) SyncRegistry(args *RiggerRegistryNameRequest, reply *RiggerRegistryResponse) error {
	authErr := r.srv.Authenticate(r.ctx, args)
	if done, err := r.srv.forward("Rigger.SyncRegistry", args, args, reply); done {
		return err
	}

	if authErr != nil {
		return structs.ErrPermissionDenied
	}

	if err := r.authorize(args, acl.ObjectCapabilityWrite); err != nil {
		return err
	}

	reg, idx, err := r.srv.syncRiggerRegistry(args.Name)
	if err != nil {
		return err
	}

	reply.Registry, reply.Index = reg, idx

	return nil
}

// SearchRegistry returns a page of the modules and stacks of a registry
// that match the query, at the commit of its last good sync; a registry
// never synced offers nothing. It needs read on the kind of registries. The
// display of the registry does not matter: what is selected for the UI is
// in its status, a search finds everything.
func (r *Rigger) SearchRegistry(args *RiggerRegistrySearchRequest, reply *RiggerRegistrySearchResponse) error {
	authErr := r.srv.Authenticate(r.ctx, args)
	if done, err := r.srv.forward("Rigger.SearchRegistry", args, args, reply); done {
		return err
	}

	if authErr != nil {
		return structs.ErrPermissionDenied
	}

	if err := r.authorize(args, acl.ObjectCapabilityRead); err != nil {
		return err
	}

	q := catalog.Query{Kind: index.Kind(args.Kind), Text: args.Text, Offset: args.Offset, Limit: args.Limit}

	items, total, reg, err := r.srv.searchRiggerRegistry(args.Name, q)
	if err != nil {
		return err
	}

	reply.Items, reply.Total, reply.Commit = items, total, reg.Status.Commit
	reply.Index = max(1, r.srv.raft.AppliedIndex())

	return nil
}

// authorize checks an object capability on the kind of registries, which
// is cluster scoped.
func (r *Rigger) authorize(args structs.RequestWithIdentity, capability string) error {
	aclObj, err := r.srv.ResolveACL(args)
	if err != nil {
		return err
	}

	if aclObj != nil && !aclObj.AllowObjectOperation(acl.ObjectClusterNamespace, structs.RiggerRegistryKindRef, capability) {
		return structs.ErrPermissionDenied
	}

	return nil
}
