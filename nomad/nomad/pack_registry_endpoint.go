// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"fmt"
	"net/http"

	"github.com/hashicorp/nomad/nomad/structs"
)

// Packs is the RPC endpoint of the cluster pack registries and their
// catalog. Writes, pack details and server-side renders are forwarded to
// the leader: it keeps the pack cache that matches the registry status
// (syncGeneration), so every server serves the same pack content.
type Packs struct {
	srv *Server
	ctx *RPCContext
}

func NewPacksEndpoint(srv *Server, ctx *RPCContext) *Packs {
	return &Packs{srv: srv, ctx: ctx}
}

// PackRegistryListRequest lists the registries or the catalog of their
// packs (Registry filters the catalog).
type PackRegistryListRequest struct {
	Registry string
	structs.QueryOptions
}

type PackRegistryListResponse struct {
	Registries []*PackRegistry
	Packs      []*PackCatalogEntry
	structs.QueryMeta
}

// PackRegistryUpsertRequest creates or updates a registry, which is synced.
type PackRegistryUpsertRequest struct {
	Registry *PackRegistry
	structs.WriteRequest
}

// PackRegistryNameRequest names a registry to sync or delete.
type PackRegistryNameRequest struct {
	Name string
	structs.WriteRequest
}

type PackRegistryResponse struct {
	Registry *PackRegistry
	structs.WriteMeta
}

// PackDetailRequest reads a pack of a registry.
type PackDetailRequest struct {
	Registry string
	Pack     string
	Version  string
	structs.QueryOptions
}

type PackDetailRPCResponse struct {
	Detail *PackDetailResponse
	structs.QueryMeta
}

// PackDeployRPCRequest renders a pack of a registry on the leader and
// plans (DryRun) or applies it as a revision of the release Name.
type PackDeployRPCRequest struct {
	Name   string
	Deploy *PackDeployRequest
	DryRun bool
	structs.WriteRequest
}

func (p *Packs) genericRequest(region, namespace, token string) *structs.GenericRequest {
	return &structs.GenericRequest{QueryOptions: structs.QueryOptions{
		Region: region, Namespace: namespace, AuthToken: token,
	}}
}

// List returns the registries.
func (p *Packs) List(args *PackRegistryListRequest, reply *PackRegistryListResponse) error {
	authErr := p.srv.Authenticate(p.ctx, args)
	if done, err := p.srv.forward("Packs.List", args, args, reply); done {
		return err
	}
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	regs, index, err := p.srv.ListPackRegistries(p.genericRequest(args.Region, args.Namespace, args.AuthToken))
	if err != nil {
		return err
	}
	reply.Registries, reply.Index = regs, index
	p.srv.setQueryMeta(&reply.QueryMeta)
	return nil
}

// Catalog returns the packs of the registries.
func (p *Packs) Catalog(args *PackRegistryListRequest, reply *PackRegistryListResponse) error {
	authErr := p.srv.Authenticate(p.ctx, args)
	if done, err := p.srv.forward("Packs.Catalog", args, args, reply); done {
		return err
	}
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	packs, index, err := p.srv.ListCatalogPacks(p.genericRequest(args.Region, args.Namespace, args.AuthToken), args.Registry)
	if err != nil {
		return err
	}
	reply.Packs, reply.Index = packs, index
	p.srv.setQueryMeta(&reply.QueryMeta)
	return nil
}

// Upsert creates or updates a registry and syncs it.
func (p *Packs) Upsert(args *PackRegistryUpsertRequest, reply *PackRegistryResponse) error {
	authErr := p.srv.Authenticate(p.ctx, args)
	if done, err := p.srv.forward("Packs.Upsert", args, args, reply); done {
		return err
	}
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	if args.Registry == nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "missing registry")
	}
	reg, index, err := p.srv.UpsertPackRegistry(p.genericRequest(args.Region, args.Namespace, args.AuthToken), args.Registry)
	if err != nil {
		return err
	}
	reply.Registry, reply.Index = reg, index
	return nil
}

// Sync syncs a registry.
func (p *Packs) Sync(args *PackRegistryNameRequest, reply *PackRegistryResponse) error {
	authErr := p.srv.Authenticate(p.ctx, args)
	if done, err := p.srv.forward("Packs.Sync", args, args, reply); done {
		return err
	}
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	reg, index, err := p.srv.SyncPackRegistry(p.genericRequest(args.Region, args.Namespace, args.AuthToken), args.Name)
	if err != nil {
		return err
	}
	reply.Registry, reply.Index = reg, index
	return nil
}

// Delete removes a registry and its cache.
func (p *Packs) Delete(args *PackRegistryNameRequest, reply *PackRegistryResponse) error {
	authErr := p.srv.Authenticate(p.ctx, args)
	if done, err := p.srv.forward("Packs.Delete", args, args, reply); done {
		return err
	}
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	index, err := p.srv.DeletePackRegistry(p.genericRequest(args.Region, args.Namespace, args.AuthToken), args.Name)
	if err != nil {
		return err
	}
	reply.Index = index
	return nil
}

// Detail returns a pack of a registry with its versions, README and form
// schema. It is served by the leader, from its cache.
func (p *Packs) Detail(args *PackDetailRequest, reply *PackDetailRPCResponse) error {
	authErr := p.srv.Authenticate(p.ctx, args)
	// Not a stale read: only the leader keeps the cache in sync.
	args.AllowStale = false
	if done, err := p.srv.forward("Packs.Detail", args, args, reply); done {
		return err
	}
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	detail, index, err := p.srv.GetCatalogPack(p.genericRequest(args.Region, args.Namespace, args.AuthToken), args.Registry, args.Pack, args.Version)
	if err != nil {
		return err
	}
	reply.Detail, reply.Index = detail, index
	p.srv.setQueryMeta(&reply.QueryMeta)
	return nil
}

// Deploy renders a pack of a registry on the leader and applies (or plans)
// it as a server-rendered release revision.
func (p *Packs) Deploy(args *PackDeployRPCRequest, reply *structs.ReleaseApplyResponse) error {
	authErr := p.srv.Authenticate(p.ctx, args)
	if done, err := p.srv.forward("Packs.Deploy", args, args, reply); done {
		return err
	}
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	convert := p.srv.config.ReleaseFromRendered
	if convert == nil {
		return fmt.Errorf("server-side pack rendering is not available on this server")
	}
	namespace := args.RequestNamespace()
	rendered, err := p.srv.RenderCatalogPack(p.genericRequest(args.Region, namespace, args.AuthToken), args.Name, args.Deploy)
	if err != nil {
		return err
	}
	req, err := convert(args.Name, rendered, namespace)
	if err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
	}
	req.DryRun = args.DryRun
	req.WriteRequest = args.WriteRequest
	return NewReleaseEndpoint(p.srv, p.ctx).applyInternal(req, reply, true)
}
