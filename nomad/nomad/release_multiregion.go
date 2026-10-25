// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/nomad/nomad/structs"
)

// A multi-region install applies the same pack as a release of the same name
// in several regions. The region that receives the request (the coordinating
// region) sends it to the leader of every region in turn over the region
// forwarding of the RPC layer, with a description of the install
// (structs.ReleaseMultiregion), and reports the result per region. Every
// region applies the request as an ordinary release operation of its own:
// its release, revisions, atomic mode, hooks and drift detection are those
// of the region. The description divides the work:
//
//   - Global resources (namespaces, quotas, ACL policies) are written in the
//     authoritative region and replicated by the servers, so only the release
//     of the global region (the authoritative region, if it is one of the
//     regions) applies and owns them. The other regions wait for their
//     replication before they apply their own resources.
//   - A job with a multiregion block is registered by the release of the
//     first region of the install that the job lists; Norca's multiregion
//     deployment registers it in the other regions of the job and coordinates
//     the rollout. The releases of those regions adopt their copy.
//   - Failures are per region: the releases of the other regions are kept,
//     and atomic mode restores only the region that failed.

// releaseReplicationWait bounds how long a region of a multi-region install
// waits for the global resources of the pack to be replicated to it.
var releaseReplicationWait = time.Minute

// applyMultiregion coordinates a multi-region install or plan.
func (r *Release) applyMultiregion(args *structs.ReleaseApplyRequest, reply *structs.ReleaseApplyResponse) error {
	if args.Multiregion != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "regions and multiregion are exclusive")
	}
	if args.RecordOnly {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "a multi-region install can't be record_only")
	}
	if err := structs.ValidateReleaseName(args.Name); err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
	}
	if err := validateTimeout(args.Timeout); err != nil {
		return err
	}
	if err := validateReleaseVariables(args.VariablesSchemas, args.VariableValues); err != nil {
		return err
	}
	regions, err := r.srv.releases.multiregionOrder(args.Regions)
	if err != nil {
		return err
	}

	// The requests to the other regions carry the token of the caller, so it
	// must be valid in all of them.
	token, err := r.srv.ResolveSecretToken(args.AuthToken)
	if err != nil {
		return err
	}
	remote := slices.ContainsFunc(regions, func(region string) bool { return region != r.srv.Region() })
	if remote && token != nil && token != structs.AnonymousACLToken && !token.Global {
		return structs.NewErrRPCCodedf(http.StatusBadRequest,
			"a multi-region install must be requested with a global ACL token, which is valid in all regions")
	}

	mr := &structs.ReleaseMultiregion{Regions: regions, GlobalRegion: regions[0]}
	var failed []string
	for _, region := range regions {
		req := multiregionApplyRequest(args, mr, region)
		var resp structs.ReleaseApplyResponse
		result := &structs.ReleaseRegionResult{Region: region}
		if err := r.srv.RPC("Release.Apply", req, &resp); err != nil {
			result.Error = err.Error()
			result.Release = r.regionRelease(region, args)
			failed = append(failed, fmt.Sprintf("region %q: %v", region, err))
		} else {
			result.Release, result.Revision = resp.Release, resp.Revision
			result.Changes, result.Warnings = resp.Changes, resp.Warnings
		}
		reply.Regions = append(reply.Regions, result)
		for _, w := range result.Warnings {
			reply.Warnings = append(reply.Warnings, fmt.Sprintf("region %q: %s", region, w))
		}
	}
	if len(failed) == len(regions) {
		return fmt.Errorf("release %q failed in every region: %s", args.Name, strings.Join(failed, "; "))
	}

	first := reply.Regions[0]
	reply.Release, reply.Revision, reply.Changes = first.Release, first.Revision, first.Changes
	reply.Index = max(1, r.srv.raft.AppliedIndex())
	return nil
}

// multiregionOrder validates the regions of a multi-region install and
// returns them in application order: the global region first, then the
// others in the requested order.
func (m *releaseManager) multiregionOrder(requested []string) ([]string, error) {
	var regions []string
	for _, region := range requested {
		region = strings.TrimSpace(region)
		if region == "" {
			return nil, structs.NewErrRPCCodedf(http.StatusBadRequest, "empty region name")
		}
		if !slices.Contains(regions, region) {
			regions = append(regions, region)
		}
	}
	if len(regions) == 0 || len(regions) > structs.MaxReleaseRegions {
		return nil, structs.NewErrRPCCodedf(http.StatusBadRequest,
			"a multi-region install needs between 1 and %d regions", structs.MaxReleaseRegions)
	}

	known := m.srv.Regions()
	members := m.srv.Members()
	for _, region := range regions {
		if !slices.Contains(known, region) {
			return nil, structs.NewErrRPCCodedf(http.StatusBadRequest, "region %q is not a known region", region)
		}
		if !ServersMeetMinimumNorcaVersion(members, region, false) {
			return nil, fmt.Errorf("all servers in region %q must run Norca %s or later for multi-region installs",
				region, minNorcaFeaturesVersion)
		}
	}

	global := regions[0]
	if auth := m.srv.config.AuthoritativeRegion; slices.Contains(regions, auth) {
		global = auth
	}
	order := []string{global}
	for _, region := range regions {
		if region != global {
			order = append(order, region)
		}
	}
	return order, nil
}

// multiregionApplyRequest returns the request a region of a multi-region
// install applies. The resources are copied, as a request to the local
// region is handled in memory.
func multiregionApplyRequest(args *structs.ReleaseApplyRequest, mr *structs.ReleaseMultiregion, region string) *structs.ReleaseApplyRequest {
	req := *args
	req.Regions = nil
	req.Multiregion = mr.Copy()
	req.Resources = make([]*structs.ReleaseResource, len(args.Resources))
	for i, res := range args.Resources {
		req.Resources[i] = res.Copy()
	}
	req.Secrets = make([]*structs.ReleaseVariableSecret, 0, len(args.Secrets))
	for _, s := range args.Secrets {
		if s != nil {
			c := *s
			req.Secrets = append(req.Secrets, &c)
		}
	}
	if args.Pack != nil {
		p := *args.Pack
		req.Pack = &p
	}
	req.WriteRequest = structs.WriteRequest{
		Region:    region,
		Namespace: args.RequestNamespace(),
		AuthToken: args.AuthToken,
	}
	return &req
}

// regionRelease reads the release of a region after a failed operation, so
// that the result shows its state. It is best effort.
func (r *Release) regionRelease(region string, args *structs.ReleaseApplyRequest) *structs.Release {
	req := &structs.ReleaseSpecificRequest{
		Name: args.Name,
		QueryOptions: structs.QueryOptions{
			Region:    region,
			Namespace: args.RequestNamespace(),
			AuthToken: args.AuthToken,
		},
	}
	var resp structs.SingleReleaseResponse
	if err := r.srv.RPC("Release.GetRelease", req, &resp); err != nil {
		return nil
	}
	return resp.Release
}

// multiregionSplit divides the prepared resources of a region of a
// multi-region install: the resources the region applies (including the
// multiregion jobs it adopts), the global resources that the global region
// applies, and the changes that report the resources applied elsewhere.
// adopted maps the keys of adopted jobs to their submitting region.
func multiregionSplit(mr *structs.ReleaseMultiregion, region string, resources []*structs.ReleaseResource) (
	local, global []*structs.ReleaseResource, adopted map[string]string, remote []*structs.ReleaseResourceChange) {

	adopted = map[string]string{}
	for _, res := range resources {
		ref := res.Ref()
		switch {
		case structs.IsGlobalReleaseResource(res.Type) && region != mr.GlobalRegion:
			global = append(global, res)
			remote = append(remote, &structs.ReleaseResourceChange{
				Type: ref.Type, Namespace: ref.Namespace, Name: ref.Name,
				Action: structs.ReleaseChangeRemote, Region: mr.GlobalRegion,
			})
			continue
		case res.Type == structs.ReleaseResourceJob && res.Job.IsMultiregion():
			jobRegions := res.Job.Multiregion.RegionNames()
			submitter := mr.JobSubmitter(jobRegions)
			switch {
			case submitter == region:
			case slices.Contains(jobRegions, region) && !res.IsHook():
				adopted[ref.Key()] = submitter
			default:
				remote = append(remote, &structs.ReleaseResourceChange{
					Type: ref.Type, Namespace: ref.Namespace, Name: ref.Name, Hook: ref.Hook,
					Action: structs.ReleaseChangeRemote, Region: submitter,
				})
				continue
			}
		}
		local = append(local, res)
	}
	return local, global, adopted, remote
}

// waitReplicated waits until the global resources applied by the global
// region of a multi-region install have been replicated to this region with
// their content, so that the resources of this region can use them. It
// gives up with a warning after releaseReplicationWait.
func (op *releaseOp) waitReplicated(global []*structs.ReleaseResource, from string) {
	if len(global) == 0 {
		return
	}
	if !op.m.srv.config.ACLEnabled {
		// The servers replicate namespaces, quotas and ACL policies only in
		// clusters with ACLs.
		op.warn("the global resources of the pack are applied in region %q, but the servers replicate them only with ACLs enabled", from)
		return
	}
	pending := global
	deadline := time.NewTimer(releaseReplicationWait)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		snap, err := op.m.srv.State().Snapshot()
		if err != nil {
			op.warn("failed to check the replication of global resources: %v", err)
			return
		}
		var still []*structs.ReleaseResource
		for _, res := range pending {
			live, err := op.m.readLive(snap, res.Ref())
			if err != nil || !live.exists() || resourceChanged(res, live) {
				still = append(still, res)
			}
		}
		if pending = still; len(pending) == 0 {
			return
		}
		select {
		case <-tick.C:
		case <-op.ctx.Done():
			return
		case <-deadline.C:
			for _, res := range pending {
				op.warn("%s was not replicated from region %q within %s", res.Ref(), from, releaseReplicationWait)
			}
			return
		}
	}
}
