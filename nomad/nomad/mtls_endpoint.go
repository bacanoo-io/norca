// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"fmt"
	"net/http"
	"slices"
	"time"

	metrics "github.com/armon/go-metrics"
	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
)

// MTLS endpoint serves the native mTLS trust bundle and signs workload
// certificates.
type MTLS struct {
	srv *Server
	ctx *RPCContext
}

func NewMTLSEndpoint(srv *Server, ctx *RPCContext) *MTLS {
	return &MTLS{srv: srv, ctx: ctx}
}

// Roots returns the trust bundle of the region: the public certificates of
// its CAs and of the CAs of the federated regions. The certificates are
// public, so the endpoint needs no ACL capability; any caller that can
// authenticate (including the anonymous token) may read them, which lets
// the leaders of other regions fetch them without a token.
func (m *MTLS) Roots(args *structs.MTLSRootsRequest, reply *structs.MTLSRootsResponse) error {
	authErr := m.srv.Authenticate(m.ctx, args)
	if done, err := m.srv.forward("MTLS.Roots", args, args, reply); done {
		return err
	}
	m.srv.MeasureRPCRate("mtls", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "mtls", "roots"}, time.Now())

	region := m.srv.Region()
	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, store *state.StateStore) error {
			roots, err := store.MTLSRoots(ws)
			if err != nil {
				return err
			}
			reply.Roots = make([]*structs.MTLSRoot, 0, len(roots))
			reply.TrustDomain, reply.ActiveRootID = "", ""
			for _, root := range roots {
				if root.Region == region && root.Active {
					reply.TrustDomain = root.TrustDomain
					reply.ActiveRootID = root.ID
				}
				reply.Roots = append(reply.Roots, root.Public())
			}
			// Own region first, then by region and age.
			slices.SortStableFunc(reply.Roots, func(a, b *structs.MTLSRoot) int {
				switch {
				case a.Region == region && b.Region != region:
					return -1
				case a.Region != region && b.Region == region:
					return 1
				case a.Region != b.Region:
					if a.Region < b.Region {
						return -1
					}
					return 1
				}
				return a.NotBefore.Compare(b.NotBefore)
			})

			index, err := store.Index(state.TableMTLSRoots)
			if err != nil {
				return err
			}
			reply.Index = max(1, index)
			m.srv.setQueryMeta(&reply.QueryMeta)
			return nil
		}}
	return m.srv.blockingRPC(&opts)
}

// SignCertificate signs a workload certificate for a task. The request
// carries the workload identity token of the task, which proves the identity
// independently of ACLs; when ACLs are enabled, the caller must also be the
// client node running the allocation, or the task itself. The SPIFFE ID and
// the DNS names come from the allocation; only the public key is taken from
// the CSR, so private keys never travel over RPC.
func (m *MTLS) SignCertificate(args *structs.MTLSSignRequest, reply *structs.MTLSSignResponse) error {
	authErr := m.srv.Authenticate(m.ctx, args)
	if done, err := m.srv.forward("MTLS.SignCertificate", args, args, reply); done {
		return err
	}
	m.srv.MeasureRPCRate("mtls", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "mtls", "sign_certificate"}, time.Now())

	if !ServersMeetMinimumNorcaVersion(m.srv.serf.Members(), m.srv.Region(), false) {
		return fmt.Errorf("all servers must be running Norca %v or later to issue mTLS certificates", minNorcaFeaturesVersion)
	}
	if !m.srv.mtlsConfig().IsEnabled() {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "native mTLS is disabled in the server configuration")
	}
	if args.IdentityToken == "" || args.CSR == "" {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "a workload identity token and a CSR are required")
	}

	claims, err := m.srv.VerifyClaim(args.IdentityToken)
	if err != nil || claims.AllocationID == "" {
		return structs.ErrPermissionDenied
	}
	if claims.AllocationID != args.AllocID || claims.TaskName != args.TaskName {
		return structs.ErrPermissionDenied
	}

	alloc, err := m.srv.State().AllocByID(nil, claims.AllocationID)
	if err != nil {
		return err
	}
	if alloc == nil || alloc.Job == nil {
		return structs.NewErrRPCCodedf(http.StatusNotFound, "allocation %s not found", claims.AllocationID)
	}
	if alloc.Namespace != claims.Namespace || alloc.JobID != claims.JobID {
		return structs.ErrPermissionDenied
	}
	if alloc.TerminalStatus() {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "allocation %s is terminal", alloc.ID)
	}

	if m.srv.config.ACLEnabled {
		identity := args.GetIdentity()
		switch {
		case identity == nil:
			return structs.ErrPermissionDenied
		case identity.ClientID != "" && identity.ClientID == alloc.NodeID:
		case identity.Claims != nil && identity.Claims.AllocationID == alloc.ID:
		default:
			return structs.ErrPermissionDenied
		}
	}

	tg := alloc.Job.LookupTaskGroup(alloc.TaskGroup)
	if tg == nil || tg.LookupTask(args.TaskName) == nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "task %q is not part of the allocation", args.TaskName)
	}
	services := tg.MTLSServices()
	if len(services) == 0 {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "task group %q has no service with an mtls block", tg.Name)
	}

	csr, err := parseWorkloadCSR(args.CSR)
	if err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
	}

	snap, err := m.srv.State().Snapshot()
	if err != nil {
		return err
	}
	roots, err := snap.MTLSRoots(nil)
	if err != nil {
		return err
	}
	active, _, _ := mtlsSplitRoots(m.srv.Region(), roots)
	if active == nil {
		return structs.NewErrRPCCodedf(http.StatusServiceUnavailable, "the certificate authority of the region is not initialized yet")
	}

	jobID := mtlsWorkloadJobID(alloc.Job)
	for _, check := range []struct{ kind, value string }{
		{"namespace", alloc.Namespace}, {"job", jobID}, {"group", tg.Name},
	} {
		if err := structs.ValidateSPIFFESegment(check.kind, check.value); err != nil {
			return structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
		}
	}
	spiffeID := structs.MTLSWorkloadSPIFFEID(active.TrustDomain, alloc.Namespace, jobID, tg.Name)
	dnsNames := mtlsDNSNames(services, alloc.Namespace, m.srv.Region())

	signer, err := m.srv.mtls.signer(active)
	if err != nil {
		m.srv.mtls.logger.Error("failed to load the key of the certificate authority", "root_id", active.ID, "error", err)
		return structs.NewErrRPCCodedf(http.StatusServiceUnavailable, "the certificate authority is not available: %v", err)
	}
	certPEM, cert, err := signWorkloadCert(signer, csr, spiffeID, dnsNames, m.srv.mtlsConfig().LeafCertTTL)
	if err != nil {
		return err
	}
	metrics.IncrCounter([]string{"nomad", "mtls", "certificates_issued"}, 1)

	reply.CertPEM = certPEM
	reply.SPIFFEID = spiffeID
	reply.RootID = active.ID
	reply.NotBefore = cert.NotBefore
	reply.NotAfter = cert.NotAfter
	reply.Index = active.ModifyIndex
	return nil
}

// mtlsWorkloadJobID returns the job ID of the SPIFFE ID of a job's
// workloads: dispatched and periodic child jobs share the identity of their
// parent job.
func mtlsWorkloadJobID(job *structs.Job) string {
	if job.ParentID != "" {
		return job.ParentID
	}
	return job.ID
}

// mtlsDNSNames returns the DNS names of the mtls services of a task group.
// Services whose name is interpolated at runtime are left out.
func mtlsDNSNames(services []*structs.Service, namespace, region string) []string {
	var names []string
	for _, service := range services {
		if !structs.IsValidMTLSServiceName(service.Name) {
			continue
		}
		for _, name := range structs.MTLSServiceDNSNames(service.Name, namespace, region) {
			if !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	return names
}

// RotateCA replaces the active CA of the region. It requires a management
// token.
func (m *MTLS) RotateCA(args *structs.MTLSRotateRequest, reply *structs.MTLSRotateResponse) error {
	authErr := m.srv.Authenticate(m.ctx, args)
	if done, err := m.srv.forward("MTLS.RotateCA", args, args, reply); done {
		return err
	}
	m.srv.MeasureRPCRate("mtls", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "mtls", "rotate_ca"}, time.Now())

	// A management token is required; a client node's secret resolves to
	// no ACL object and is not enough.
	if aclObj, err := m.srv.ResolveACL(args); err != nil {
		return err
	} else if m.srv.config.ACLEnabled && (aclObj == nil || !aclObj.IsManagement()) {
		return structs.ErrPermissionDenied
	}
	if !m.srv.mtlsConfig().IsEnabled() {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "native mTLS is disabled in the server configuration")
	}

	root, index, err := m.srv.mtls.rotate()
	if err != nil {
		return err
	}
	reply.RootID = root.ID
	reply.Index = index
	return nil
}
