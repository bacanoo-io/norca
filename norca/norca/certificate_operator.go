// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"crypto"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-memdb"
)

// The operator of certificates keeps every Certificate object issued and
// renewed.
//
// The leader makes the private key and never hands it out. With the
// built-in issuer it signs the certificate itself; with an external issuer
// it writes a CertificateOrder with a signing request and waits for the
// controller of the issuer to write the chain into the status of the order,
// which it checks before it trusts it. The certificate ends in the secret
// store, at the path of the spec: the items cert (the leaf and what goes
// with it), key, and with the built-in issuer ca (the trust bundle). While
// an order is open the key that waits for its certificate is the item
// next_key of the same secret, so the next leader finds it; the items cert
// and key keep the current certificate until the new one is complete.

var (
	// certificateIssuerStale is the age of the heartbeat of an external
	// issuer after which its controller counts as away. Tests lower it.
	certificateIssuerStale = 3 * time.Minute

	// certificateRetryBase and certificateRetryMax bound the wait after a
	// failed issuance: it doubles with every failure in a row.
	certificateRetryBase = time.Minute
	certificateRetryMax  = time.Hour

	// certificateRevokeOrderKeep is how long a revocation order may wait
	// for the controller of its issuer.
	certificateRevokeOrderKeep = 24 * time.Hour
)

// certificateReasonRevocationPending is the reason of the condition Failed
// of a certificate that is being deleted while its valid certificates are
// not all revoked yet (review finding ATM-24f).
const certificateReasonRevocationPending = "RevocationPending"

// certificateAuthorityMinRemaining is the life the issuing CA must have left
// for the built-in issuer to sign with it. A leaf ends with its CA at the
// latest, and is renewed at two thirds of its validity: with a CA near its
// end the renewals would follow each other ever faster, and with an expired
// one every run of the operator would issue again. The leader rotates its
// own CA long before; an imported CA is the operator's to replace.
const certificateAuthorityMinRemaining = time.Hour

// certificateAuthorityExpiring reports whether the CA has expired or ends
// too soon to sign with.
func certificateAuthorityExpiring(ca *structs.CertificateAuthority, now time.Time) bool {
	return !now.Add(certificateAuthorityMinRemaining).Before(ca.NotAfter)
}

// certificateOperator is the built-in operator of certificates.
type certificateOperator struct {
	srv    *Server
	logger hclog.Logger

	// writeFault, if set, is asked before the operator writes a Certificate
	// object, and fails the write with the error it returns. Only tests set
	// it: a write of the status that is lost.
	writeFault atomic.Pointer[func(*structs.CustomObject) error]
}

// writeObject writes a Certificate object as the operator changed it
// (Server.writeOperatorObject, which writes an object that is being deleted
// also when its kind refuses it: review findings ATM-24n and ATM-24o).
func (o *certificateOperator) writeObject(obj *structs.CustomObject) (*structs.CustomObject, error) {
	if fault := o.writeFault.Load(); fault != nil {
		if err := (*fault)(obj); err != nil {
			return nil, err
		}
	}
	return o.srv.writeOperatorObject(obj)
}

func newCertificateOperator(srv *Server, logger hclog.Logger) *certificateOperator {
	return &certificateOperator{srv: srv, logger: logger.Named("certificates")}
}

func (o *certificateOperator) kinds() []*structs.ObjectKind { return builtinCertificateKinds() }

// certificatePass is what one pass of the operator read.
type certificatePass struct {
	now     time.Time
	store   *state.StateStore
	ws      memdb.WatchSet
	issuers map[string]*structs.CustomObject
	orders  map[string]*structs.CustomObject
	active  *structs.CertificateAuthority
	all     []*structs.CertificateAuthority

	// wish is the earliest time a certificate wants the next pass.
	wish time.Time

	// after is what the pass tells of the certificate it is at only when the
	// status of that certificate is written (tell, settled): the events of
	// its revocation orders that are over.
	after []func()

	// unsettled names the certificates (namespace/name) whose status the
	// pass did not write as it found it: their revocation orders that are
	// over are kept for the next pass (cleanOrders; review finding ATM-24j).
	unsettled map[string]bool
}

// tell records something to tell of the certificate the pass is at once its
// status is written.
func (p *certificatePass) tell(event func()) { p.after = append(p.after, event) }

// settled ends the work of the pass on a certificate. ok says that the
// status is stored as the pass left it: what waited for that is told. If it
// is not, nothing is told, and the revocation orders of the certificate
// that are over stay (cleanOrders), so the next pass finds them as this one
// did, and tells then (review finding ATM-24j).
func (p *certificatePass) settled(obj *structs.CustomObject, ok bool) {
	after := p.after
	p.after = nil
	if !ok {
		if p.unsettled == nil {
			p.unsettled = map[string]bool{}
		}
		p.unsettled[obj.Namespace+"/"+obj.Name] = true
		return
	}
	for _, event := range after {
		event()
	}
}

func (p *certificatePass) want(t time.Time) {
	if !t.IsZero() && (p.wish.IsZero() || t.Before(p.wish)) {
		p.wish = t
	}
}

// listObjects returns the objects of a kind, watched.
func listObjects(store *state.StateStore, ws memdb.WatchSet, kind, namespace string) ([]*structs.CustomObject, error) {
	iter, err := store.CustomObjects(ws, structs.OperatorsGroup, kind, namespace, state.SortDefault)
	if err != nil {
		return nil, err
	}
	var out []*structs.CustomObject
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		out = append(out, raw.(*structs.CustomObject))
	}
	return out, nil
}

func (o *certificateOperator) reconcile(ws memdb.WatchSet) (time.Time, error) {
	store := o.srv.State()
	p := &certificatePass{
		now:     time.Now().UTC(),
		store:   store,
		ws:      ws,
		issuers: map[string]*structs.CustomObject{},
		orders:  map[string]*structs.CustomObject{},
	}

	// What a pass depends on besides the objects: the keyring (the keys of
	// the CA are encrypted with it) and the namespaces (what they may ask
	// for).
	if _, err := store.GetActiveRootKeyMeta(ws); err != nil {
		return time.Time{}, err
	}
	if _, err := store.Namespaces(ws); err != nil {
		return time.Time{}, err
	}

	issuers, err := listObjects(store, ws, structs.CertificateIssuerKind, "")
	if err != nil {
		return time.Time{}, err
	}
	wantCA := false
	for _, issuer := range issuers {
		p.issuers[issuer.Name] = issuer
		spec := &structs.CertificateIssuerSpec{}
		if err := structs.DecodeObjectPart(issuer.Spec, spec); err == nil && spec.Builtin != nil && !issuer.Deleting() {
			wantCA = true
		}
	}

	var caErr error
	if err := o.srv.certificateCA.reconcile(wantCA); err != nil {
		caErr = err
		if errors.Is(err, errCertificateCANotReady) {
			o.logger.Debug("issuing certificate authority not ready", "error", err)
		} else {
			o.logger.Warn("maintenance of the issuing certificate authority failed", "error", err)
		}
		p.want(p.now.Add(operatorsRetry))
	}
	if p.all, err = store.CertificateAuthorities(ws); err != nil {
		return time.Time{}, err
	}
	p.active, _ = splitCertificateAuthorities(o.srv.Region(), p.all)
	if p.active != nil && !p.active.Imported {
		// The rotation of the CA is due at two thirds of its life.
		p.want(p.active.NotBefore.Add(p.active.NotAfter.Sub(p.active.NotBefore)*2/3 + time.Second))
	}
	if p.active != nil && p.active.Imported {
		// The status of the issuers changes when an imported CA nears its
		// end.
		if t := p.active.NotAfter.Add(-certificateAuthorityMinRemaining + time.Second); t.After(p.now) {
			p.want(t)
		}
	}

	for _, issuer := range issuers {
		o.reconcileIssuer(p, issuer, caErr)
	}

	orders, err := listObjects(store, ws, structs.CertificateOrderKind, "")
	if err != nil {
		return time.Time{}, err
	}
	for _, order := range orders {
		p.orders[order.Name] = order
	}

	certs, err := listObjects(store, ws, structs.CertificateKind, structs.AllNamespacesSentinel)
	if err != nil {
		return time.Time{}, err
	}
	openOrders := map[string]bool{}
	for _, cert := range certs {
		if order := o.reconcileCertificate(p, cert); order != "" {
			openOrders[order] = true
		}
	}
	o.cleanOrders(p, orders, openOrders)
	return p.wish, nil
}

// issuerAvailable reports whether an issuer can sign now: the built-in one
// when the CA is there, an external one while its controller reports a
// heartbeat.
func (o *certificateOperator) issuerAvailable(p *certificatePass, issuer *structs.CustomObject) bool {
	spec := &structs.CertificateIssuerSpec{}
	if err := structs.DecodeObjectPart(issuer.Spec, spec); err != nil || issuer.Deleting() {
		return false
	}
	if spec.Builtin != nil {
		return p.active != nil
	}
	status := &structs.CertificateIssuerStatus{}
	if err := structs.DecodeObjectPart(issuer.Status, status); err != nil || status.Heartbeat == "" {
		return false
	}
	beat, err := time.Parse(time.RFC3339, status.Heartbeat)
	return err == nil && p.now.Sub(beat) < certificateIssuerStale
}

// reconcileIssuer writes the status of a built-in issuer: the CA that signs
// for it. The status of an external issuer is its controller's.
func (o *certificateOperator) reconcileIssuer(p *certificatePass, issuer *structs.CustomObject, caErr error) {
	spec := &structs.CertificateIssuerSpec{}
	if err := structs.DecodeObjectPart(issuer.Spec, spec); err != nil || spec.Builtin == nil || issuer.Deleting() {
		return
	}
	status := map[string]any{structs.ObjectStatusObservedGeneration: issuer.Generation}
	conditions := newConditionSet(issuer.Status, p.now)
	expiring := p.active != nil && certificateAuthorityExpiring(p.active, p.now)
	switch {
	case expiring:
		status["authority"] = p.active.ID
		status["notAfter"] = p.active.NotAfter.UTC().Format(time.RFC3339)
		conditions.set(structs.CertificateConditionReady, false, "AuthorityExpired", fmt.Sprintf(
			"the issuing certificate authority %s ends at %s; import or rotate to a new one",
			p.active.ID, p.active.NotAfter.UTC().Format(time.RFC3339)))
	case p.active != nil:
		status["authority"] = p.active.ID
		status["notAfter"] = p.active.NotAfter.UTC().Format(time.RFC3339)
		conditions.set(structs.CertificateConditionReady, true, "AuthorityReady", "")
	case caErr != nil:
		conditions.set(structs.CertificateConditionReady, false, "AuthorityNotReady", caErr.Error())
	default:
		conditions.set(structs.CertificateConditionReady, false, "AuthorityNotReady", "the issuing certificate authority is being created")
	}
	conditions.set(structs.CertificateConditionDegraded, expiring || (p.active == nil && caErr != nil), "", "")
	status[structs.ObjectStatusConditions] = conditions.list()
	o.writeStatus(issuer, status, nil)
}

// writeStatus writes a changed status (and, if finalizers is not nil, the
// finalizers) of an object. It returns the written object, or the given one
// if nothing changed or the write failed: the next pass tries again. ok says
// that the object is stored with the status: false for a write that failed.
func (o *certificateOperator) writeStatus(obj *structs.CustomObject, status map[string]any, finalizers *[]string) (written *structs.CustomObject, ok bool) {
	normalized, err := structs.NormalizeObjectMap(status)
	if err != nil {
		o.logger.Warn("status of an object can't be encoded", "object", obj.KindRef()+"/"+obj.Name, "error", err)
		return obj, false
	}
	if structs.ObjectMapsEqual(obj.Status, normalized) && (finalizers == nil || slices.Equal(obj.Finalizers, *finalizers)) {
		return obj, true
	}
	changed := obj.Copy()
	changed.Status = normalized
	if finalizers != nil {
		changed.Finalizers = slices.Clone(*finalizers)
	}
	written, err = o.writeObject(changed)
	if err != nil {
		if !structs.IsObjectConflictError(err) {
			o.logger.Warn("failed to write the status of an object", "object", obj.KindRef()+"/"+obj.Name,
				"namespace", obj.Namespace, "error", err)
		}
		return obj, false
	}
	return written, true
}

// certificateOutcome is what a pass found out about a certificate, for its
// conditions.
type certificateOutcome struct {
	// ready: there is a certificate in the secret that is valid now.
	ready bool

	// progress is "", Issuing (the first certificate) or Renewing.
	progress string

	// failedReason and failedMessage say why the certificate can't be
	// issued, or why the last attempt failed.
	failedReason, failedMessage string

	// unavailable: the controller of the external issuer is away.
	unavailable bool
}

// reconcileCertificate brings one certificate to its wanted state. It
// returns the name of its open order.
func (o *certificateOperator) reconcileCertificate(p *certificatePass, obj *structs.CustomObject) (order string) {
	spec := &structs.CertificateSpec{}
	status := &structs.CertificateStatus{}
	if err := structs.DecodeObjectPart(obj.Spec, spec); err != nil {
		o.logger.Warn("certificate with a spec that can't be decoded", "namespace", obj.Namespace, "certificate", obj.Name, "error", err)
		if !obj.Deleting() {
			return ""
		}
		// The deletion does not wait for a spec nobody can read (review
		// finding ATM-24n): the object would keep its finalizer for good.
		spec = certificateDeletionSpec(obj)
		o.logger.Warn("a certificate with a spec that can't be decoded is being deleted: "+
			"its certificates are revoked as its status lists them, and its secret is left as it is",
			"namespace", obj.Namespace, "certificate", obj.Name, "keep_certificates", spec.KeepCertificates)
	}
	if err := structs.DecodeObjectPart(obj.Status, status); err != nil {
		status = &structs.CertificateStatus{}
	}

	p.after = nil
	if obj.Deleting() {
		o.finishDeletion(p, obj, spec, status)
		return ""
	}

	secret, err := p.store.SecretByPath(p.ws, obj.Namespace, spec.SecretPath)
	if err != nil {
		o.logger.Warn("secret lookup failed", "namespace", obj.Namespace, "path", spec.SecretPath, "error", err)
		p.settled(obj, false)
		return status.Order
	}
	ours := secret != nil && secret.Labels[structs.SecretLabelCertificate] == obj.Name
	// A current version that is soft deleted or destroyed can't be read:
	// the certificate is not held, and a new one is issued into a new
	// version (review finding ATM-24). The old one is not revoked: a
	// deletion is no key compromise, and a soft delete can be undone.
	have := ours && !secret.Deleted && !secret.Destroyed && status.Serial != "" &&
		slices.Contains(secret.ItemKeys, structs.CertificateSecretItemCert) &&
		slices.Contains(secret.ItemKeys, structs.CertificateSecretItemKey)
	notAfter, _ := time.Parse(time.RFC3339, status.NotAfter)

	// A certificate in the secret that the status does not record: the
	// write of the status after its issuance was lost (review finding
	// ATM-24c). It is made known as an earlier certificate before anything
	// else is done, in a write of its own: the issuance that follows puts
	// another certificate in its place in the secret, and then nothing would
	// name its serial.
	if found := o.unrecorded(p, obj, spec, status, secret, ours); found != nil {
		if o.recordUnrecorded(p, obj, spec, status, found) {
			// The pass has not looked at the revocation orders.
			p.settled(obj, false)
			return status.Order
		}
		// Known by now, as an earlier certificate: the secret does not hold
		// the certificate of the status, and a new one is issued.
		have = false
	}

	out := &certificateOutcome{ready: have && p.now.Before(notAfter)}
	st := *status
	// The earlier certificates that have expired need no revocation any
	// more.
	st.Earlier = certificateEarlierValid(status.Earlier, p.now)

	issuerObj := p.issuers[spec.Issuer]
	issuerSpec := &structs.CertificateIssuerSpec{}
	if issuerObj != nil {
		if err := structs.DecodeObjectPart(issuerObj.Spec, issuerSpec); err != nil {
			issuerObj = nil
		}
	}

	switch refused := o.refusal(p, obj, spec, issuerObj, issuerSpec, secret, ours); {
	case refused != nil:
		// Nothing is issued for a certificate that is not allowed, and an
		// open order is dropped. What it has stays until it expires, unless
		// the namespace lost the names: then it is taken back.
		out.failedReason, out.failedMessage = refused.reason, refused.message
		o.dropOrder(p, &st)
		// The failures of a withdrawal under way are those of its
		// revocation; earlier ones were of the issuance.
		if prev, _ := certificateFailure(obj.Status); !refused.lostNames || prev != refused.reason {
			st.Failures, st.RetryAt = 0, ""
		}
		if refused.lostNames {
			o.withdraw(p, obj, spec, &st, secret, ours, refused.message, out)
		} else {
			o.settleHeld(p, obj, &st)
			o.settleEarlier(p, obj, &st)
		}

	default:
		// A withdrawal ends when the namespace has the names back. The
		// failures and the wait it left were those of its revocation, which
		// is not carried on (settleEarlier): they must not hold up the
		// issuance that follows (review finding ATM-24h).
		if prev, _ := certificateFailure(obj.Status); prev == "NamesNotAllowed" {
			st.Failures, st.RetryAt = 0, ""
		}
		o.settleHeld(p, obj, &st)
		o.settleEarlier(p, obj, &st)
		why := o.issuanceNeeded(p, spec, &st, have, issuerSpec)
		retryAt, _ := time.Parse(time.RFC3339, st.RetryAt)
		switch {
		case why == "":
			o.dropOrder(p, &st)
			st.Failures, st.RetryAt = 0, ""
			renewAt, _ := time.Parse(time.RFC3339, st.RenewAt)
			p.want(renewAt)

		case st.Order == "" && p.now.Before(retryAt):
			// The last attempt failed: wait before the next one.
			out.failedReason, out.failedMessage = certificateFailure(obj.Status)
			p.want(retryAt)

		case issuerSpec.Builtin != nil:
			o.issueBuiltin(p, obj, spec, &st, secret, have, out)

		default:
			o.issueExternal(p, obj, spec, &st, secret, have, issuerObj, out)
		}
	}

	// A certificate whose certificates are revoked at its deletion (review
	// finding ATM-24f), or that deletes its secret, needs the operator
	// then. The object endpoint puts the finalizer on when the object is
	// written (checkCertificateApply); here it follows the spec.
	finalizers := slices.Clone(obj.Finalizers)
	hasFinalizer := slices.Contains(finalizers, structs.OperatorsFinalizer)
	switch needed := spec.NeedsOperatorAtDeletion(); {
	case needed && !hasFinalizer:
		finalizers = append(finalizers, structs.OperatorsFinalizer)
	case !needed && hasFinalizer:
		finalizers = slices.DeleteFunc(finalizers, func(f string) bool { return f == structs.OperatorsFinalizer })
	}

	encoded, err := o.encodeStatus(p, obj, &st, out)
	if err != nil {
		o.logger.Warn("status of a certificate can't be encoded", "certificate", obj.Name, "error", err)
		p.settled(obj, false)
		return st.Order
	}
	_, ok := o.writeStatus(obj, encoded, &finalizers)
	p.settled(obj, ok)
	return st.Order
}

// encodeStatus returns the status of a certificate as a pass leaves it: what
// it recorded (st), and the conditions for what it found out (out).
func (o *certificateOperator) encodeStatus(p *certificatePass, obj *structs.CustomObject, st *structs.CertificateStatus,
	out *certificateOutcome) (map[string]any, error) {

	encoded, err := structs.EncodeObjectPart(st)
	if err != nil {
		return nil, err
	}
	conditions := newConditionSet(obj.Status, p.now)
	conditions.set(structs.CertificateConditionReady, out.ready, "", "")
	conditions.set(structs.CertificateConditionIssuing, out.progress == structs.CertificateConditionIssuing, "", "")
	conditions.set(structs.CertificateConditionRenewing, out.progress == structs.CertificateConditionRenewing, "", "")
	conditions.set(structs.CertificateConditionFailed, out.failedReason != "", out.failedReason, out.failedMessage)
	conditions.set(structs.CertificateConditionIssuerUnavailable, out.unavailable, "", "")
	// Degraded: something keeps the certificate from being issued or
	// renewed, whether or not it still has a valid one.
	conditions.set(structs.CertificateConditionDegraded, out.failedReason != "" || out.unavailable, "", "")
	encoded[structs.ObjectStatusConditions] = conditions.list()
	encoded[structs.ObjectStatusObservedGeneration] = obj.Generation
	return encoded, nil
}

// unrecorded returns the certificate that the secret of a certificate holds
// and the status does not record as the one that is held, as an entry for
// the earlier certificates, or nil: the operator has written it to the
// secret, and the write of the status that followed was lost (review finding
// ATM-24c). Without it nothing names the serial of that certificate once the
// next one is issued, and nobody can revoke it by its serial number.
//
// The secret is read only when its current version is not the one the status
// names, and when the operator wrote it for this very object
// (structs.SecretLabelCertificateID): the secret a deleted object left
// behind holds a certificate of that object, on which one created again
// under its name has no right (review finding ATM-24f). The labels and the
// items of a secret are the user's to write, though, and a certificate is
// public: a certificate counts as one of this object only if the secret
// holds the private key of it too (who holds the key of a certificate may
// have it revoked), and if an issuer the object can have had signed it: an
// issuing CA of the region, or the external issuer of the spec or of the
// status, by the roots its controller declared. Anything else in the secret
// is left alone.
func (o *certificateOperator) unrecorded(p *certificatePass, obj *structs.CustomObject, spec *structs.CertificateSpec,
	status *structs.CertificateStatus, secret *structs.Secret, ours bool) *structs.CertificateEarlier {

	if secret == nil || !ours || secret.Deleted || secret.Destroyed || secret.CurrentVersion == status.SecretVersion ||
		secret.Labels[structs.SecretLabelCertificateID] != obj.ID ||
		!slices.Contains(secret.ItemKeys, structs.CertificateSecretItemCert) ||
		!slices.Contains(secret.ItemKeys, structs.CertificateSecretItemKey) {
		return nil
	}
	items, _, err := o.srv.secretReadValue(obj.Namespace, spec.SecretPath, 0)
	if err != nil {
		o.logger.Debug("the secret of a certificate can't be read", "namespace", obj.Namespace, "path", spec.SecretPath, "error", err)
		return nil
	}
	chain, err := parseCertificatesPEM(items[structs.CertificateSecretItemCert])
	if err != nil || len(chain) == 0 {
		return nil
	}
	leaf := chain[0]
	serial := certificateSerial(leaf)
	if serial == status.Serial {
		return nil
	}
	key, err := parsePrivateKeyPEM(items[structs.CertificateSecretItemKey])
	if err != nil || !publicKeysEqual(leaf.PublicKey, key.Public()) {
		return nil
	}
	found := &structs.CertificateEarlier{Serial: serial, NotAfter: leaf.NotAfter.UTC().Format(time.RFC3339)}

	// The built-in issuer: the CA that signed it revokes it, whatever the
	// issuer of the certificate is now.
	for _, ca := range p.all {
		if ca.Region != o.srv.Region() {
			continue
		}
		certs, err := parseCertificatesPEM(ca.Cert)
		if err != nil || len(certs) == 0 || leaf.CheckSignatureFrom(certs[0]) != nil {
			continue
		}
		found.Authority, found.Issuer = ca.ID, status.Issuer
		if issuer := p.issuers[spec.Issuer]; issuer != nil {
			issuerSpec := &structs.CertificateIssuerSpec{}
			if err := structs.DecodeObjectPart(issuer.Spec, issuerSpec); err == nil && issuerSpec.Builtin != nil {
				found.Issuer = spec.Issuer
			}
		}
		return found
	}

	// An external issuer: the one the spec names, which the lost write was
	// most likely for, or the one of the certificate the status holds.
	intermediates := x509.NewCertPool()
	for _, cert := range chain[1:] {
		intermediates.AddCert(cert)
	}
	for i, name := range []string{spec.Issuer, status.Issuer} {
		issuer := p.issuers[name]
		if issuer == nil || (i == 1 && name == spec.Issuer) {
			continue
		}
		issuerSpec := &structs.CertificateIssuerSpec{}
		issuerStatus := &structs.CertificateIssuerStatus{}
		if err := structs.DecodeObjectPart(issuer.Spec, issuerSpec); err != nil || issuerSpec.External == nil {
			continue
		}
		_ = structs.DecodeObjectPart(issuer.Status, issuerStatus)
		declared, err := parseCertificatesPEM(issuerStatus.Roots)
		if err != nil || len(declared) == 0 {
			continue
		}
		roots := x509.NewCertPool()
		for _, root := range declared {
			roots.AddCert(root)
		}
		if _, err := leaf.Verify(x509.VerifyOptions{
			Roots:         roots,
			Intermediates: intermediates,
			CurrentTime:   p.now,
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
		}); err == nil {
			found.Issuer = name
			return found
		}
	}
	return nil
}

// recordUnrecorded makes a certificate the secret holds and the status does
// not record (unrecorded) known as an earlier certificate of the object, and
// reports whether the pass ends with that: the status is written with it and
// with nothing else, so that the entry is stored before the next issuance
// takes the certificate out of the secret. A write that fails is tried again
// by the next pass, which still finds the certificate in the secret. It
// reports false for a certificate that needs no entry: it has one, or it has
// expired.
func (o *certificateOperator) recordUnrecorded(p *certificatePass, obj *structs.CustomObject, spec *structs.CertificateSpec,
	status *structs.CertificateStatus, found *structs.CertificateEarlier) bool {

	earlier := certificateEarlierValid(status.Earlier, p.now)
	if len(certificateEarlierValid([]*structs.CertificateEarlier{found}, p.now)) == 0 ||
		slices.ContainsFunc(earlier, func(e *structs.CertificateEarlier) bool { return e.Serial == found.Serial }) {
		return false
	}
	part, err := structs.EncodeObjectPart(&structs.CertificateStatus{Earlier: o.appendEarlier(obj, earlier, found)})
	if err != nil {
		o.logger.Warn("status of a certificate can't be encoded", "certificate", obj.Name, "error", err)
		return false
	}
	changed := obj.Copy()
	if changed.Status == nil {
		changed.Status = map[string]any{}
	}
	changed.Status["earlier"] = part["earlier"]
	written, err := o.writeObject(changed)
	if err != nil {
		if !structs.IsObjectConflictError(err) {
			o.logger.Warn("failed to record a certificate that the status of its object did not know",
				"namespace", obj.Namespace, "certificate", obj.Name, "serial", found.Serial, "error", err)
		}
		p.want(p.now.Add(operatorsRetry))
		return true
	}
	o.srv.operatorEvent(written, structs.ObjectEventTypeWarning, "CertificateRecovered", fmt.Sprintf(
		"the secret %q holds a certificate with the serial %s, valid until %s, that the status did not record: "+
			"the write of the status after its issuance was lost; it is kept as an earlier certificate, "+
			"which can be revoked by its serial number, and a new certificate is issued",
		spec.SecretPath, found.Serial, found.NotAfter))
	return true
}

// certificateFailure returns the reason and the message of the Failed
// condition of a status, if it is true.
func certificateFailure(status map[string]any) (reason, message string) {
	for _, c := range operatorConditions(status) {
		if c.Type == structs.CertificateConditionFailed && c.Status == "True" {
			return c.Reason, c.Message
		}
	}
	return "", ""
}

// certificateRefusal says why nothing may be issued for a certificate.
type certificateRefusal struct {
	reason, message string

	// lostNames: the namespace may not ask for the names or the issuer of
	// the certificate (NamesNotAllowed), whatever kind of issuer signs
	// them; what it holds is taken back (withdraw).
	lostNames bool
}

// refusal checks what must hold before anything is issued for a
// certificate: the spec, the issuer, what the namespace may ask for, and
// that the secret is the certificate's own. The object endpoint checks the
// same when the object is written; the namespace, the issuer and the secret
// can change afterwards.
func (o *certificateOperator) refusal(p *certificatePass, obj *structs.CustomObject, spec *structs.CertificateSpec,
	issuerObj *structs.CustomObject, issuerSpec *structs.CertificateIssuerSpec, secret *structs.Secret, ours bool) *certificateRefusal {

	if err := spec.Validate(); err != nil {
		return &certificateRefusal{reason: "InvalidSpec", message: err.Error()}
	}
	ns, err := p.store.NamespaceByName(nil, obj.Namespace)
	if err != nil {
		return &certificateRefusal{reason: "NamespaceLookupFailed", message: err.Error()}
	}
	// What the namespace may ask for is checked whatever kind of issuer
	// signs: a right it lost is found also when the issuer is gone.
	if err := structs.CheckCertificateNames(spec, ns, true); err != nil {
		return &certificateRefusal{reason: "NamesNotAllowed", message: err.Error(), lostNames: true}
	}
	if issuerObj == nil || issuerObj.Deleting() {
		return &certificateRefusal{reason: "IssuerNotFound", message: fmt.Sprintf("the issuer %q does not exist", spec.Issuer)}
	}
	if err := structs.CheckCertificateNames(spec, ns, issuerSpec.Builtin != nil); err != nil {
		// IP addresses that an issuer recreated as an external one can't
		// sign: no right the namespace lost.
		return &certificateRefusal{reason: "NamesNotAllowed", message: err.Error()}
	}
	if secret != nil && !ours {
		return &certificateRefusal{reason: "SecretConflict", message: fmt.Sprintf(
			"the secret %q exists and was not written for this certificate: delete it or choose another secret_path", spec.SecretPath)}
	}
	return nil
}

// withdraw takes back the certificate of a namespace that may no longer ask
// for its names or its issuer (review finding ATM-21): the names may be
// another namespace's now, and the key and a valid certificate for them must
// not stay with it until the certificate expires. A certificate that is
// still valid is revoked with its issuer (cessationOfOperation), what the
// secret holds for the certificate is destroyed (removeSecret), and then the
// status no longer says that a certificate is held. Until the revocation is
// done (revokeWithdrawn) the serial stays in the status, and the next passes
// see to it; the secret is emptied all the same. A certificate no issuer is
// left to revoke (the external issuer is gone) is withdrawn without, which
// the event says.
//
// The earlier certificates of the object that are still valid are taken back
// the same way (withdrawEarlier, review finding ATM-24e), each on its own:
// none waits for another, nor does the certificate that is held. The event
// Withdrawn names the ones a pass has revoked or found without an issuer; it
// is recorded when the held certificate is given up, and again whenever a
// later pass has such news of an earlier one.
func (o *certificateOperator) withdraw(p *certificatePass, obj *structs.CustomObject, spec *structs.CertificateSpec,
	st *structs.CertificateStatus, secret *structs.Secret, ours bool, why string, out *certificateOutcome) {

	serial := st.Serial
	notAfter, _ := time.Parse(time.RFC3339, st.NotAfter)
	revoked, unrevocable := true, ""
	if serial != "" && p.now.Before(notAfter) {
		// Asked for, as by Certificate.Revoke: should the namespace get the
		// names back before the revocation is done, a new certificate is
		// issued (issuanceNeeded).
		st.Revoke = serial
		revoked, unrevocable = o.revokeWithdrawn(p, obj, st, "certificate", out)
		if unrevocable != "" {
			st.Revoke = ""
			out.failedMessage += "; " + unrevocable
		}
	}

	// The earlier certificates, before the list may lose its oldest entry
	// to the certificate that was held (keepEarlier).
	news := o.withdrawEarlier(p, obj, st, out)

	// given: what was held is given up in this pass.
	given := false
	removed, err := o.removeSecret(obj, spec, secret, ours)
	switch {
	case errors.Is(err, structs.ErrSecretConflict):
		o.secretChanged(p, obj, spec)
	case err != nil:
		o.logger.Warn("failed to take the certificate of a namespace that may no longer ask for it out of its secret",
			"namespace", obj.Namespace, "path", spec.SecretPath, "error", err)
		out.failedMessage += fmt.Sprintf("; taking the certificate out of the secret %q failed: %v", spec.SecretPath, err)
		p.want(p.now.Add(operatorsRetry))
	default:
		// The secret holds no certificate of the operator now.
		out.ready = false
		given = revoked && (serial != "" || removed)
	}
	if !given && len(news) == 0 {
		return
	}

	message := fmt.Sprintf("the namespace may no longer ask for this certificate (%s)", why)
	if given {
		// What was held stays known as an earlier certificate, and so do
		// the ones before it: the revocation by serial number finds them
		// there.
		*st = structs.CertificateStatus{Orders: st.Orders, Revoke: st.Revoke, Earlier: o.keepEarlier(p, obj, st)}
		switch {
		case unrevocable != "":
			message += fmt.Sprintf(": the certificate with the serial %s is not revoked, %s", serial, unrevocable)
			// Told here: the next passes, which try it with the earlier
			// certificates, do not tell it again.
			if i := slices.IndexFunc(st.Earlier, func(e *structs.CertificateEarlier) bool { return e.Serial == serial }); i >= 0 {
				entry := *st.Earlier[i]
				entry.NotRevocable = unrevocable
				st.Earlier = slices.Clone(st.Earlier)
				st.Earlier[i] = &entry
			}
		case serial != "" && st.Revoke == serial:
			message += fmt.Sprintf(": the certificate with the serial %s is revoked", serial)
		}
		if removed {
			message += fmt.Sprintf("; what the secret %q held for it is destroyed", spec.SecretPath)
		}
		operatorCount(structs.CertificateKind, "withdrawn")
	}
	for _, text := range news {
		message += "; " + text
	}
	o.srv.operatorEvent(obj, structs.ObjectEventTypeWarning, "Withdrawn", message)
}

// withdrawEarlier has the earlier certificates of a certificate under
// withdrawal revoked (review finding ATM-24e): replaced by a renewal, a
// changed spec or a deleted version of the secret, they are valid until their
// end for names the namespace may no longer ask for. Every entry that is not
// marked revoked (the expired ones are gone from the list) is revoked like
// the certificate that is held (revokeWithdrawn), with the issuer and the
// authority of its own and its own count of failures and wait, so that none
// holds up another. An entry is marked revoked when that is done: at once
// with the built-in issuer, with an external one when its revocation order is
// valid. One no issuer is left to revoke stays unmarked, with the reason in
// the entry, and is tried on every pass: the issuer may be there again.
//
// It returns what the event of the withdrawal has to tell, each only once:
// the serials revoked in this pass, and the entries found without an issuer
// that were not known to be so.
func (o *certificateOperator) withdrawEarlier(p *certificatePass, obj *structs.CustomObject,
	st *structs.CertificateStatus, out *certificateOutcome) (news []string) {

	var revoked []string
	earlier := slices.Clone(st.Earlier)
	for i, e := range earlier {
		if e.Revoked {
			// By a user, whatever the reason, or by a pass before. Only an
			// order of a user that raises its reason may still be open.
			if e.Authority == "" {
				entry := *e
				o.settleRevocation(p, obj, "earlier certificate", e.Serial, e.Issuer, &entry.Revoked, &entry.RevokeReason, &entry.Revoking)
				earlier[i] = &entry
			}
			continue
		}
		one := &structs.CertificateStatus{
			Issuer:    e.Issuer,
			Authority: e.Authority,
			Serial:    e.Serial,
			NotAfter:  e.NotAfter,
			Revoking:  e.Revoking,
			Failures:  e.Failures,
			RetryAt:   e.RetryAt,
		}
		done, unrevocable := o.revokeWithdrawn(p, obj, one, "earlier certificate", out)
		entry := *e
		entry.Revoked, entry.RevokeReason, entry.Revoking = one.Revoked, one.RevokeReason, one.Revoking
		entry.Failures, entry.RetryAt, entry.NotRevocable = one.Failures, one.RetryAt, unrevocable
		switch {
		case unrevocable != "":
			entry.Failures, entry.RetryAt = 0, ""
			text := fmt.Sprintf("the earlier certificate with the serial %s is not revoked, %s", e.Serial, unrevocable)
			out.failedMessage += "; " + text
			if e.NotRevocable != unrevocable {
				news = append(news, text)
			}
		case done:
			entry.Revoked, entry.Failures, entry.RetryAt = true, 0, ""
			revoked = append(revoked, e.Serial)
		}
		earlier[i] = &entry
	}
	st.Earlier = earlier

	switch len(revoked) {
	case 0:
	case 1:
		news = slices.Insert(news, 0, fmt.Sprintf("the earlier certificate with the serial %s is revoked", revoked[0]))
	default:
		news = slices.Insert(news, 0, fmt.Sprintf("the earlier certificates with the serials %s are revoked", strings.Join(revoked, ", ")))
	}
	return news
}

// certificateOrderReason returns the reason a revocation order carries, by
// the name of its RFC 5280 CRLReason: unspecified for none.
func certificateOrderReason(spec *structs.CertificateOrderSpec) string {
	if spec.Reason == "" {
		return structs.CertificateRevokeReasonUnspecified
	}
	return spec.Reason
}

// revocation says how the revocation order of a serial of a certificate
// stands in this pass: its state is "" if there is none, else
// structs.CertificateOrderValid, structs.CertificateOrderFailed, or
// structs.CertificateOrderPending for one its controller has not finished.
// reason is the reason the order carries (certificateOrderReason), problem
// why it failed. An order its controller did not answer within
// certificateRevokeOrderKeep has failed too (review finding ATM-24d):
// cleanOrders removes it at the end of this pass.
func (p *certificatePass) revocation(obj *structs.CustomObject, serial string) (state, reason, problem string) {
	order := p.orders[certificateRevokeOrderName(obj.Namespace, obj.Name, serial)]
	if order == nil {
		return "", "", ""
	}
	state, reason, problem = certificateRevocationState(order)
	if state == structs.CertificateOrderPending && certificateRevokeOrderExpired(order, p.now) {
		return structs.CertificateOrderFailed, reason, fmt.Sprintf(
			"its controller did not answer within %s", certificateRevokeOrderKeep)
	}
	return state, reason, problem
}

// certificateRevokeOrderExpired reports whether a revocation order has
// waited for the controller of its issuer for as long as it may.
func certificateRevokeOrderExpired(order *structs.CustomObject, now time.Time) bool {
	return !now.Before(time.Unix(0, order.CreateTime).Add(certificateRevokeOrderKeep))
}

// certificateRevocationState returns the state and the reason of a
// revocation order, and why it failed, as certificatePass.revocation tells
// them, but for the time the order has waited.
func certificateRevocationState(order *structs.CustomObject) (state, reason, problem string) {
	spec := &structs.CertificateOrderSpec{}
	status := &structs.CertificateOrderStatus{}
	_ = structs.DecodeObjectPart(order.Spec, spec)
	_ = structs.DecodeObjectPart(order.Status, status)
	switch status.State {
	case structs.CertificateOrderValid:
		return status.State, certificateOrderReason(spec), ""
	case structs.CertificateOrderFailed:
		problem = status.Problem
		if problem == "" {
			problem = "the issuer gave no reason"
		}
		return status.State, certificateOrderReason(spec), problem
	}
	return structs.CertificateOrderPending, certificateOrderReason(spec), ""
}

// listed reports whether the revocation list of a CA of the built-in issuer
// holds a serial of a certificate.
func (p *certificatePass) listed(obj *structs.CustomObject, authority, serial string) bool {
	for _, ca := range p.all {
		if ca.ID != authority {
			continue
		}
		for _, rev := range ca.Revoked {
			if rev.Serial == serial && rev.Certificate == obj.Namespace+"/"+obj.Name {
				return true
			}
		}
	}
	return false
}

// settleRevocation brings what the status says of the revocation of one
// certificate of an external issuer in line with the revocation order of its
// serial (review finding ATM-24g): revoked, reason and revoking are the
// fields of the certificate that is held (structs.CertificateStatus) or of
// an earlier one (structs.CertificateEarlier), what names it in the event
// ("certificate", "earlier certificate"). Revoked means that the issuer has
// done an order, whoever placed it, and the reason is that of the order; an
// order that is only placed marks nothing. revoking, which the endpoint of
// the revocation sets for the order of a user, follows the reason of the
// order while it is open and ends with it: an order that failed, or that is
// gone, leaves the certificate as it was, and nothing is placed again.
//
// The order of a user that the issuer failed, or did not answer in time, is
// told by the event RevocationFailed (review finding ATM-24d), once: in the
// pass that ends revoking, which is the pass in which cleanOrders removes
// the order. The events of an order that is over are told when the status
// that says the same is written (certificatePass.tell), and the order is
// removed only then (review finding ATM-24j): after a write of the status
// that is lost, the next pass finds the order as this one did, and neither
// the mark nor the event is lost or told twice. The conditions of the
// certificate are not touched: the certificate in use is as good as it was,
// and nothing is being tried.
func (o *certificateOperator) settleRevocation(p *certificatePass, obj *structs.CustomObject, what, serial, issuer string,
	revoked *bool, reason, revoking *string) {

	switch state, asked, problem := p.revocation(obj, serial); state {
	case structs.CertificateOrderValid:
		if !*revoked {
			message := fmt.Sprintf("%s with the serial %s revoked by the issuer %q for the reason %s: its revocation order is done",
				what, serial, issuer, asked)
			p.tell(func() { o.srv.operatorEvent(obj, structs.ObjectEventTypeNormal, "Revoked", message) })
		}
		*revoked, *reason, *revoking = true, asked, ""
	case structs.CertificateOrderPending:
		if *revoking != "" {
			*revoking = asked
		}
	case structs.CertificateOrderFailed:
		switch {
		case *revoking == "":
			// Not the order of a user (a withdrawal the namespace got the
			// names back from), or told before.
		case *revoked:
			message := fmt.Sprintf("the issuer %q did not take the reason %s for the %s with the serial %s, revoked before: %s; "+
				"it stays revoked for the reason %s, and nothing is tried again",
				issuer, asked, what, serial, problem, *reason)
			p.tell(func() { o.srv.operatorEvent(obj, structs.ObjectEventTypeWarning, "RevocationFailed", message) })
		default:
			message := fmt.Sprintf("the issuer %q did not revoke the %s with the serial %s: %s; "+
				"it is not revoked, and nothing is tried again: revoke it again to place a new order",
				issuer, what, serial, problem)
			p.tell(func() { o.srv.operatorEvent(obj, structs.ObjectEventTypeWarning, "RevocationFailed", message) })
		}
		*revoking = ""
	default:
		*revoking = ""
	}
}

// settleHeld looks after what the status says of the revocation of the
// certificate that is held (review finding ATM-24g), which goes with it into
// the earlier certificates when the next one is issued (keepEarlier): a
// revocation order may be done, and removed (cleanOrders), long before that.
// A certificate of the built-in issuer is revoked when the revocation list
// of its CA holds it.
func (o *certificateOperator) settleHeld(p *certificatePass, obj *structs.CustomObject, st *structs.CertificateStatus) {
	switch {
	case st.Serial == "":
		st.Revoked, st.RevokeReason, st.Revoking = false, "", ""
	case st.Authority != "":
		st.Revoked = st.Revoked || p.listed(obj, st.Authority, st.Serial)
	default:
		o.settleRevocation(p, obj, "certificate", st.Serial, st.Issuer, &st.Revoked, &st.RevokeReason, &st.Revoking)
	}
}

// settleEarlier looks after the earlier certificates of a certificate that
// is not under withdrawal. What a withdrawal left unfinished when the
// namespace got the names back is not carried on (review finding ATM-24e),
// as the revocation of the certificate that was held is not: a revocation
// order that is placed runs its course, a failed one is not placed again, and
// the count of failures, the wait and the missing issuer of an entry are
// forgotten; the next withdrawal starts anew. An entry whose revocation order
// the issuer has done is marked revoked, whoever placed the order, and only
// then (settleRevocation; review finding ATM-24g). One of the built-in issuer
// that the revocation list of its CA holds is marked too.
func (o *certificateOperator) settleEarlier(p *certificatePass, obj *structs.CustomObject, st *structs.CertificateStatus) {
	var earlier []*structs.CertificateEarlier
	for i, e := range st.Earlier {
		entry := *e
		if !e.Revoked {
			entry.Failures, entry.RetryAt, entry.NotRevocable = 0, "", ""
		}
		if e.Authority == "" {
			o.settleRevocation(p, obj, "earlier certificate", e.Serial, e.Issuer, &entry.Revoked, &entry.RevokeReason, &entry.Revoking)
		} else if !e.Revoked {
			entry.Revoked = p.listed(obj, e.Authority, e.Serial)
		}
		if entry == *e {
			continue
		}
		if earlier == nil {
			earlier = slices.Clone(st.Earlier)
		}
		earlier[i] = &entry
	}
	if earlier != nil {
		st.Earlier = earlier
	}
}

// revokeWithdrawn has a certificate a withdrawal takes back revoked, with
// the reason cessationOfOperation, and reports whether that is over: done,
// or no issuer is left to revoke it (unrevocable says why). The deletion of
// a certificate has its certificates revoked the same way (revokeDeleted;
// review finding ATM-24f). st is the status
// of the certificate that is held, or for an earlier certificate
// (withdrawEarlier) one with the serial, the issuer, the authority, the end,
// the failures and the wait of that one; what names it in the messages
// ("certificate", "earlier certificate"). The built-in
// issuer lists it in the revocation list of its CA at once. An external
// issuer revokes through a revocation order the withdrawal waits for: one
// that failed is counted (Failures, RetryAt), told in the event
// RevocationFailed, and placed again after a wait that doubles every time;
// cleanOrders removes the failed order after this pass has seen it. An order
// its controller did not answer in time has failed too
// (certificatePass.revocation; review finding ATM-24l). A
// revocation order of the serial that the user placed is waited for the
// same way. What is done is recorded in st (Revoked, and RevokeReason with
// an external issuer; review finding ATM-24g), which is what marks the
// certificate among the earlier ones; one that is revoked already is not
// revoked again.
func (o *certificateOperator) revokeWithdrawn(p *certificatePass, obj *structs.CustomObject,
	st *structs.CertificateStatus, what string, out *certificateOutcome) (done bool, unrevocable string) {

	serial := st.Serial
	if st.Revoked {
		// Revoked before, at the request of a user: its issuer is not asked
		// again.
		return true, ""
	}
	retryAt, _ := time.Parse(time.RFC3339, st.RetryAt)
	waiting := fmt.Sprintf("; waiting for the issuer %q to revoke the %s with the serial %s", st.Issuer, what, serial)
	if st.Authority == "" {
		switch state, asked, problem := p.revocation(obj, serial); state {
		case "":
			// An order of a user is over; the withdrawal sees to the rest.
			st.Revoking = ""
		case structs.CertificateOrderValid:
			st.Revoked, st.RevokeReason, st.Revoking = true, asked, ""
			return true, ""
		case structs.CertificateOrderFailed:
			// Failed by the issuer, or not answered in time.
			st.Revoking = ""
			if !p.now.Before(retryAt) {
				// Not counted yet.
				retryAt = o.retryLater(p, st)
				// Told when the status with the count is written: the
				// order stays until then (review finding ATM-24j).
				message := fmt.Sprintf("the issuer %q did not revoke the %s with the serial %s: %s; tried again at %s",
					st.Issuer, what, serial, problem, st.RetryAt)
				p.tell(func() { o.srv.operatorEvent(obj, structs.ObjectEventTypeWarning, "RevocationFailed", message) })
			}
			out.failedMessage += fmt.Sprintf("; revoking the %s with the serial %s failed (%s), tried again at %s",
				what, serial, problem, st.RetryAt)
			p.want(retryAt)
			return false, ""
		default:
			out.failedMessage += waiting
			return false, ""
		}
	}
	if p.now.Before(retryAt) {
		out.failedMessage += fmt.Sprintf("; revoking the %s with the serial %s is tried again at %s", what, serial, st.RetryAt)
		p.want(retryAt)
		return false, ""
	}

	_, err := o.srv.revokeWithIssuer(p.store, obj, st, structs.CertificateRevokeReasonCessationOfOperation)
	switch {
	case errors.Is(err, errCertificateNotRevocable):
		// Nothing to wait for: the certificate stays valid until it
		// expires, without its key in the secret.
		return true, fmt.Sprintf("%v; it stays valid until %s", err, st.NotAfter)
	case err != nil:
		o.logger.Warn("failed to revoke a certificate that is taken back or deleted",
			"namespace", obj.Namespace, "certificate", obj.Name, "serial", serial, "error", err)
		out.failedMessage += fmt.Sprintf("; revoking the %s with the serial %s failed: %v", what, serial, err)
		p.want(p.now.Add(operatorsRetry))
		return false, ""
	case st.Authority != "":
		st.Revoked = true
		return true, ""
	default:
		// The order is placed: its status brings the next pass.
		out.failedMessage += waiting
		return false, ""
	}
}

// removeSecret destroys what the secret of a certificate holds for it: with
// delete_secret the whole secret, as at the deletion of the certificate
// (finishDeletion), else every version that is not destroyed yet (the
// metadata, with the label of the certificate, stays for the next issuance).
// The operator writes nothing else into the secret. Only a secret that is
// the certificate's own is touched, and only as the pass read it: one
// changed since fails with structs.ErrSecretConflict. It reports whether
// anything was destroyed.
func (o *certificateOperator) removeSecret(obj *structs.CustomObject, spec *structs.CertificateSpec, secret *structs.Secret, ours bool) (bool, error) {
	if secret == nil || !ours {
		return false, nil
	}
	read := secret.ModifyIndex
	args := &structs.SecretVersionsRequest{Path: spec.SecretPath, CheckIndex: &read}
	args.Namespace = obj.Namespace
	args.Region = o.srv.Region()
	if !spec.DeleteSecret {
		for _, v := range secret.Versions {
			if !v.Destroyed {
				args.Versions = append(args.Versions, v.Version)
			}
		}
		if len(args.Versions) == 0 {
			return false, nil
		}
	}
	_, _, err := o.srv.secretVersionsOp(structs.SecretOpDestroy, args)
	if errors.Is(err, structs.ErrSecretNotFound) {
		return false, nil
	}
	return err == nil, err
}

// issuanceNeeded says why a certificate has to be issued now, or "".
func (o *certificateOperator) issuanceNeeded(p *certificatePass, spec *structs.CertificateSpec, st *structs.CertificateStatus,
	have bool, issuerSpec *structs.CertificateIssuerSpec) string {

	renewAt, _ := time.Parse(time.RFC3339, st.RenewAt)
	switch {
	case !have:
		return "Issuing"
	case st.Request != spec.RequestHash():
		return "SpecChanged"
	case st.Revoke != "" && st.Revoke == st.Serial:
		return "Revoked"
	case issuerSpec.Builtin != nil && p.active != nil && st.Authority != p.active.ID:
		return "AuthorityChanged"
	case !renewAt.IsZero() && !p.now.Before(renewAt):
		return "RenewalDue"
	}
	return ""
}

// certificateRenewAt returns when a certificate is renewed: the time the
// issuer suggests, else renew_before ahead of the end of the validity, else
// at two thirds of the validity.
func certificateRenewAt(cert *x509.Certificate, spec *structs.CertificateSpec, suggested string) time.Time {
	if t, err := time.Parse(time.RFC3339, suggested); err == nil && t.After(cert.NotBefore) && t.Before(cert.NotAfter) {
		return t
	}
	if before, err := spec.RenewBeforeDuration(); err == nil && before > 0 && before < cert.NotAfter.Sub(cert.NotBefore) {
		return cert.NotAfter.Add(-before)
	}
	return cert.NotBefore.Add(cert.NotAfter.Sub(cert.NotBefore) * 2 / 3)
}

// fail records a failed attempt: the next one waits, longer every time.
func (o *certificateOperator) fail(p *certificatePass, obj *structs.CustomObject, st *structs.CertificateStatus,
	out *certificateOutcome, reason, message string) {

	o.retryLater(p, st)
	out.failedReason, out.failedMessage = reason, message
	operatorCount(structs.CertificateKind, "failures")
	o.srv.operatorEvent(obj, structs.ObjectEventTypeWarning, reason, message)
}

// retryLater counts a failed attempt and returns when the next one is made:
// the wait doubles with every failure in a row.
func (o *certificateOperator) retryLater(p *certificatePass, st *structs.CertificateStatus) time.Time {
	st.Failures++
	wait := certificateRetryMax
	if st.Failures < 16 {
		wait = min(certificateRetryBase<<(st.Failures-1), certificateRetryMax)
	}
	retryAt := p.now.Add(wait)
	st.RetryAt = retryAt.Format(time.RFC3339)
	p.want(retryAt)
	return retryAt
}

// certificateEarlierValid returns the earlier certificates that have not
// expired at now, in a list of its own if any is left out. An entry without
// a readable end counts as expired.
func certificateEarlierValid(earlier []*structs.CertificateEarlier, now time.Time) []*structs.CertificateEarlier {
	valid := func(e *structs.CertificateEarlier) bool {
		if e == nil {
			return false
		}
		notAfter, err := time.Parse(time.RFC3339, e.NotAfter)
		return err == nil && now.Before(notAfter)
	}
	if !slices.ContainsFunc(earlier, func(e *structs.CertificateEarlier) bool { return !valid(e) }) {
		return earlier
	}
	var out []*structs.CertificateEarlier
	for _, e := range earlier {
		if valid(e) {
			out = append(out, e)
		}
	}
	return out
}

// keepEarlier returns the earlier certificates of a status whose current
// certificate is about to leave it, with that certificate added if it is
// still valid (review finding ATM-24b): replaced or taken back, it stays
// valid until its end, and its revocation by serial number needs to know
// who signed it. One that is revoked is kept too, marked: the built-in
// issuer has it in the revocation list of its CA, but an external issuer has
// no such list, and the reason of either may still be raised to
// keyCompromise (review finding ATM-25b). The mark is for a revocation that
// is done, as the status says it of the certificate that is held (review
// finding ATM-24g): one that was asked for, the serial in revoke, and whose
// order is open, failed or waiting to be placed again enters unmarked, with
// revoking if a user asked for it and the order is open (settleEarlier then
// marks it when the issuer has done the order). The list is bounded: beyond
// structs.CertificateEarlierMax the oldest entry goes, and an event names
// its serial, which the status then no longer vouches for. The list of st
// is not changed.
func (o *certificateOperator) keepEarlier(p *certificatePass, obj *structs.CustomObject, st *structs.CertificateStatus) []*structs.CertificateEarlier {
	notAfter, err := time.Parse(time.RFC3339, st.NotAfter)
	if st.Serial == "" || err != nil || !p.now.Before(notAfter) ||
		slices.ContainsFunc(st.Earlier, func(e *structs.CertificateEarlier) bool { return e.Serial == st.Serial }) {
		return st.Earlier
	}
	return o.appendEarlier(obj, st.Earlier, &structs.CertificateEarlier{
		Serial:    st.Serial,
		Issuer:    st.Issuer,
		Authority: st.Authority,
		NotAfter:  st.NotAfter,
		// What is done, not what was asked for (review finding ATM-24g): the
		// callers have brought these in line with the issuer in this pass
		// (settleHeld, revokeWithdrawn).
		Revoked:      st.Revoked,
		RevokeReason: st.RevokeReason,
		Revoking:     st.Revoking,
	})
}

// appendEarlier returns the earlier certificates of an object with one more.
// The list is bounded (keepEarlier): the oldest entries go, each with an
// event. The list given is not changed.
func (o *certificateOperator) appendEarlier(obj *structs.CustomObject, list []*structs.CertificateEarlier,
	entry *structs.CertificateEarlier) []*structs.CertificateEarlier {

	earlier := append(slices.Clone(list), entry)
	for len(earlier) > structs.CertificateEarlierMax {
		dropped := earlier[0]
		earlier = earlier[1:]
		o.srv.operatorEvent(obj, structs.ObjectEventTypeWarning, "EarlierCertificateDropped", fmt.Sprintf(
			"the earlier certificate with the serial %s, valid until %s, is no longer kept in the status: "+
				"more than %d earlier certificates are still valid; it can no longer be revoked by its serial number",
			dropped.Serial, dropped.NotAfter, structs.CertificateEarlierMax))
	}
	return earlier
}

// issued records a new certificate in the status. The certificate it takes
// the place of is kept as an earlier one (keepEarlier).
func (o *certificateOperator) issued(p *certificatePass, obj *structs.CustomObject, spec *structs.CertificateSpec,
	st *structs.CertificateStatus, out *certificateOutcome, cert *x509.Certificate, authority, suggestedRenewal string, version uint64, renewal bool) {

	renewAt := certificateRenewAt(cert, spec, suggestedRenewal)
	// Never due at once, whatever the certificate says: the renewal would
	// repeat on every run of the operator.
	if earliest := p.now.Add(operatorsRetry); renewAt.Before(earliest) {
		renewAt = earliest
	}
	*st = structs.CertificateStatus{
		Issuer:        spec.Issuer,
		Authority:     authority,
		Serial:        certificateSerial(cert),
		NotBefore:     cert.NotBefore.UTC().Format(time.RFC3339),
		NotAfter:      cert.NotAfter.UTC().Format(time.RFC3339),
		RenewAt:       renewAt.UTC().Format(time.RFC3339),
		SecretVersion: version,
		Request:       spec.RequestHash(),
		Orders:        st.Orders,
		Earlier:       o.keepEarlier(p, obj, st),
	}
	out.ready = p.now.Before(cert.NotAfter)
	out.progress, out.failedReason, out.failedMessage = "", "", ""
	p.want(renewAt)

	reason, what := "Issued", "issued"
	if renewal {
		reason, what = "Renewed", "renewed"
	}
	operatorCount(structs.CertificateKind, "issued")
	o.srv.operatorEvent(obj, structs.ObjectEventTypeNormal, reason, fmt.Sprintf(
		"certificate %s by the issuer %q: serial %s, valid until %s, version %d of the secret %q",
		what, spec.Issuer, st.Serial, st.NotAfter, version, spec.SecretPath))
}

// writeSecret writes a new version of the secret of a certificate. The
// metadata of an existing secret is kept, apart from the expiry. existing is
// the secret as the pass read it, nil if there was none: the write is a
// check-and-set on it, and fails with structs.ErrSecretConflict if somebody
// else created or changed the secret since (secretChanged).
func (o *certificateOperator) writeSecret(obj *structs.CustomObject, spec *structs.CertificateSpec, existing *structs.Secret,
	items structs.SecretItems, expires *time.Time) (uint64, error) {

	meta := &structs.SecretMeta{
		Description: fmt.Sprintf("Certificate %s (%s/%s)", obj.Name, structs.OperatorsGroup, structs.CertificateKind),
	}
	var checkIndex uint64
	if existing != nil {
		meta = existing.SecretMeta.Copy()
		checkIndex = existing.ModifyIndex
	}
	if meta.Labels == nil {
		meta.Labels = map[string]string{}
	}
	meta.Labels[structs.SecretLabelCertificate] = obj.Name
	meta.Labels[structs.SecretLabelCertificateID] = obj.ID
	if expires != nil {
		meta.ExpiresAt = expires
	}
	written, _, err := o.srv.secretWrite(&secretWrite{
		namespace:  obj.Namespace,
		path:       spec.SecretPath,
		items:      items,
		meta:       meta,
		checkIndex: &checkIndex,
	})
	if err != nil {
		return 0, err
	}
	return written.CurrentVersion, nil
}

// secretChanged handles a write of the secret of a certificate that found
// the secret changed since the pass read it: nothing is overwritten, and the
// next pass reads the secret again and decides whether it is still the
// certificate's (refusal).
func (o *certificateOperator) secretChanged(p *certificatePass, obj *structs.CustomObject, spec *structs.CertificateSpec) {
	o.logger.Debug("the secret of a certificate changed while it was written; reading it again",
		"namespace", obj.Namespace, "certificate", obj.Name, "path", spec.SecretPath)
	p.want(p.now.Add(operatorsSettle))
}

// issueBuiltin issues a certificate with the issuing CA of the region.
func (o *certificateOperator) issueBuiltin(p *certificatePass, obj *structs.CustomObject, spec *structs.CertificateSpec,
	st *structs.CertificateStatus, secret *structs.Secret, have bool, out *certificateOutcome) {

	out.progress = structs.CertificateConditionIssuing
	if have {
		out.progress = structs.CertificateConditionRenewing
	}
	// An order of an earlier, external issuer of the certificate is over.
	o.dropOrder(p, st)
	if p.active == nil {
		out.failedReason, out.failedMessage = "AuthorityNotReady", "the issuing certificate authority of the region is not ready yet"
		p.want(p.now.Add(operatorsRetry))
		return
	}
	if certificateAuthorityExpiring(p.active, p.now) {
		o.fail(p, obj, st, out, "AuthorityExpired", fmt.Sprintf(
			"the issuing certificate authority %s ends at %s; nothing is issued with it",
			p.active.ID, p.active.NotAfter.UTC().Format(time.RFC3339)))
		return
	}
	validity, err := spec.ValidityDuration()
	if err != nil {
		out.failedReason, out.failedMessage = "InvalidSpec", err.Error()
		return
	}
	signer, err := o.srv.certificateCA.signer(p.active)
	if err != nil {
		o.fail(p, obj, st, out, "AuthorityKeyUnavailable", err.Error())
		return
	}
	key, err := generateCertificateKey(spec.KeyType)
	if err != nil {
		o.fail(p, obj, st, out, "KeyGenerationFailed", err.Error())
		return
	}
	certPEM, cert, err := signCertificate(signer, key.Public(), spec, validity)
	if err != nil {
		o.fail(p, obj, st, out, "SigningFailed", err.Error())
		return
	}
	keyPEM, err := encodePrivateKeyPEM(key)
	if err != nil {
		o.fail(p, obj, st, out, "KeyEncodingFailed", err.Error())
		return
	}
	items := structs.SecretItems{
		structs.CertificateSecretItemCert: certPEM + certificateIntermediates(p.active),
		structs.CertificateSecretItemKey:  keyPEM,
		structs.CertificateSecretItemCA:   certificateTrustBundle(o.srv.Region(), p.all),
	}
	version, err := o.writeSecret(obj, spec, secret, items, &cert.NotAfter)
	if errors.Is(err, structs.ErrSecretConflict) {
		o.secretChanged(p, obj, spec)
		return
	}
	if err != nil {
		o.fail(p, obj, st, out, "SecretWriteFailed", err.Error())
		return
	}
	o.issued(p, obj, spec, st, out, cert, p.active.ID, "", version, have)
}

// dropOrder deletes the open order of a certificate and forgets it.
func (o *certificateOperator) dropOrder(p *certificatePass, st *structs.CertificateStatus) {
	if st.Order == "" {
		return
	}
	if order := p.orders[st.Order]; order != nil {
		o.deleteOrder(order)
	}
	st.Order, st.OrderRequest = "", ""
}

// deleteOrder removes an order. A failure is left to the next pass.
func (o *certificateOperator) deleteOrder(order *structs.CustomObject) {
	if _, err := o.srv.removeCustomObject(order, structs.ObjectPropagationBackground); err != nil && !structs.IsObjectConflictError(err) {
		o.logger.Warn("failed to delete a certificate order", "order", order.Name, "error", err)
	}
}

// issueExternal moves the issuance by an external issuer one step on: it
// places the order, or takes the chain of an order that is done.
func (o *certificateOperator) issueExternal(p *certificatePass, obj *structs.CustomObject, spec *structs.CertificateSpec,
	st *structs.CertificateStatus, secret *structs.Secret, have bool, issuerObj *structs.CustomObject, out *certificateOutcome) {

	out.progress = structs.CertificateConditionIssuing
	if have {
		out.progress = structs.CertificateConditionRenewing
	}
	out.unavailable = !o.issuerAvailable(p, issuerObj)
	request := spec.RequestHash()

	if st.Order != "" {
		order := p.orders[st.Order]
		if order == nil || st.OrderRequest != request {
			// The order is gone, or it was placed for other names: start
			// over at the next pass.
			o.dropOrder(p, st)
			p.want(p.now.Add(operatorsSettle))
			return
		}
		orderStatus := &structs.CertificateOrderStatus{}
		_ = structs.DecodeObjectPart(order.Status, orderStatus)
		switch orderStatus.State {
		case structs.CertificateOrderValid:
			o.takeChain(p, obj, spec, st, secret, have, issuerObj, order, orderStatus, out)
		case structs.CertificateOrderFailed:
			o.dropOrder(p, st)
			problem := orderStatus.Problem
			if problem == "" {
				problem = "the issuer gave no reason"
			}
			o.fail(p, obj, st, out, "OrderFailed", problem)
		}
		return
	}

	if out.unavailable {
		// Nothing is ordered from an issuer whose controller is away; the
		// current certificate stays.
		return
	}
	if len(spec.IPAddresses) > 0 {
		out.failedReason, out.failedMessage = "NamesNotAllowed", "IP addresses are signed by the built-in issuer only"
		return
	}

	key, err := generateCertificateKey(spec.KeyType)
	if err != nil {
		o.fail(p, obj, st, out, "KeyGenerationFailed", err.Error())
		return
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: slices.Clone(spec.DNSNames)}, key)
	if err != nil {
		o.fail(p, obj, st, out, "SigningRequestFailed", err.Error())
		return
	}
	keyPEM, err := encodePrivateKeyPEM(key)
	if err != nil {
		o.fail(p, obj, st, out, "KeyEncodingFailed", err.Error())
		return
	}

	// The key waits in the secret, next to the current certificate. A
	// secret whose current version is soft deleted or destroyed (by a
	// withdrawal or a user) holds none that can be kept.
	items := structs.SecretItems{}
	if secret != nil && !secret.Deleted && !secret.Destroyed {
		current, _, err := o.srv.secretReadValue(obj.Namespace, spec.SecretPath, 0)
		if err != nil {
			o.fail(p, obj, st, out, "SecretReadFailed", err.Error())
			return
		}
		for _, k := range []string{structs.CertificateSecretItemCert, structs.CertificateSecretItemKey, structs.CertificateSecretItemCA} {
			if v, ok := current[k]; ok {
				items[k] = v
			}
		}
	}
	items[structs.CertificateSecretItemNextKey] = keyPEM
	if _, err := o.writeSecret(obj, spec, secret, items, nil); err != nil {
		if errors.Is(err, structs.ErrSecretConflict) {
			o.secretChanged(p, obj, spec)
			return
		}
		o.fail(p, obj, st, out, "SecretWriteFailed", err.Error())
		return
	}

	st.Orders++
	name := structs.CertificateOrderName(obj.Namespace, obj.Name, st.Orders)
	if stale := p.orders[name]; stale != nil {
		// An order of an attempt whose status write was lost.
		o.deleteOrder(stale)
	}
	orderSpec, err := structs.EncodeObjectPart(&structs.CertificateOrderSpec{
		Issuer:      spec.Issuer,
		Certificate: obj.Namespace + "/" + obj.Name,
		CSR:         string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})),
		DNSNames:    slices.Clone(spec.DNSNames),
	})
	if err == nil {
		_, err = o.srv.writeOperatorObject(&structs.CustomObject{
			Group:     structs.OperatorsGroup,
			Kind:      structs.CertificateOrderKind,
			Name:      name,
			ManagedBy: structs.OperatorsManagedBy,
			Spec:      orderSpec,
			Status:    map[string]any{"state": structs.CertificateOrderPending},
		})
	}
	if err != nil {
		st.Orders--
		o.fail(p, obj, st, out, "OrderWriteFailed", err.Error())
		return
	}
	st.Order, st.OrderRequest = name, request
	operatorCount(structs.CertificateKind, "orders")
	o.srv.operatorEvent(obj, structs.ObjectEventTypeNormal, "Ordered",
		fmt.Sprintf("order %s placed with the issuer %q", name, spec.Issuer))
}

// takeChain checks the chain of an order that is done and writes the
// certificate to the secret.
func (o *certificateOperator) takeChain(p *certificatePass, obj *structs.CustomObject, spec *structs.CertificateSpec,
	st *structs.CertificateStatus, secret *structs.Secret, have bool, issuerObj, order *structs.CustomObject,
	orderStatus *structs.CertificateOrderStatus, out *certificateOutcome) {

	if secret == nil {
		o.dropOrder(p, st)
		o.fail(p, obj, st, out, "KeyLost", "the secret with the key of the order is gone")
		return
	}
	if secret.Deleted || secret.Destroyed {
		// Waiting would not bring the key back: a version can only be
		// undeleted by hand.
		o.dropOrder(p, st)
		o.fail(p, obj, st, out, "KeyLost", "the version of the secret with the key of the order is deleted")
		return
	}
	current, _, err := o.srv.secretReadValue(obj.Namespace, spec.SecretPath, 0)
	if err != nil {
		// The order stays: the secret store may be back at the next pass.
		out.failedReason, out.failedMessage = "SecretReadFailed", err.Error()
		p.want(p.now.Add(operatorsRetry))
		return
	}
	keyPEM := current[structs.CertificateSecretItemNextKey]
	key, err := parsePrivateKeyPEM(keyPEM)
	if err != nil {
		o.dropOrder(p, st)
		o.fail(p, obj, st, out, "KeyLost", "the secret no longer holds the key of the order")
		return
	}
	issuerStatus := &structs.CertificateIssuerStatus{}
	_ = structs.DecodeObjectPart(issuerObj.Status, issuerStatus)
	leaf, chainPEM, err := checkOrderChain(orderStatus.Chain, key.Public(), spec, issuerStatus.Roots, p.now)
	if err != nil {
		o.dropOrder(p, st)
		o.fail(p, obj, st, out, "ChainRefused", fmt.Sprintf("the chain of the order %s is not accepted: %v", order.Name, err))
		return
	}
	items := structs.SecretItems{
		structs.CertificateSecretItemCert: chainPEM,
		structs.CertificateSecretItemKey:  keyPEM,
	}
	version, err := o.writeSecret(obj, spec, secret, items, &leaf.NotAfter)
	if errors.Is(err, structs.ErrSecretConflict) {
		// The order stays: the next pass takes its chain if the secret
		// still holds the key of the order.
		o.secretChanged(p, obj, spec)
		return
	}
	if err != nil {
		out.failedReason, out.failedMessage = "SecretWriteFailed", err.Error()
		p.want(p.now.Add(operatorsRetry))
		return
	}
	o.deleteOrder(order)
	o.issued(p, obj, spec, st, out, leaf, "", orderStatus.RenewAfter, version, have)
}

// checkOrderChain checks the chain an external issuer returned before it is
// trusted: the leaf is for the key of the request and for exactly the names
// asked for, it is valid now, and the chain leads to a root the controller
// of the issuer declared. It returns the leaf and the chain to store: the
// leaf and the certificates between it and the root.
func checkOrderChain(chainPEM string, pub crypto.PublicKey, spec *structs.CertificateSpec, rootsPEM string, now time.Time) (*x509.Certificate, string, error) {
	certs, err := parseCertificatesPEM(chainPEM)
	if err != nil {
		return nil, "", err
	}
	if len(certs) == 0 {
		return nil, "", errors.New("it holds no certificate")
	}
	leaf := certs[0]
	if !publicKeysEqual(leaf.PublicKey, pub) {
		return nil, "", errors.New("the certificate is not for the key of the request")
	}
	if leaf.IsCA {
		return nil, "", errors.New("the certificate is a CA certificate")
	}
	got := slices.Clone(leaf.DNSNames)
	want := slices.Clone(spec.DNSNames)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		return nil, "", fmt.Errorf("the certificate is for the names %s, not for %s",
			strings.Join(got, ", "), strings.Join(want, ", "))
	}
	if len(leaf.IPAddresses) > 0 || len(leaf.EmailAddresses) > 0 || len(leaf.URIs) > 0 {
		return nil, "", errors.New("the certificate carries names that were not asked for")
	}

	roots := x509.NewCertPool()
	declared, err := parseCertificatesPEM(rootsPEM)
	if err != nil || len(declared) == 0 {
		return nil, "", errors.New("the controller of the issuer declared no roots")
	}
	for _, root := range declared {
		roots.AddCert(root)
	}
	intermediates := x509.NewCertPool()
	var stored strings.Builder
	stored.WriteString(encodeCertificatePEM(leaf.Raw))
	for _, cert := range certs[1:] {
		if slices.ContainsFunc(declared, func(root *x509.Certificate) bool { return root.Equal(cert) }) {
			// A root at the end of the chain is not served.
			continue
		}
		intermediates.AddCert(cert)
		stored.WriteString(encodeCertificatePEM(cert.Raw))
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		CurrentTime:   now,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return nil, "", err
	}
	return leaf, stored.String(), nil
}

// certificateDeletionSpec returns the spec the deletion of a certificate goes
// by when its spec can't be decoded (review finding ATM-24n). The revocation
// needs none: the status lists the certificates of the object and who
// signed them, and they are revoked as by default, unless the spec says
// keep_certificates in a way that can be read on its own. The secret is
// left alone, whatever the spec holds: nothing is destroyed on the word of a
// spec that can't be read, and a secret that stays can be deleted by hand.
func certificateDeletionSpec(obj *structs.CustomObject) *structs.CertificateSpec {
	spec := &structs.CertificateSpec{}
	spec.KeepCertificates, _ = obj.Spec["keep_certificates"].(bool)
	return spec
}

// finishDeletion does what the deletion of a certificate asks of the
// operator, and lets the object go when that is over: it drops the open
// order, has the certificates of the object revoked unless the spec keeps
// them (revokeDeleted; review finding ATM-24f), and deletes the secret if the
// spec says so.
//
// The object stays, with the finalizer of the servers, until the revocation
// of each of its valid certificates is closed. While it waits, the status is
// written as in any other pass: what is revoked, the failures and the waits,
// and the condition Failed with the reason RevocationPending, whose message
// says what the deletion waits for. The secret does not wait: with
// delete_secret it is destroyed in the first pass.
//
// The spec is read in every pass: an object that waits goes at once when it
// is changed to keep_certificates. One that can't be decoded does not hold
// up the deletion (certificateDeletionSpec).
func (o *certificateOperator) finishDeletion(p *certificatePass, obj *structs.CustomObject, spec *structs.CertificateSpec, status *structs.CertificateStatus) {
	if !slices.Contains(obj.Finalizers, structs.OperatorsFinalizer) {
		return
	}
	st := *status
	st.Earlier = certificateEarlierValid(status.Earlier, p.now)
	out := &certificateOutcome{}
	o.dropOrder(p, &st)

	closed := true
	var unrevoked []string
	if !spec.KeepCertificates {
		// A certificate the status does not record (review finding ATM-24c)
		// is revoked with the others.
		if spec.SecretPath == "" {
			// A spec that can't be decoded (certificateDeletionSpec).
		} else if secret, err := p.store.SecretByPath(p.ws, obj.Namespace, spec.SecretPath); err != nil {
			o.logger.Warn("secret lookup failed", "namespace", obj.Namespace, "path", spec.SecretPath, "error", err)
		} else {
			ours := secret != nil && secret.Labels[structs.SecretLabelCertificate] == obj.Name
			found := o.unrecorded(p, obj, spec, status, secret, ours)
			if found != nil && len(certificateEarlierValid([]*structs.CertificateEarlier{found}, p.now)) == 1 &&
				!slices.ContainsFunc(st.Earlier, func(e *structs.CertificateEarlier) bool { return e.Serial == found.Serial }) {
				st.Earlier = o.appendEarlier(obj, st.Earlier, found)
			}
		}
		closed, unrevoked = o.revokeDeleted(p, obj, &st, out)
	}
	if closed {
		out.failedMessage = ""
	} else {
		out.failedReason = certificateReasonRevocationPending
		out.failedMessage = "the certificate is being deleted, and its certificates that are still valid are revoked first" + out.failedMessage
	}
	secretGone := !spec.DeleteSecret || o.deleteSecret(p, obj, spec)

	encoded, err := o.encodeStatus(p, obj, &st, out)
	if err != nil {
		o.logger.Warn("status of a certificate can't be encoded", "certificate", obj.Name, "error", err)
		p.settled(obj, false)
		return
	}
	if !closed || !secretGone {
		_, ok := o.writeStatus(obj, encoded, nil)
		p.settled(obj, ok)
		return
	}

	changed := obj.Copy()
	changed.Status = encoded
	changed.Finalizers = slices.DeleteFunc(changed.Finalizers, func(f string) bool { return f == structs.OperatorsFinalizer })
	written, err := o.writeObject(changed)
	if err != nil {
		if !structs.IsObjectConflictError(err) {
			o.logger.Warn("failed to remove the finalizer of a deleted certificate", "certificate", obj.Name, "error", err)
		}
		p.settled(obj, false)
		return
	}
	p.settled(obj, true)
	if len(unrevoked) > 0 {
		// The events of the object go with it: the log keeps what nobody
		// could revoke.
		o.logger.Warn("a deleted certificate leaves certificates that nobody could revoke; they stay valid until they expire",
			"namespace", obj.Namespace, "certificate", obj.Name, "serials", strings.Join(unrevoked, ", "))
	}
	o.srv.finalizeObjectIfReady(written)
}

// revokeDeleted has the certificates of a certificate that is being deleted
// revoked (review finding ATM-24f): the one that is held and the earlier
// ones, as far as they are still valid and not revoked. With the object its
// status goes, the only record of who signed them: nothing could revoke them
// by their serial number afterwards. They are revoked as a withdrawal revokes
// them, each on its own (revokeWithdrawn, withdrawEarlier): the built-in
// issuer at once, an external one through a revocation order that is waited
// for, counted when it fails and placed again after a wait that doubles.
//
// It reports whether the revocation is closed, which is what the object
// waits for: every certificate is revoked, or has no issuer left to revoke it
// (unrevoked names those), and the expired ones need none. A revocation that
// fails is never given up while its certificate is valid and its issuer is
// there. The event RevokedOnDelete tells what a pass has revoked, and what it
// found without an issuer: an earlier certificate once, the held one when
// the object goes.
func (o *certificateOperator) revokeDeleted(p *certificatePass, obj *structs.CustomObject, st *structs.CertificateStatus,
	out *certificateOutcome) (closed bool, unrevoked []string) {

	// The failures and the wait of the status are those of this revocation,
	// or of the withdrawal it carries on; earlier ones were of an issuance.
	if prev, _ := certificateFailure(obj.Status); prev != certificateReasonRevocationPending && prev != "NamesNotAllowed" {
		st.Failures, st.RetryAt = 0, ""
	}

	closed = true
	var news []string
	heldUnrevoked := ""
	notAfter, _ := time.Parse(time.RFC3339, st.NotAfter)
	if st.Serial != "" && p.now.Before(notAfter) {
		before := st.Revoked
		done, unrevocable := o.revokeWithdrawn(p, obj, st, "certificate", out)
		switch {
		case unrevocable != "":
			unrevoked = append(unrevoked, st.Serial)
			heldUnrevoked = fmt.Sprintf("the certificate with the serial %s is not revoked, %s", st.Serial, unrevocable)
			out.failedMessage += "; " + heldUnrevoked
		case !done:
			closed = false
		case !before:
			news = append(news, fmt.Sprintf("the certificate with the serial %s is revoked", st.Serial))
		}
	}

	news = append(news, o.withdrawEarlier(p, obj, st, out)...)
	for _, e := range st.Earlier {
		switch {
		case e.Revoked:
		case e.NotRevocable != "":
			unrevoked = append(unrevoked, e.Serial)
		default:
			closed = false
		}
	}
	if closed && heldUnrevoked != "" {
		news = append(news, heldUnrevoked)
	}
	if len(news) > 0 {
		eventType := structs.ObjectEventTypeNormal
		if slices.ContainsFunc(news, func(text string) bool { return strings.Contains(text, "is not revoked") }) {
			eventType = structs.ObjectEventTypeWarning
		}
		o.srv.operatorEvent(obj, eventType, "RevokedOnDelete", "the certificate is being deleted: "+strings.Join(news, "; "))
	}
	return closed, unrevoked
}

// deleteSecret destroys the secret of a deleted certificate, if it is the
// certificate's own, and reports whether it is gone. One that could not be
// destroyed is left to the next pass.
func (o *certificateOperator) deleteSecret(p *certificatePass, obj *structs.CustomObject, spec *structs.CertificateSpec) bool {
	secret, err := o.srv.State().SecretByPath(nil, obj.Namespace, spec.SecretPath)
	if err != nil {
		p.want(p.now.Add(operatorsRetry))
		return false
	}
	if secret == nil || secret.Labels[structs.SecretLabelCertificate] != obj.Name {
		return true
	}
	// Only the secret as read is destroyed: one changed since is read again
	// by the next pass.
	read := secret.ModifyIndex
	args := &structs.SecretVersionsRequest{Path: spec.SecretPath, CheckIndex: &read}
	args.Namespace = obj.Namespace
	args.Region = o.srv.Region()
	_, _, err = o.srv.secretVersionsOp(structs.SecretOpDestroy, args)
	if errors.Is(err, structs.ErrSecretConflict) {
		o.secretChanged(p, obj, spec)
		return false
	}
	if err != nil && !errors.Is(err, structs.ErrSecretNotFound) {
		o.logger.Warn("failed to delete the secret of a deleted certificate", "namespace", obj.Namespace,
			"path", spec.SecretPath, "error", err)
		p.want(p.now.Add(operatorsRetry))
		return false
	}
	return true
}

// cleanOrders deletes orders nobody waits for: issuance orders that are not
// the open order of their certificate, and revocation orders that are done
// or too old. A revocation order that is over stays while the status of its
// certificate is not written as the pass left it (certificatePass.settled;
// review finding ATM-24j): the status is the only record of what the order
// came to, and the next pass must find the order to write it.
func (o *certificateOperator) cleanOrders(p *certificatePass, orders []*structs.CustomObject, open map[string]bool) {
	for _, order := range orders {
		spec := &structs.CertificateOrderSpec{}
		status := &structs.CertificateOrderStatus{}
		_ = structs.DecodeObjectPart(order.Spec, spec)
		_ = structs.DecodeObjectPart(order.Status, status)

		if spec.Revoke == "" {
			if !open[order.Name] {
				o.deleteOrder(order)
			}
			continue
		}
		done := status.State == structs.CertificateOrderValid || status.State == structs.CertificateOrderFailed
		expires := time.Unix(0, order.CreateTime).Add(certificateRevokeOrderKeep)
		if p.unsettled[spec.Certificate] && (done || certificateRevokeOrderExpired(order, p.now)) {
			p.want(p.now.Add(operatorsRetry))
			continue
		}
		// The pass has seen the order as it is removed here, and has told
		// on its certificate a failed one and one that was not answered in
		// time (settleRevocation for the order of a user, review finding
		// ATM-24d; revokeWithdrawn for that of a withdrawal or a deletion,
		// review finding ATM-24l).
		switch {
		case done:
			if status.State == structs.CertificateOrderFailed {
				o.logger.Warn("revocation order failed", "order", order.Name, "serial", spec.Revoke, "problem", status.Problem)
			}
			o.deleteOrder(order)
		case certificateRevokeOrderExpired(order, p.now):
			o.logger.Warn("revocation order was not fulfilled in time", "order", order.Name, "serial", spec.Revoke)
			o.deleteOrder(order)
		default:
			p.want(expires)
		}
	}
}
