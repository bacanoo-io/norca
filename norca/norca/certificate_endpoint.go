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
//
// With a serial number it revokes the certificate of the object with that
// serial (review finding ATM-24b): the current one as without it, an earlier
// one (revokeEarlier) without a new certificate.
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
	serial := ""
	if args.Serial != "" {
		var ok bool
		if serial, ok = structs.NormalizeCertificateSerial(args.Serial); !ok {
			return structs.NewErrRPCCodedf(http.StatusBadRequest,
				"invalid serial number %q: give it in hexadecimal, as the status of the certificate shows it", args.Serial)
		}
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
		if serial != "" && serial != status.Serial {
			index, err := c.revokeEarlier(snap, obj, status, serial, reason, args)
			if structs.IsObjectConflictError(err) && attempt < certificateRevokeAttempts {
				continue
			}
			if err != nil {
				return err
			}
			reply.Serial, reply.Earlier, reply.Index = serial, true, index
			return nil
		}
		if status.Serial == "" {
			return structs.NewErrRPCCodedf(http.StatusConflict, "certificate %q has not been issued yet", args.Name)
		}
		reply.Serial = status.Serial
		if status.Revoke == status.Serial {
			// Revoked already: nothing changes, except that the reason may
			// still be raised to keyCompromise (review finding ATM-25b).
			reply.Index = obj.ModifyIndex
			if reason != structs.CertificateRevokeReasonKeyCompromise {
				return nil
			}
			raised, err := c.srv.revokeWithIssuer(&snap.StateStore, obj, status, reason)
			if structs.IsObjectConflictError(err) && attempt < certificateRevokeAttempts {
				continue
			}
			if err != nil && !errors.Is(err, errCertificateNotRevocable) {
				return err
			}
			if raised {
				c.srv.operatorEvent(obj, structs.ObjectEventTypeNormal, "RevocationReason", fmt.Sprintf(
					"%s given by %s as the reason of the revocation of the certificate with the serial %s, revoked before",
					reason, objectReporter(args), status.Serial))
			}
			return nil
		}

		if _, err := c.srv.revokeWithIssuer(&snap.StateStore, obj, status, reason); err != nil &&
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

// revokeEarlier revokes a certificate of a Certificate object that is not
// its current one, by its serial number (review finding ATM-24b), and
// returns the index of the object after it. Nothing is issued for it: the
// current certificate and the secret stay as they are.
//
// What says that the serial is a certificate of this object is written by
// the servers only: the earlier certificates in the status of the object
// (the object API refuses a status of a certificate,
// checkCertificateStatusWrite), and the revocation lists of the issuing CAs
// of the region, whose entries name their object. The secret, its labels
// and its versions are the user's to write, and a certificate is public:
// neither says anything here. A serial that is in neither place is refused
// in the same words whether it is nobody's or a certificate of another
// object.
//
// An earlier certificate in the status is revoked with the issuer that
// signed it and then marked revoked, where it stays until it expires; one
// that is revoked already, there or in a revocation list, changes only when
// the reason is keyCompromise (review finding ATM-25b), as the current one
// does. An object changed meanwhile fails with a conflict; the caller tries
// again.
func (c *Certificate) revokeEarlier(snap *state.StateSnapshot, obj *structs.CustomObject, status *structs.CertificateStatus,
	serial, reason string, args *structs.CertificateRevokeRequest) (uint64, error) {

	raise := reason == structs.CertificateRevokeReasonKeyCompromise
	raised := func() {
		c.srv.operatorEvent(obj, structs.ObjectEventTypeNormal, "RevocationReason", fmt.Sprintf(
			"%s given by %s as the reason of the revocation of the earlier certificate with the serial %s, revoked before",
			reason, objectReporter(args), serial))
	}

	earlier := certificateEarlierValid(status.Earlier, time.Now().UTC())
	i := slices.IndexFunc(earlier, func(e *structs.CertificateEarlier) bool { return e.Serial == serial })
	if i >= 0 {
		entry := earlier[i]
		if entry.Revoked && !raise {
			return obj.ModifyIndex, nil
		}
		told, err := c.srv.revokeWithIssuer(&snap.StateStore, obj, &structs.CertificateStatus{
			Issuer:    entry.Issuer,
			Authority: entry.Authority,
			Serial:    entry.Serial,
			NotAfter:  entry.NotAfter,
		}, reason)
		if errors.Is(err, errCertificateNotRevocable) {
			// Nothing is issued in its place, so there is nothing else to
			// do for it: the caller learns that it stays valid.
			return 0, structs.NewErrRPCCodedf(http.StatusConflict,
				"the certificate with the serial %s is not revoked, %v; it stays valid until %s", serial, err, entry.NotAfter)
		}
		if err != nil {
			return 0, err
		}
		if entry.Revoked {
			if told {
				raised()
			}
			return obj.ModifyIndex, nil
		}

		marked := make([]*structs.CertificateEarlier, len(earlier))
		for j, e := range earlier {
			cp := *e
			cp.Revoked = cp.Revoked || j == i
			marked[j] = &cp
		}
		part, err := structs.EncodeObjectPart(&structs.CertificateStatus{Earlier: marked})
		if err != nil {
			return 0, err
		}
		changed := obj.Copy()
		changed.Status["earlier"] = part["earlier"]
		written, err := c.srv.writeOperatorObject(changed)
		if err != nil {
			return 0, err
		}
		message := fmt.Sprintf("earlier certificate with the serial %s revoked by %s", serial, objectReporter(args))
		if reason != "" {
			message += " for the reason " + reason
		}
		c.srv.operatorEvent(written, structs.ObjectEventTypeNormal, "Revoked", message+"; the current certificate stays")
		operatorCount(structs.CertificateKind, "revoked")
		return written.ModifyIndex, nil
	}

	// A revoked certificate of the object that the status does not list:
	// the revocation list of the CA that signed it names the object.
	all, err := snap.CertificateAuthorities(nil)
	if err != nil {
		return 0, err
	}
	for _, ca := range all {
		if ca.Region != c.srv.Region() {
			continue
		}
		for _, rev := range ca.Revoked {
			if rev.Serial != serial || rev.Certificate != obj.Namespace+"/"+obj.Name {
				continue
			}
			if !raise {
				return obj.ModifyIndex, nil
			}
			_, changed, err := c.srv.certificateCA.revoke(ca.ID, &structs.CertificateRevocation{
				Serial:      rev.Serial,
				Certificate: rev.Certificate,
				RevokedAt:   rev.RevokedAt,
				NotAfter:    rev.NotAfter,
				Reason:      reason,
			})
			if err != nil {
				return 0, err
			}
			if changed {
				raised()
			}
			return obj.ModifyIndex, nil
		}
	}
	return 0, structs.NewErrRPCCodedf(http.StatusNotFound,
		"certificate %q has no certificate with the serial %s that can be revoked: it is neither its current certificate nor an earlier one that is still valid",
		args.Name, serial)
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
// "" for none). The certificate operator calls it too. For an earlier
// certificate of the object (revokeEarlier) status holds the serial, the
// issuer, the authority and the end of that one, and nothing else. It fails
// with errCertificateNotRevocable when no issuer is left to revoke the
// certificate: the CA that signed it is gone (only after every certificate
// it signed has expired), or the external issuer is gone or was recreated as
// a built-in one.
//
// For a certificate the issuer was told of before, nothing changes, except
// that the reason keyCompromise takes the place of another one (review
// finding ATM-25b): in the entry of the built-in issuer, whose time of
// revocation stays (certificateCA.revoke), and in the revocation order of an
// external issuer that its controller has not finished. With no order left,
// a new one with the reason is placed; whether the certificate authority
// behind the controller takes a reason for a certificate it has revoked is
// its own matter. It reports whether the issuer was told anything new.
func (s *Server) revokeWithIssuer(store *state.StateStore, obj *structs.CustomObject, status *structs.CertificateStatus, reason string) (bool, error) {
	notAfter, _ := time.Parse(time.RFC3339, status.NotAfter)

	// A certificate with an authority was signed by the built-in issuer,
	// whatever the issuer of that name is now.
	if status.Authority != "" {
		found, changed, err := s.certificateCA.revoke(status.Authority, &structs.CertificateRevocation{
			Serial:      status.Serial,
			Certificate: obj.Namespace + "/" + obj.Name,
			RevokedAt:   time.Now().UTC().Truncate(time.Second),
			NotAfter:    notAfter,
			Reason:      reason,
		})
		if err == nil && !found {
			err = fmt.Errorf("%w: the certificate authority %s that signed it is gone", errCertificateNotRevocable, status.Authority)
		}
		return changed, err
	}
	issuerObj, err := store.CustomObjectByName(nil, structs.OperatorsGroup, structs.CertificateIssuerKind, "", status.Issuer)
	if err != nil {
		return false, err
	}
	if issuerObj == nil {
		return false, fmt.Errorf("%w: the issuer %q is gone", errCertificateNotRevocable, status.Issuer)
	}
	issuer := &structs.CertificateIssuerSpec{}
	if err := structs.DecodeObjectPart(issuerObj.Spec, issuer); err != nil {
		return false, err
	}
	if issuer.External == nil {
		return false, fmt.Errorf("%w: the issuer %q is no longer the external issuer that signed it", errCertificateNotRevocable, status.Issuer)
	}

	// An external issuer revokes through an order of its own.
	name := certificateRevokeOrderName(obj.Namespace, obj.Name, status.Serial)
	existing, err := store.CustomObjectByName(nil, structs.OperatorsGroup, structs.CertificateOrderKind, "", name)
	if err != nil {
		return false, err
	}
	if existing != nil {
		return s.raiseRevokeOrderReason(existing, reason)
	}
	spec, err := structs.EncodeObjectPart(&structs.CertificateOrderSpec{
		Issuer:      status.Issuer,
		Certificate: obj.Namespace + "/" + obj.Name,
		Revoke:      status.Serial,
		Reason:      reason,
	})
	if err != nil {
		return false, err
	}
	_, err = s.writeOperatorObject(&structs.CustomObject{
		Group:     structs.OperatorsGroup,
		Kind:      structs.CertificateOrderKind,
		Name:      name,
		ManagedBy: structs.OperatorsManagedBy,
		Spec:      spec,
		Status:    map[string]any{"state": structs.CertificateOrderPending},
	})
	return err == nil, err
}

// raiseRevokeOrderReason gives a revocation order that is not done yet the
// reason keyCompromise in the place of another one; any other reason leaves
// the order as it is. It reports whether the order was changed. An order the
// controller wrote meanwhile fails with a conflict.
func (s *Server) raiseRevokeOrderReason(order *structs.CustomObject, reason string) (bool, error) {
	if reason != structs.CertificateRevokeReasonKeyCompromise {
		return false, nil
	}
	spec := &structs.CertificateOrderSpec{}
	status := &structs.CertificateOrderStatus{}
	if err := structs.DecodeObjectPart(order.Spec, spec); err != nil {
		return false, err
	}
	_ = structs.DecodeObjectPart(order.Status, status)
	if spec.Reason == reason || status.State == structs.CertificateOrderValid || status.State == structs.CertificateOrderFailed {
		return false, nil
	}
	changed := order.Copy()
	changed.Spec["reason"] = reason
	_, err := s.writeOperatorObject(changed)
	return err == nil, err
}
