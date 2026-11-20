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
// if nothing changed or the write failed: the next pass tries again.
func (o *certificateOperator) writeStatus(obj *structs.CustomObject, status map[string]any, finalizers *[]string) *structs.CustomObject {
	normalized, err := structs.NormalizeObjectMap(status)
	if err != nil {
		o.logger.Warn("status of an object can't be encoded", "object", obj.KindRef()+"/"+obj.Name, "error", err)
		return obj
	}
	if structs.ObjectMapsEqual(obj.Status, normalized) && (finalizers == nil || slices.Equal(obj.Finalizers, *finalizers)) {
		return obj
	}
	changed := obj.Copy()
	changed.Status = normalized
	if finalizers != nil {
		changed.Finalizers = slices.Clone(*finalizers)
	}
	written, err := o.srv.writeOperatorObject(changed)
	if err != nil {
		if !structs.IsObjectConflictError(err) {
			o.logger.Warn("failed to write the status of an object", "object", obj.KindRef()+"/"+obj.Name,
				"namespace", obj.Namespace, "error", err)
		}
		return obj
	}
	return written
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
		return ""
	}
	if err := structs.DecodeObjectPart(obj.Status, status); err != nil {
		status = &structs.CertificateStatus{}
	}

	if obj.Deleting() {
		o.finishDeletion(p, obj, spec, status)
		return ""
	}

	secret, err := p.store.SecretByPath(p.ws, obj.Namespace, spec.SecretPath)
	if err != nil {
		o.logger.Warn("secret lookup failed", "namespace", obj.Namespace, "path", spec.SecretPath, "error", err)
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
		}

	default:
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

	// A certificate that deletes its secret needs the operator at its
	// deletion.
	finalizers := slices.Clone(obj.Finalizers)
	hasFinalizer := slices.Contains(finalizers, structs.OperatorsFinalizer)
	switch {
	case spec.DeleteSecret && !hasFinalizer:
		finalizers = append(finalizers, structs.OperatorsFinalizer)
	case !spec.DeleteSecret && hasFinalizer:
		finalizers = slices.DeleteFunc(finalizers, func(f string) bool { return f == structs.OperatorsFinalizer })
	}

	encoded, err := structs.EncodeObjectPart(&st)
	if err != nil {
		o.logger.Warn("status of a certificate can't be encoded", "certificate", obj.Name, "error", err)
		return st.Order
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
	o.writeStatus(obj, encoded, &finalizers)
	return st.Order
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
		revoked, unrevocable = o.revokeWithdrawn(p, obj, st, out)
		if unrevocable != "" {
			st.Revoke = ""
			out.failedMessage += "; " + unrevocable
		}
	}

	removed, err := o.removeSecret(obj, spec, secret, ours)
	if errors.Is(err, structs.ErrSecretConflict) {
		o.secretChanged(p, obj, spec)
		return
	}
	if err != nil {
		o.logger.Warn("failed to take the certificate of a namespace that may no longer ask for it out of its secret",
			"namespace", obj.Namespace, "path", spec.SecretPath, "error", err)
		out.failedMessage += fmt.Sprintf("; taking the certificate out of the secret %q failed: %v", spec.SecretPath, err)
		p.want(p.now.Add(operatorsRetry))
		return
	}
	// The secret holds no certificate of the operator now.
	out.ready = false
	if !revoked || (serial == "" && !removed) {
		return
	}

	// What was held stays known as an earlier certificate, and so do the
	// ones before it: the revocation by serial number finds them there.
	*st = structs.CertificateStatus{Orders: st.Orders, Revoke: st.Revoke, Earlier: o.keepEarlier(p, obj, st)}
	message := fmt.Sprintf("the namespace may no longer ask for this certificate (%s)", why)
	switch {
	case unrevocable != "":
		message += fmt.Sprintf(": the certificate with the serial %s is not revoked, %s", serial, unrevocable)
	case serial != "" && st.Revoke == serial:
		message += fmt.Sprintf(": the certificate with the serial %s is revoked", serial)
	}
	if removed {
		message += fmt.Sprintf("; what the secret %q held for it is destroyed", spec.SecretPath)
	}
	operatorCount(structs.CertificateKind, "withdrawn")
	o.srv.operatorEvent(obj, structs.ObjectEventTypeWarning, "Withdrawn", message)
}

// revokeWithdrawn has the certificate a withdrawal takes back revoked, with
// the reason cessationOfOperation, and reports whether that is over: done,
// or no issuer is left to revoke it (unrevocable says why). The built-in
// issuer lists it in the revocation list of its CA at once. An external
// issuer revokes through a revocation order the withdrawal waits for: one
// that failed is counted (Failures, RetryAt), told in the event
// RevocationFailed, and placed again after a wait that doubles every time;
// cleanOrders removes the failed order after this pass has seen it. A
// revocation order of the serial that the user placed is waited for the
// same way.
func (o *certificateOperator) revokeWithdrawn(p *certificatePass, obj *structs.CustomObject,
	st *structs.CertificateStatus, out *certificateOutcome) (done bool, unrevocable string) {

	serial := st.Serial
	retryAt, _ := time.Parse(time.RFC3339, st.RetryAt)
	waiting := fmt.Sprintf("; waiting for the issuer %q to revoke the certificate with the serial %s", st.Issuer, serial)
	if st.Authority == "" {
		if order := p.orders[certificateRevokeOrderName(obj.Namespace, obj.Name, serial)]; order != nil {
			status := &structs.CertificateOrderStatus{}
			_ = structs.DecodeObjectPart(order.Status, status)
			switch status.State {
			case structs.CertificateOrderValid:
				return true, ""
			case structs.CertificateOrderFailed:
				problem := status.Problem
				if problem == "" {
					problem = "the issuer gave no reason"
				}
				if !p.now.Before(retryAt) {
					// Not counted yet.
					retryAt = o.retryLater(p, st)
					o.srv.operatorEvent(obj, structs.ObjectEventTypeWarning, "RevocationFailed", fmt.Sprintf(
						"the issuer %q did not revoke the certificate with the serial %s: %s; tried again at %s",
						st.Issuer, serial, problem, st.RetryAt))
				}
				out.failedMessage += fmt.Sprintf("; revoking the certificate with the serial %s failed (%s), tried again at %s",
					serial, problem, st.RetryAt)
				p.want(retryAt)
				return false, ""
			default:
				out.failedMessage += waiting
				return false, ""
			}
		}
	}
	if p.now.Before(retryAt) {
		out.failedMessage += fmt.Sprintf("; revoking the certificate with the serial %s is tried again at %s", serial, st.RetryAt)
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
		o.logger.Warn("failed to revoke the certificate of a namespace that may no longer ask for it",
			"namespace", obj.Namespace, "certificate", obj.Name, "serial", serial, "error", err)
		out.failedMessage += fmt.Sprintf("; revoking the certificate with the serial %s failed: %v", serial, err)
		p.want(p.now.Add(operatorsRetry))
		return false, ""
	case st.Authority != "":
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
// keyCompromise (review finding ATM-25b). The list is bounded: beyond
// structs.CertificateEarlierMax the oldest entry goes, and an event names
// its serial, which the status then no longer vouches for. The list of st
// is not changed.
func (o *certificateOperator) keepEarlier(p *certificatePass, obj *structs.CustomObject, st *structs.CertificateStatus) []*structs.CertificateEarlier {
	notAfter, err := time.Parse(time.RFC3339, st.NotAfter)
	if st.Serial == "" || err != nil || !p.now.Before(notAfter) ||
		slices.ContainsFunc(st.Earlier, func(e *structs.CertificateEarlier) bool { return e.Serial == st.Serial }) {
		return st.Earlier
	}
	earlier := append(slices.Clone(st.Earlier), &structs.CertificateEarlier{
		Serial:    st.Serial,
		Issuer:    st.Issuer,
		Authority: st.Authority,
		NotAfter:  st.NotAfter,
		Revoked:   st.Revoke == st.Serial,
	})
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

// finishDeletion does what the deletion of a certificate asks of the
// operator: it drops the open order, deletes the secret if the spec says so,
// and lets the object go.
func (o *certificateOperator) finishDeletion(p *certificatePass, obj *structs.CustomObject, spec *structs.CertificateSpec, status *structs.CertificateStatus) {
	if !slices.Contains(obj.Finalizers, structs.OperatorsFinalizer) {
		return
	}
	if spec.DeleteSecret {
		secret, err := o.srv.State().SecretByPath(nil, obj.Namespace, spec.SecretPath)
		if err != nil {
			return
		}
		if secret != nil && secret.Labels[structs.SecretLabelCertificate] == obj.Name {
			// Only the secret as read is destroyed: one changed since is
			// read again by the next pass.
			read := secret.ModifyIndex
			args := &structs.SecretVersionsRequest{Path: spec.SecretPath, CheckIndex: &read}
			args.Namespace = obj.Namespace
			args.Region = o.srv.Region()
			_, _, err := o.srv.secretVersionsOp(structs.SecretOpDestroy, args)
			if errors.Is(err, structs.ErrSecretConflict) {
				o.secretChanged(p, obj, spec)
				return
			}
			if err != nil && !errors.Is(err, structs.ErrSecretNotFound) {
				o.logger.Warn("failed to delete the secret of a deleted certificate", "namespace", obj.Namespace,
					"path", spec.SecretPath, "error", err)
				return
			}
		}
	}
	changed := obj.Copy()
	changed.Finalizers = slices.DeleteFunc(changed.Finalizers, func(f string) bool { return f == structs.OperatorsFinalizer })
	written, err := o.srv.writeOperatorObject(changed)
	if err != nil {
		if !structs.IsObjectConflictError(err) {
			o.logger.Warn("failed to remove the finalizer of a deleted certificate", "certificate", obj.Name, "error", err)
		}
		return
	}
	o.srv.finalizeObjectIfReady(written)
}

// cleanOrders deletes orders nobody waits for: issuance orders that are not
// the open order of their certificate, and revocation orders that are done
// or too old.
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
		switch {
		case done:
			if status.State == structs.CertificateOrderFailed {
				o.logger.Warn("revocation order failed", "order", order.Name, "serial", spec.Revoke, "problem", status.Problem)
			}
			o.deleteOrder(order)
		case !p.now.Before(expires):
			o.logger.Warn("revocation order was not fulfilled in time", "order", order.Name, "serial", spec.Revoke)
			o.deleteOrder(order)
		default:
			p.want(expires)
		}
	}
}
