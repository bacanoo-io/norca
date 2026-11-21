// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"slices"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// The deletion of a Certificate object (review finding ATM-24f): its history
// goes with it, so the certificates it still vouches for are revoked before
// it goes, unless the spec says keep_certificates.

// gone waits until a certificate object is removed.
func (c *certTest) gone(ns, name, what string) {
	c.t.Helper()
	must.Wait(c.t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return c.get(ns, structs.CertificateKind, name) == nil }),
		wait.Timeout(30*time.Second), wait.Gap(50*time.Millisecond),
	), must.Sprintf("waiting for the certificate %s/%s to go: %s; object: %v", ns, name, what,
		func() any { return c.get(ns, structs.CertificateKind, name) }()))
}

// deleting reports whether the certificate is marked for deletion and waits
// for the operator.
func deleting(obj *structs.CustomObject) bool {
	return obj.Deleting() && slices.Contains(obj.Finalizers, structs.OperatorsFinalizer)
}

// The deletion of a certificate of the built-in issuer revokes the
// certificate that is held and the earlier ones that are still valid, with
// the reason cessationOfOperation, and then the object goes. The secret stays
// as without the revocation.
func TestCertificate_DeleteRevokes(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	_, err := c.apply("", structs.CertificateIssuerKind, "internal", map[string]any{"builtin": map[string]any{}})
	must.NoError(t, err)
	created, err := c.apply("default", structs.CertificateKind, "web-tls", map[string]any{
		"issuer": "internal", "dns_names": []any{"web.example.com"}, "secret_path": "certs/web"})
	must.NoError(t, err)
	must.SliceContains(t, created.Finalizers, structs.OperatorsFinalizer,
		must.Sprint("the servers have work at the deletion from the first write on"))
	first := c.waitFor("default", "web-tls", "issued", certReady)

	// A second certificate, while the first one is valid.
	c.secretVersions(structs.SecretOpDelete, "default", "certs/web", first.SecretVersion)
	second := c.waitFor("default", "web-tls", "issued again", func(obj *structs.CustomObject, st *structs.CertificateStatus) bool {
		return certReady(obj, st) && st.Serial != first.Serial
	})
	must.NotNil(t, earlierCertificate(second, first.Serial))
	must.Len(t, 0, c.crlSerials())

	must.NoError(t, c.delete("default", structs.CertificateKind, "web-tls"))
	c.gone("default", "web-tls", "its certificates are revoked")

	cessation, _ := structs.CertificateRevokeReasonCode(structs.CertificateRevokeReasonCessationOfOperation)
	must.Eq(t, map[string]int{first.Serial: cessation, second.Serial: cessation}, c.crlReasons(),
		must.Sprint("the valid certificates of a deleted object are revoked"))

	secret, err := c.s.State().SecretByPath(nil, "default", "certs/web")
	must.NoError(t, err)
	must.NotNil(t, secret, must.Sprint("without delete_secret the secret stays"))
}

// With keep_certificates nothing is revoked at the deletion, and the object
// goes at once: the servers have no work for it.
func TestCertificate_DeleteKeepsCertificates(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	_, err := c.apply("", structs.CertificateIssuerKind, "internal", map[string]any{"builtin": map[string]any{}})
	must.NoError(t, err)
	created, err := c.apply("default", structs.CertificateKind, "web-tls", map[string]any{
		"issuer": "internal", "dns_names": []any{"web.example.com"}, "secret_path": "certs/web", "keep_certificates": true})
	must.NoError(t, err)
	must.SliceNotContains(t, created.Finalizers, structs.OperatorsFinalizer)
	c.waitFor("default", "web-tls", "issued", certReady)

	must.NoError(t, c.delete("default", structs.CertificateKind, "web-tls"))
	must.Nil(t, c.get("default", structs.CertificateKind, "web-tls"), must.Sprint("the object goes at once"))
	time.Sleep(time.Second)
	must.Len(t, 0, c.crlSerials(), must.Sprint("nothing is revoked"))

	// With delete_secret too the operator deletes the secret, and revokes
	// nothing.
	_, err = c.apply("default", structs.CertificateKind, "tmp", map[string]any{
		"issuer": "internal", "dns_names": []any{"tmp.example.com"}, "secret_path": "certs/tmp",
		"keep_certificates": true, "delete_secret": true})
	must.NoError(t, err)
	c.waitFor("default", "tmp", "issued", certReady)
	must.NoError(t, c.delete("default", structs.CertificateKind, "tmp"))
	c.gone("default", "tmp", "its secret is deleted")
	secret, err := c.s.State().SecretByPath(nil, "default", "certs/tmp")
	must.NoError(t, err)
	must.Nil(t, secret)
	must.Len(t, 0, c.crlSerials(), must.Sprint("nothing is revoked"))
}

// With an external issuer the object stays until the revocation of every
// valid certificate of it is closed: each has its revocation order, one the
// issuer fails is counted and placed again after its wait, as in a withdrawal
// (review finding ATM-24e), and the status says what the deletion waits for.
func TestCertificate_ExternalIssuerDeleteWaits(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	a := newAcmeTest(t, c)
	first := a.issue("")
	second := a.replaced(first)

	must.NoError(t, c.delete("default", structs.CertificateKind, "web-tls"))
	a.revocationPlaced(second.Serial)
	a.revocationPlaced(first.Serial)
	obj, _ := c.certificate("default", "web-tls")
	must.NotNil(t, obj, must.Sprint("the object stays while its certificates are not revoked"))
	must.True(t, deleting(obj))
	c.waitFor("default", "web-tls", "the condition of the deletion", func(obj *structs.CustomObject, _ *structs.CertificateStatus) bool {
		reason, message := certificateFailure(obj.Status)
		return reason == certificateReasonRevocationPending && len(containing([]string{message}, second.Serial)) == 1
	})

	// The issuer fails the revocation of the earlier certificate: counted,
	// told, and tried again after the wait.
	a.revocationDone(first.Serial, map[string]any{"state": "failed", "problem": "the CA is down"})
	failed := c.waitFor("default", "web-tls", "the failed revocation of the earlier certificate", func(_ *structs.CustomObject, st *structs.CertificateStatus) bool {
		e := earlierCertificate(st, first.Serial)
		return e != nil && e.Failures == 1 && e.RetryAt != ""
	})
	must.False(t, earlierCertificate(failed, first.Serial).Revoked)
	messages := c.eventMessages("default", structs.CertificateKind, "web-tls", "RevocationFailed")
	must.Len(t, 1, messages)
	must.StrContains(t, messages[0], first.Serial)
	must.StrContains(t, messages[0], "the CA is down")

	// The certificate that was held is revoked: the object still waits for
	// the earlier one, and places no order before its wait is over.
	a.revocationDone(second.Serial, map[string]any{"state": "valid"})
	st := c.waitFor("default", "web-tls", "the held certificate revoked", func(_ *structs.CustomObject, st *structs.CertificateStatus) bool {
		return st.Revoked
	})
	must.Eq(t, structs.CertificateRevokeReasonCessationOfOperation, st.RevokeReason)
	time.Sleep(time.Second)
	obj, _ = c.certificate("default", "web-tls")
	must.NotNil(t, obj, must.Sprint("the object waits for the failed revocation"))
	must.Nil(t, a.revocation(first.Serial), must.Sprint("no new order before the wait is over"))
	must.Len(t, 1, containing(c.eventMessages("default", structs.CertificateKind, "web-tls", "RevokedOnDelete"), second.Serial+" is revoked"))

	// The wait is cut short: a new order, and with it done the object goes.
	c.setEarlier("default", "web-tls", func(list []*structs.CertificateEarlier) []*structs.CertificateEarlier {
		out := make([]*structs.CertificateEarlier, len(list))
		for i, e := range list {
			cp := *e
			if cp.Serial == first.Serial {
				cp.RetryAt = time.Now().Add(-time.Second).UTC().Format(time.RFC3339)
			}
			out[i] = &cp
		}
		return out
	})
	a.revocationPlaced(first.Serial)
	must.NoError(t, a.status("", structs.CertificateOrderKind, certificateRevokeOrderName("default", "web-tls", first.Serial),
		map[string]any{"state": "valid"}))
	c.gone("default", "web-tls", "every revocation is done")

	secret, err := c.s.State().SecretByPath(nil, "default", "certs/web")
	must.NoError(t, err)
	must.NotNil(t, secret)
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return certificateOrderCount(t, c.s) == 0 }),
		wait.Timeout(30*time.Second), wait.Gap(50*time.Millisecond),
	), must.Sprint("the revocation orders that are done are removed"))
}

// A certificate nobody is left to revoke does not hold up the deletion: the
// object goes, and an expired one needs no revocation.
func TestCertificate_ExternalIssuerDeleteNotRevocable(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	a := newAcmeTest(t, c)
	first := a.issue("")
	a.replaced(first)

	// The earlier certificate has expired; the issuer of the other is gone.
	c.setEarlier("default", "web-tls", func(list []*structs.CertificateEarlier) []*structs.CertificateEarlier {
		out := make([]*structs.CertificateEarlier, len(list))
		for i, e := range list {
			cp := *e
			cp.NotAfter = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
			out[i] = &cp
		}
		return out
	})
	must.NoError(t, c.delete("", structs.CertificateIssuerKind, "acme"))
	c.waitFor("default", "web-tls", "IssuerNotFound", func(obj *structs.CustomObject, _ *structs.CertificateStatus) bool {
		reason, _ := certificateFailure(obj.Status)
		return reason == "IssuerNotFound"
	})

	must.NoError(t, c.delete("default", structs.CertificateKind, "web-tls"))
	c.gone("default", "web-tls", "nobody can revoke its certificate")
	must.Eq(t, 0, certificateOrderCount(t, c.s))
}

// The spec of an object that waits for a revocation can still be changed to
// keep_certificates: the object then goes without it.
func TestCertificate_ExternalIssuerDeleteGivenUp(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	a := newAcmeTest(t, c)
	held := a.issue("")

	must.NoError(t, c.delete("default", structs.CertificateKind, "web-tls"))
	a.revocationPlaced(held.Serial)
	obj, _ := c.certificate("default", "web-tls")
	must.NotNil(t, obj)
	must.True(t, deleting(obj))

	_, err := c.apply("default", structs.CertificateKind, "web-tls", map[string]any{
		"issuer": "acme", "dns_names": []any{"web.example.com"}, "secret_path": "certs/web", "keep_certificates": true})
	must.NoError(t, err)
	c.gone("default", "web-tls", "the spec says keep_certificates now")
}

// An object created again under the name of a deleted one has no right on
// the certificates of that one (review finding ATM-24f): it can't raise the
// reason of their revocation in the revocation list of the CA, which names
// the namespace and the name only.
func TestCertificate_RecreatedObjectOldSerials(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	_, err := c.apply("", structs.CertificateIssuerKind, "internal", map[string]any{"builtin": map[string]any{}})
	must.NoError(t, err)
	spec := map[string]any{"issuer": "internal", "dns_names": []any{"web.example.com"}, "secret_path": "certs/web"}
	_, err = c.apply("default", structs.CertificateKind, "web-tls", spec)
	must.NoError(t, err)
	old := c.waitFor("default", "web-tls", "issued", certReady)
	must.NoError(t, c.delete("default", structs.CertificateKind, "web-tls"))
	c.gone("default", "web-tls", "its certificate is revoked")
	cessation, _ := structs.CertificateRevokeReasonCode(structs.CertificateRevokeReasonCessationOfOperation)
	must.Eq(t, map[string]int{old.Serial: cessation}, c.crlReasons())

	_, err = c.apply("default", structs.CertificateKind, "web-tls", spec)
	must.NoError(t, err)
	again := c.waitFor("default", "web-tls", "issued for the new object", func(obj *structs.CustomObject, st *structs.CertificateStatus) bool {
		return certReady(obj, st) && st.Serial != old.Serial
	})
	must.Nil(t, earlierCertificate(again, old.Serial))

	_, err = c.revokeSerial("default", "web-tls", old.Serial, structs.CertificateRevokeReasonKeyCompromise)
	must.ErrorContains(t, err, "has no certificate with the serial "+old.Serial)
	must.Eq(t, map[string]int{old.Serial: cessation}, c.crlReasons(),
		must.Sprint("the reason of the certificate of the deleted object stays"))

	// Its own revoked certificate, when the status no longer lists it, is
	// still its own in the revocation list.
	_, err = c.revokeSerial("default", "web-tls", again.Serial, "")
	must.NoError(t, err)
	c.waitFor("default", "web-tls", "issued after the revocation", func(obj *structs.CustomObject, st *structs.CertificateStatus) bool {
		return certReady(obj, st) && st.Serial != again.Serial
	})
	c.setEarlier("default", "web-tls", func([]*structs.CertificateEarlier) []*structs.CertificateEarlier { return nil })
	_, err = c.revokeSerial("default", "web-tls", again.Serial, structs.CertificateRevokeReasonKeyCompromise)
	must.NoError(t, err)
	compromise, _ := structs.CertificateRevokeReasonCode(structs.CertificateRevokeReasonKeyCompromise)
	must.Eq(t, map[string]int{old.Serial: cessation, again.Serial: compromise}, c.crlReasons())
}

// A certificate whose spec the kind refuses can still be deleted (review
// finding ATM-24n): the finalizer of the servers is taken off although the
// object does not pass its kind. With a spec that can't be decoded the
// status still lists its certificates, and they are revoked without the
// spec; the secret is left alone then.
func TestCertificate_DeleteInvalidSpec(t *testing.T) {
	ci.Parallel(t)

	tests := []struct {
		name       string
		spoil      func(spec map[string]any)
		decodes    bool
		secretGone bool
	}{
		{name: "a spec that can't be decoded", spoil: func(spec map[string]any) { spec["dns_names"] = "web.example.com" }},
		{name: "a spec with a field the kind does not know", spoil: func(spec map[string]any) { spec["unknown"] = true },
			decodes: true, secretGone: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, cleanup := newCertTest(t)
			defer cleanup()
			c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
			_, err := c.apply("", structs.CertificateIssuerKind, "internal", map[string]any{"builtin": map[string]any{}})
			must.NoError(t, err)
			_, err = c.apply("default", structs.CertificateKind, "web-tls", map[string]any{
				"issuer": "internal", "dns_names": []any{"web.example.com"}, "secret_path": "certs/web", "delete_secret": true})
			must.NoError(t, err)
			held := c.waitFor("default", "web-tls", "issued", certReady)

			// A spec the schema of the kind refuses, written past it.
			must.Wait(t, wait.InitialSuccess(
				wait.ErrorFunc(func() error {
					broken := c.get("default", structs.CertificateKind, "web-tls").Copy()
					tt.spoil(broken.Spec)
					_, _, err := c.s.raftApply(structs.CustomObjectUpsertRequestType|structs.IgnoreUnknownTypeFlag, &structs.CustomObjectUpsertStateRequest{
						Object:              broken,
						ExpectedModifyIndex: broken.ModifyIndex,
						WriteRequest:        structs.WriteRequest{Region: c.s.Region()},
					})
					return err
				}),
				wait.Timeout(10*time.Second), wait.Gap(50*time.Millisecond)))
			obj := c.get("default", structs.CertificateKind, "web-tls")
			must.Eq(t, tt.decodes, structs.DecodeObjectPart(obj.Spec, &structs.CertificateSpec{}) == nil)
			must.SliceContains(t, obj.Finalizers, structs.OperatorsFinalizer)
			_, err = c.s.writeOperatorObject(obj.Copy())
			must.ErrorContains(t, err, "is invalid", must.Sprint("the object does not pass its kind"))

			must.NoError(t, c.delete("default", structs.CertificateKind, "web-tls"))
			c.gone("default", "web-tls", "its spec does not hold it")
			must.Eq(t, []string{held.Serial}, c.crlSerials(), must.Sprint("the certificate the status lists is revoked"))
			secret, err := c.s.State().SecretByPath(nil, "default", "certs/web")
			must.NoError(t, err)
			must.Eq(t, tt.secretGone, secret == nil,
				must.Sprint("delete_secret of a spec that can be read is done; nothing is deleted on the word of one that can't"))
		})
	}
}

// A revocation of a certificate of an object that is being deleted is made
// like any other, and its answer says that the object is being deleted
// (review finding ATM-24m): nothing is issued in the place of what is
// revoked. The reason of the revocation order the deletion waits for can be
// raised that way.
func TestCertificate_ExternalIssuerRevokeDeleting(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	a := newAcmeTest(t, c)
	first := a.issue("")
	second := a.replaced(first)

	resp, err := c.revokeSerial("default", "web-tls", first.Serial, "")
	must.NoError(t, err)
	must.True(t, resp.Earlier)
	must.True(t, resp.Pending)
	must.False(t, resp.Deleting)
	a.revocationDone(first.Serial, map[string]any{"state": "failed", "problem": "the CA is down"})
	c.waitFor("default", "web-tls", "the end of the pending revocation", func(_ *structs.CustomObject, st *structs.CertificateStatus) bool {
		e := earlierCertificate(st, first.Serial)
		return e != nil && e.Revoking == ""
	})

	must.NoError(t, c.delete("default", structs.CertificateKind, "web-tls"))
	a.revocationPlaced(second.Serial)
	a.revocationPlaced(first.Serial)

	// The current certificate, with a stronger reason than the deletion's.
	resp, err = c.revokeSerial("default", "web-tls", "", structs.CertificateRevokeReasonKeyCompromise)
	must.NoError(t, err)
	must.Eq(t, second.Serial, resp.Serial)
	must.False(t, resp.Earlier)
	must.True(t, resp.Pending)
	must.True(t, resp.Deleting, must.Sprint("the answer says that the object is being deleted"))
	spec := &structs.CertificateOrderSpec{}
	must.NoError(t, structs.DecodeObjectPart(a.revocation(second.Serial).Spec, spec))
	must.Eq(t, structs.CertificateRevokeReasonKeyCompromise, spec.Reason)

	// An earlier one.
	resp, err = c.revokeSerial("default", "web-tls", first.Serial, "")
	must.NoError(t, err)
	must.True(t, resp.Earlier)
	must.True(t, resp.Pending)
	must.True(t, resp.Deleting)

	// The deletion goes on: nothing is issued, and the object goes when its
	// certificates are revoked.
	_, st := c.certificate("default", "web-tls")
	must.Eq(t, "", st.Order)
	for _, serial := range []string{first.Serial, second.Serial} {
		must.NoError(t, a.status("", structs.CertificateOrderKind, certificateRevokeOrderName("default", "web-tls", serial),
			map[string]any{"state": "valid"}))
	}
	c.gone("default", "web-tls", "every revocation is done")
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return certificateOrderCount(t, c.s) == 0 }),
		wait.Timeout(30*time.Second), wait.Gap(50*time.Millisecond),
	), must.Sprint("no order is left, and none was placed for a new certificate"))
}
