// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/hashicorp/go-memdb"
	metrics "github.com/hashicorp/go-metrics"
)

// Certificate is the RPC endpoint of the built-in certificate issuer: the
// trust bundle and the revocation list of the issuing CA of the region, the
// revocation of a certificate, and the import and rotation of the CA.
// Certificates and issuers themselves are objects of the object API.
type Certificate struct {
	srv *Server
	ctx *RPCContext
}

func NewCertificateEndpoint(srv *Server, ctx *RPCContext) *Certificate {
	return &Certificate{srv: srv, ctx: ctx}
}

// Roots returns the trust bundle of the issuing CA of the region. It holds
// public certificates only.
func (c *Certificate) Roots(args *structs.CertificateRootsRequest, reply *structs.CertificateRootsResponse) error {
	authErr := c.srv.Authenticate(c.ctx, args)
	if done, err := c.srv.forward("Certificate.Roots", args, args, reply); done {
		return err
	}
	c.srv.MeasureRPCRate("certificate", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "certificate", "roots"}, time.Now())

	region := c.srv.Region()
	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, store *state.StateStore) error {
			all, err := store.CertificateAuthorities(ws)
			if err != nil {
				return err
			}
			active, retired := splitCertificateAuthorities(region, all)
			slices.SortFunc(retired, func(a, b *structs.CertificateAuthority) int { return b.NotBefore.Compare(a.NotBefore) })
			reply.ActiveID = ""
			reply.Authorities = make([]*structs.CertificateAuthority, 0, len(retired)+1)
			if active != nil {
				reply.ActiveID = active.ID
				reply.Authorities = append(reply.Authorities, active.Public())
			}
			for _, ca := range retired {
				reply.Authorities = append(reply.Authorities, ca.Public())
			}
			reply.Roots = certificateTrustBundle(region, all)

			index, err := store.Index(state.TableCertificateAuthorities)
			if err != nil {
				return err
			}
			reply.Index = max(1, index)
			c.srv.setQueryMeta(&reply.QueryMeta)
			return nil
		}}
	return c.srv.blockingRPC(&opts)
}

// CRL returns the revocation list of an issuing CA of the region, signed
// now by the server that answers.
func (c *Certificate) CRL(args *structs.CertificateCRLRequest, reply *structs.CertificateCRLResponse) error {
	authErr := c.srv.Authenticate(c.ctx, args)
	if done, err := c.srv.forward("Certificate.CRL", args, args, reply); done {
		return err
	}
	c.srv.MeasureRPCRate("certificate", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "certificate", "crl"}, time.Now())

	store := c.srv.State()
	all, err := store.CertificateAuthorities(nil)
	if err != nil {
		return err
	}
	active, retired := splitCertificateAuthorities(c.srv.Region(), all)
	ca := active
	if args.Authority != "" {
		ca = nil
		for _, candidate := range append(retired, active) {
			if candidate != nil && candidate.ID == args.Authority {
				ca = candidate
			}
		}
	}
	if ca == nil {
		return structs.NewErrRPCCodedf(http.StatusNotFound, "the region has no such issuing certificate authority")
	}
	if reply.CRL, err = c.srv.certificateCA.revocationList(ca); err != nil {
		return err
	}
	index, err := store.Index(state.TableCertificateAuthorities)
	if err != nil {
		return err
	}
	reply.Index = max(1, index)
	c.srv.setQueryMeta(&reply.QueryMeta)
	return nil
}

// requireManagement checks that the caller holds a management token; a
// client node's secret resolves to no ACL object and is not enough.
func (c *Certificate) requireManagement(args structs.RequestWithIdentity) error {
	aclObj, err := c.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	if c.srv.config.ACLEnabled && (aclObj == nil || !aclObj.IsManagement()) {
		return structs.ErrPermissionDenied
	}
	return nil
}

// ImportAuthority puts the CA of an operator in the place of the issuing CA
// of the region. The certificates of the built-in issuer are issued again
// with it.
func (c *Certificate) ImportAuthority(args *structs.CertificateAuthorityImportRequest, reply *structs.CertificateAuthorityResponse) error {
	authErr := c.srv.Authenticate(c.ctx, args)
	if done, err := c.srv.forward("Certificate.ImportAuthority", args, args, reply); done {
		return err
	}
	c.srv.MeasureRPCRate("certificate", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "certificate", "import_authority"}, time.Now())

	if err := c.requireManagement(args); err != nil {
		return err
	}
	ca, index, err := c.srv.certificateCA.importCA(args.Cert, args.Key)
	if err != nil {
		return err
	}
	reply.Authority = ca.Public()
	reply.Index = index
	return nil
}

// RotateAuthority replaces the issuing CA of the region with a new one of
// the leader.
func (c *Certificate) RotateAuthority(args *structs.CertificateAuthorityRotateRequest, reply *structs.CertificateAuthorityResponse) error {
	authErr := c.srv.Authenticate(c.ctx, args)
	if done, err := c.srv.forward("Certificate.RotateAuthority", args, args, reply); done {
		return err
	}
	c.srv.MeasureRPCRate("certificate", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "certificate", "rotate_authority"}, time.Now())

	if err := c.requireManagement(args); err != nil {
		return err
	}
	ca, index, err := c.srv.certificateCA.rotate()
	if err != nil {
		return err
	}
	reply.Authority = ca.Public()
	reply.Index = index
	return nil
}

// certificateRevokeAttempts is how often a revocation is tried again when
// the certificate changes under it.
const certificateRevokeAttempts = 5

// Revoke revokes the current certificate of a Certificate object: the
// built-in issuer lists it in its revocation list, an external issuer gets
// a revocation order. The operator then issues a new certificate.
func (c *Certificate) Revoke(args *structs.CertificateRevokeRequest, reply *structs.CertificateRevokeResponse) error {
	authErr := c.srv.Authenticate(c.ctx, args)
	if done, err := c.srv.forward("Certificate.Revoke", args, args, reply); done {
		return err
	}
	c.srv.MeasureRPCRate("certificate", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "certificate", "revoke"}, time.Now())

	aclObj, err := c.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	namespace := args.RequestNamespace()
	kindRef := structs.ObjectKindRef(structs.OperatorsGroup, structs.CertificateKind)
	if !aclObj.AllowObjectOperation(namespace, kindRef, acl.ObjectCapabilityWrite) {
		return structs.ErrPermissionDenied
	}
	// The reason, by the name of its RFC 5280 CRLReason (review finding
	// ATM-25); unspecified is the same as none.
	reason := args.Reason
	if _, ok := structs.CertificateRevokeReasonCode(reason); !ok {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "unknown revocation reason %q: give one of %s",
			reason, strings.Join(structs.CertificateRevokeReasons, ", "))
	}
	if reason == structs.CertificateRevokeReasonUnspecified {
		reason = ""
	}

	for attempt := 1; ; attempt++ {
		snap, err := c.srv.State().Snapshot()
		if err != nil {
			return err
		}
		obj, err := snap.CustomObjectByName(nil, structs.OperatorsGroup, structs.CertificateKind, namespace, args.Name)
		if err != nil {
			return err
		}
		if obj == nil {
			return structs.NewErrRPCCodedf(http.StatusNotFound, "certificate %q not found", args.Name)
		}
		status := &structs.CertificateStatus{}
		if err := structs.DecodeObjectPart(obj.Status, status); err != nil {
			return err
		}
		if status.Serial == "" {
			return structs.NewErrRPCCodedf(http.StatusConflict, "certificate %q has not been issued yet", args.Name)
		}
		reply.Serial = status.Serial
		if status.Revoke == status.Serial {
			reply.Index = obj.ModifyIndex
			return nil
		}

		if err := c.srv.revokeWithIssuer(&snap.StateStore, obj, status, reason); err != nil &&
			!errors.Is(err, errCertificateNotRevocable) {
			return err
		}
		changed := obj.Copy()
		changed.Status["revoke"] = status.Serial
		written, err := c.srv.writeOperatorObject(changed)
		if err == nil {
			reply.Index = written.ModifyIndex
			message := fmt.Sprintf("certificate with the serial %s revoked by %s", status.Serial, objectReporter(args))
			if reason != "" {
				message += " for the reason " + reason
			}
			c.srv.operatorEvent(written, structs.ObjectEventTypeNormal, "Revoked", message)
			operatorCount(structs.CertificateKind, "revoked")
			return nil
		}
		if !structs.IsObjectConflictError(err) || attempt >= certificateRevokeAttempts {
			return err
		}
	}
}

// certificateRevokeOrderName returns the name of the revocation order of a
// serial of a certificate.
func certificateRevokeOrderName(namespace, name, serial string) string {
	return fmt.Sprintf("%s.%s.revoke-%.16s", namespace, name, serial)
}

// errCertificateNotRevocable is the error of a revocation that no issuer is
// left to make; it says why.
var errCertificateNotRevocable = errors.New("nobody can revoke the certificate")

// revokeWithIssuer tells the issuer of a certificate that its current
// certificate is revoked, for the reason (the name of an RFC 5280 CRLReason,
// "" for none). The certificate operator calls it too. It fails with
// errCertificateNotRevocable when no issuer is left to revoke the
// certificate: the CA that signed it is gone (only after every certificate
// it signed has expired), or the external issuer is gone or was recreated as
// a built-in one.
func (s *Server) revokeWithIssuer(store *state.StateStore, obj *structs.CustomObject, status *structs.CertificateStatus, reason string) error {
	notAfter, _ := time.Parse(time.RFC3339, status.NotAfter)

	// A certificate with an authority was signed by the built-in issuer,
	// whatever the issuer of that name is now.
	if status.Authority != "" {
		found, err := s.certificateCA.revoke(status.Authority, &structs.CertificateRevocation{
			Serial:      status.Serial,
			Certificate: obj.Namespace + "/" + obj.Name,
			RevokedAt:   time.Now().UTC().Truncate(time.Second),
			NotAfter:    notAfter,
			Reason:      reason,
		})
		if err == nil && !found {
			err = fmt.Errorf("%w: the certificate authority %s that signed it is gone", errCertificateNotRevocable, status.Authority)
		}
		return err
	}
	issuerObj, err := store.CustomObjectByName(nil, structs.OperatorsGroup, structs.CertificateIssuerKind, "", status.Issuer)
	if err != nil {
		return err
	}
	if issuerObj == nil {
		return fmt.Errorf("%w: the issuer %q is gone", errCertificateNotRevocable, status.Issuer)
	}
	issuer := &structs.CertificateIssuerSpec{}
	if err := structs.DecodeObjectPart(issuerObj.Spec, issuer); err != nil {
		return err
	}
	if issuer.External == nil {
		return fmt.Errorf("%w: the issuer %q is no longer the external issuer that signed it", errCertificateNotRevocable, status.Issuer)
	}

	// An external issuer revokes through an order of its own.
	name := certificateRevokeOrderName(obj.Namespace, obj.Name, status.Serial)
	existing, err := store.CustomObjectByName(nil, structs.OperatorsGroup, structs.CertificateOrderKind, "", name)
	if err != nil || existing != nil {
		return err
	}
	spec, err := structs.EncodeObjectPart(&structs.CertificateOrderSpec{
		Issuer:      status.Issuer,
		Certificate: obj.Namespace + "/" + obj.Name,
		Revoke:      status.Serial,
		Reason:      reason,
	})
	if err != nil {
		return err
	}
	_, err = s.writeOperatorObject(&structs.CustomObject{
		Group:     structs.OperatorsGroup,
		Kind:      structs.CertificateOrderKind,
		Name:      name,
		ManagedBy: structs.OperatorsManagedBy,
		Spec:      spec,
		Status:    map[string]any{"state": structs.CertificateOrderPending},
	})
	return err
}
