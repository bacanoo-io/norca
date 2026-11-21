// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// operator returns the certificate operator the server runs.
func (c *certTest) operator() *certificateOperator {
	c.t.Helper()
	for _, op := range c.s.operators.operators {
		if o, ok := op.(*certificateOperator); ok {
			return o
		}
	}
	c.t.Fatal("the server runs no certificate operator")
	return nil
}

// lostWrites counts the writes the certificate operator lost (loseWrites).
type lostWrites struct {
	mu sync.Mutex
	n  int
}

func (l *lostWrites) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.n
}

// loseWrites makes the certificate operator of the server lose the writes of
// the Certificate objects that lose reports true for, as a write does that
// fails on its way: the pass goes on as after any failed write. stop ends it.
// lose runs under a lock, one call at a time.
func (c *certTest) loseWrites(lose func(obj *structs.CustomObject) bool) (lost *lostWrites, stop func()) {
	c.t.Helper()
	o := c.operator()
	lost = &lostWrites{}
	fault := func(obj *structs.CustomObject) error {
		lost.mu.Lock()
		defer lost.mu.Unlock()
		if obj.Kind != structs.CertificateKind || !lose(obj) {
			return nil
		}
		lost.n++
		return errors.New("the write is lost (test)")
	}
	o.writeFault.Store(&fault)
	return lost, func() { o.writeFault.Store(nil) }
}

// secretSerial returns the serial number of the certificate a secret holds.
func (c *certTest) secretSerial(ns, path string) string {
	c.t.Helper()
	certs, err := parseCertificatesPEM(c.secretItems(ns, path)[structs.CertificateSecretItemCert])
	must.NoError(c.t, err)
	must.Positive(c.t, len(certs))
	return certificateSerial(certs[0])
}

// A write of the status that is lost after an issuance leaves a certificate
// in the secret that the status does not record (review finding ATM-24c).
// The next pass makes it known as an earlier certificate before it issues
// again, so that it can be revoked by its serial number.
func TestCertificate_LostIssuanceWrite(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	_, err := c.apply("", structs.CertificateIssuerKind, "internal", map[string]any{"builtin": map[string]any{}})
	must.NoError(t, err)

	// The write of the status of the first issuance is lost.
	lostSerial := ""
	lost, stop := c.loseWrites(func(obj *structs.CustomObject) bool {
		serial, _ := obj.Status["serial"].(string)
		if serial == "" || lostSerial != "" {
			return false
		}
		lostSerial = serial
		return true
	})
	defer stop()
	_, err = c.apply("default", structs.CertificateKind, "web-tls", map[string]any{
		"issuer": "internal", "dns_names": []any{"web.example.com"}, "secret_path": "certs/web"})
	must.NoError(t, err)
	st := c.waitFor("default", "web-tls", "issued", certReady)
	must.Eq(t, 1, lost.count())
	stop()
	must.NotEq(t, "", lostSerial)
	must.NotEq(t, lostSerial, st.Serial, must.Sprint("the certificate of the lost write is not the one the status holds"))
	must.Eq(t, st.Serial, c.secretSerial("default", "certs/web"))

	active, _ := splitCertificateAuthorities(c.s.Region(), func() []*structs.CertificateAuthority {
		all, err := c.s.State().CertificateAuthorities(nil)
		must.NoError(t, err)
		return all
	}())
	must.NotNil(t, active)
	must.Eq(t, []*structs.CertificateEarlier{{Serial: lostSerial, Issuer: "internal", Authority: active.ID,
		NotAfter: earlierNotAfter(st, lostSerial)}}, st.Earlier,
		must.Sprint("the certificate of the lost write is known as an earlier certificate"))
	messages := c.eventMessages("default", structs.CertificateKind, "web-tls", "CertificateRecovered")
	must.Len(t, 1, messages)
	must.StrContains(t, messages[0], lostSerial)

	// It can be revoked by its serial number.
	resp, err := c.revokeSerial("default", "web-tls", lostSerial, "")
	must.NoError(t, err)
	must.True(t, resp.Earlier)
	must.SliceContains(t, c.crlSerials(), lostSerial)
}

// earlierNotAfter returns the end of an earlier certificate of a status, ""
// if the status does not list the serial.
func earlierNotAfter(st *structs.CertificateStatus, serial string) string {
	if e := earlierCertificate(st, serial); e != nil {
		return e.NotAfter
	}
	return ""
}

// While the status can't be written, the certificate the secret holds stays:
// nothing is issued in its place before it is recorded (review finding
// ATM-24c). The deletion of the object revokes it with the others.
func TestCertificate_LostIssuanceWriteDeleted(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	_, err := c.apply("", structs.CertificateIssuerKind, "internal", map[string]any{"builtin": map[string]any{}})
	must.NoError(t, err)

	// No write of the status gets through until the object is deleted.
	// issued is the serial of the first issuance, later counts the writes
	// lost after it.
	issued, later := "", 0
	lost, stop := c.loseWrites(func(obj *structs.CustomObject) bool {
		if obj.Deleting() {
			return false
		}
		serial, _ := obj.Status["serial"].(string)
		switch {
		case issued != "":
			later++
		case serial != "":
			issued = serial
		}
		return true
	})
	defer stop()
	_, err = c.apply("default", structs.CertificateKind, "web-tls", map[string]any{
		"issuer": "internal", "dns_names": []any{"web.example.com"}, "secret_path": "certs/web"})
	must.NoError(t, err)
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool {
			lost.mu.Lock()
			defer lost.mu.Unlock()
			return later >= 1
		}),
		wait.Timeout(30*time.Second), wait.Gap(50*time.Millisecond),
	), must.Sprint("the pass after the lost write of the issuance"))
	time.Sleep(time.Second)
	lost.mu.Lock()
	first := issued
	lost.mu.Unlock()
	must.Eq(t, first, c.secretSerial("default", "certs/web"),
		must.Sprint("nothing is issued in the place of a certificate that is not recorded yet"))
	_, st := c.certificate("default", "web-tls")
	must.Eq(t, "", st.Serial)

	must.NoError(t, c.delete("default", structs.CertificateKind, "web-tls"))
	c.gone("default", "web-tls", "its certificate is revoked")
	must.Eq(t, []string{first}, c.crlSerials(), must.Sprint("the certificate the status did not record is revoked at the deletion"))
}

// What the secret of a certificate holds counts as a certificate of the
// object only with its private key (review finding ATM-24c): the items of
// the secret are the user's to write, and a certificate is public. One of
// another object that a user puts there is not taken into the earlier
// certificates, and can't be revoked through this object.
func TestCertificate_SecretForeignCertificate(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	_, err := c.apply("", structs.CertificateIssuerKind, "internal", map[string]any{"builtin": map[string]any{}})
	must.NoError(t, err)
	for name, path := range map[string]string{"web-tls": "certs/web", "other": "certs/other"} {
		_, err = c.apply("default", structs.CertificateKind, name, map[string]any{
			"issuer": "internal", "dns_names": []any{name + ".example.com"}, "secret_path": path})
		must.NoError(t, err)
	}
	web := c.waitFor("default", "web-tls", "issued", certReady)
	other := c.waitFor("default", "other", "issued", certReady)

	// The certificate of the other object, with the key of this one.
	var w structs.SecretWriteResponse
	must.NoError(t, c.s.RPC(structs.SecretUpsertRPCMethod, secretWriteReq("certs/web", "", structs.SecretItems{
		structs.CertificateSecretItemCert: c.secretItems("default", "certs/other")[structs.CertificateSecretItemCert],
		structs.CertificateSecretItemKey:  c.secretItems("default", "certs/web")[structs.CertificateSecretItemKey],
	}), &w))
	secret, err := c.s.State().SecretByPath(nil, "default", "certs/web")
	must.NoError(t, err)
	must.Eq(t, "web-tls", secret.Labels[structs.SecretLabelCertificate], must.Sprint("the secret is still the certificate's"))
	must.Eq(t, other.Serial, c.secretSerial("default", "certs/web"))

	time.Sleep(2 * time.Second)
	_, st := c.certificate("default", "web-tls")
	must.Eq(t, web.Serial, st.Serial)
	must.Nil(t, earlierCertificate(st, other.Serial), must.Sprint("a certificate without its key in the secret is not the object's"))
	_, err = c.revokeSerial("default", "web-tls", other.Serial, "")
	must.ErrorContains(t, err, "has no certificate with the serial "+other.Serial)
	must.SliceEmpty(t, c.crlSerials())
}

// A revocation order that is over is removed only when the status of its
// certificate says what it came to (review finding ATM-24j): when the write
// of the status is lost in the pass that sees the order done, the order
// stays, and the next pass marks the certificate revoked and tells it, once.
func TestCertificate_ExternalIssuerRevocationLostWrite(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	a := newAcmeTest(t, c)
	first := a.issue("")
	a.replaced(first)
	resp, err := c.revokeSerial("default", "web-tls", first.Serial, structs.CertificateRevokeReasonSuperseded)
	must.NoError(t, err)
	must.True(t, resp.Pending)

	// The issuer does the order while the status can't be written.
	lost, stop := c.loseWrites(func(*structs.CustomObject) bool { return true })
	defer stop()
	must.NoError(t, a.status("", structs.CertificateOrderKind, certificateRevokeOrderName("default", "web-tls", first.Serial),
		map[string]any{"state": "valid"}))
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return lost.count() >= 1 }),
		wait.Timeout(30*time.Second), wait.Gap(50*time.Millisecond),
	), must.Sprint("the pass that sees the order done"))
	time.Sleep(time.Second)
	must.NotNil(t, a.revocation(first.Serial), must.Sprint("the order stays while the status does not say what it came to"))
	_, st := c.certificate("default", "web-tls")
	must.False(t, earlierCertificate(st, first.Serial).Revoked)
	must.SliceEmpty(t, containing(c.eventMessages("default", structs.CertificateKind, "web-tls", "Revoked"), first.Serial),
		must.Sprint("nothing is told before the status is written"))

	// The status can be written again: the next pass finds the order.
	stop()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com", "poke": "1"})
	st = c.waitFor("default", "web-tls", "the earlier certificate revoked", func(_ *structs.CustomObject, st *structs.CertificateStatus) bool {
		e := earlierCertificate(st, first.Serial)
		return e != nil && e.Revoked
	})
	must.Eq(t, &structs.CertificateEarlier{Serial: first.Serial, Issuer: "acme", NotAfter: first.NotAfter,
		Revoked: true, RevokeReason: structs.CertificateRevokeReasonSuperseded}, earlierCertificate(st, first.Serial))
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return a.revocation(first.Serial) == nil }),
		wait.Timeout(30*time.Second), wait.Gap(50*time.Millisecond),
	), must.Sprint("the order is removed once the status is written"))
	must.Len(t, 1, containing(c.eventMessages("default", structs.CertificateKind, "web-tls", "Revoked"), first.Serial))
}

// ageRevocation makes the revocation order of a serial as old as an order
// may get without an answer of its controller.
func (a *acmeTest) ageRevocation(serial string) {
	a.t.Helper()
	must.Wait(a.t, wait.InitialSuccess(
		wait.ErrorFunc(func() error {
			order := a.revocation(serial)
			if order == nil {
				return errors.New("no revocation order")
			}
			aged := order.Copy()
			aged.CreateTime = time.Now().Add(-certificateRevokeOrderKeep - time.Minute).UnixNano()
			_, err := a.s.writeOperatorObject(aged)
			return err
		}),
		wait.Timeout(10*time.Second), wait.Gap(50*time.Millisecond)))
}

// A revocation order of a withdrawal or of a deletion that its controller
// does not answer in time has failed, like one the issuer fails (review
// finding ATM-24l): it is counted, told by the event RevocationFailed, and
// placed again only after the wait.
func TestCertificate_ExternalIssuerRevocationTimeout(t *testing.T) {
	ci.Parallel(t)

	tests := []struct {
		name  string
		start func(c *certTest)
	}{
		{name: "withdrawal", start: func(c *certTest) {
			c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.org"})
		}},
		{name: "deletion", start: func(c *certTest) {
			must.NoError(c.t, c.delete("default", structs.CertificateKind, "web-tls"))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, cleanup := newCertTest(t)
			defer cleanup()
			c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
			a := newAcmeTest(t, c)
			held := a.issue("")

			tt.start(c)
			a.revocationPlaced(held.Serial)
			a.ageRevocation(held.Serial)
			st := c.waitFor("default", "web-tls", "the revocation that was not answered counted", func(_ *structs.CustomObject, st *structs.CertificateStatus) bool {
				return st.Failures == 1 && st.RetryAt != ""
			})
			must.Eq(t, held.Serial, st.Serial)
			must.False(t, st.Revoked)
			must.Wait(t, wait.InitialSuccess(
				wait.BoolFunc(func() bool {
					return len(c.eventMessages("default", structs.CertificateKind, "web-tls", "RevocationFailed")) == 1
				}),
				wait.Timeout(30*time.Second), wait.Gap(50*time.Millisecond),
			), must.Sprint("an order that is not answered in time is told by an event"))
			messages := c.eventMessages("default", structs.CertificateKind, "web-tls", "RevocationFailed")
			must.StrContains(t, messages[0], held.Serial)
			must.StrContains(t, messages[0], "did not answer within "+certificateRevokeOrderKeep.String())

			// The order is removed, and none is placed before the wait is over.
			must.Wait(t, wait.InitialSuccess(
				wait.BoolFunc(func() bool { return a.revocation(held.Serial) == nil }),
				wait.Timeout(30*time.Second), wait.Gap(50*time.Millisecond),
			), must.Sprint("the order that was not answered is removed"))
			time.Sleep(2 * time.Second)
			must.Nil(t, a.revocation(held.Serial), must.Sprint("no new order before the wait is over"))
			must.Len(t, 1, c.eventMessages("default", structs.CertificateKind, "web-tls", "RevocationFailed"))

			// After the wait a new order is placed.
			c.cutRetryWait("default", "web-tls")
			a.revocationPlaced(held.Serial)
		})
	}
}

// lostAtLeast waits until the certificate operator has lost n writes.
func (c *certTest) lostAtLeast(lost *lostWrites, n int) {
	c.t.Helper()
	must.Wait(c.t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return lost.count() >= n }),
		wait.Timeout(30*time.Second), wait.Gap(50*time.Millisecond),
	), must.Sprintf("the operator lost %d of %d writes", lost.count(), n))
}

// The event of a withdrawal is told when the status that says the same is
// written (review finding ATM-24p): a pass whose write of the status is lost
// tells nothing, and the pass that repeats it tells it once.
func TestCertificate_LostNamesLostWrite(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	_, err := c.apply("", structs.CertificateIssuerKind, "internal", map[string]any{"builtin": map[string]any{}})
	must.NoError(t, err)
	_, err = c.apply("default", structs.CertificateKind, "web-tls", map[string]any{
		"issuer": "internal", "dns_names": []any{"web.example.com"}, "secret_path": "certs/web"})
	must.NoError(t, err)
	held := c.waitFor("default", "web-tls", "issued", certReady)
	events := func() []string {
		t.Helper()
		return c.eventMessages("default", structs.CertificateKind, "web-tls", "Withdrawn")
	}

	// Two passes of the withdrawal, neither of which can write the status.
	lost, stop := c.loseWrites(func(*structs.CustomObject) bool { return true })
	defer stop()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.org"})
	c.lostAtLeast(lost, 1)
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.net"})
	c.lostAtLeast(lost, 2)
	time.Sleep(time.Second)
	_, st := c.certificate("default", "web-tls")
	must.Eq(t, held.Serial, st.Serial, must.Sprint("the status still holds the certificate"))
	must.SliceEmpty(t, events(), must.Sprint("nothing is told before the status is written"))

	// The status can be written again: told once.
	stop()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.org"})
	c.waitFor("default", "web-tls", "withdrawn", withdrawn)
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return len(events()) > 0 }),
		wait.Timeout(30*time.Second), wait.Gap(50*time.Millisecond)))
	time.Sleep(time.Second)
	messages := events()
	must.Len(t, 1, messages)
	must.StrContains(t, messages[0], held.Serial+" is revoked")
	must.SliceContains(t, c.crlSerials(), held.Serial)
}

// The event of what a deletion revoked is told when the status is written
// too (review finding ATM-24p).
func TestCertificate_ExternalIssuerDeleteLostWrite(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	a := newAcmeTest(t, c)
	first := a.issue("")
	second := a.replaced(first)
	events := func() []string {
		t.Helper()
		return c.eventMessages("default", structs.CertificateKind, "web-tls", "RevokedOnDelete")
	}

	must.NoError(t, c.delete("default", structs.CertificateKind, "web-tls"))
	a.revocationPlaced(second.Serial)
	a.revocationPlaced(first.Serial)

	// The issuer revokes the earlier certificate while the status can't be
	// written: two passes see the order done.
	lost, stop := c.loseWrites(func(*structs.CustomObject) bool { return true })
	defer stop()
	must.NoError(t, a.status("", structs.CertificateOrderKind, certificateRevokeOrderName("default", "web-tls", first.Serial),
		map[string]any{"state": "valid"}))
	c.lostAtLeast(lost, 1)
	a.issuer()
	c.lostAtLeast(lost, 2)
	time.Sleep(time.Second)
	must.SliceEmpty(t, events(), must.Sprint("nothing is told before the status is written"))

	// Written again: told once, while the object waits for the other order.
	stop()
	a.issuer()
	c.waitFor("default", "web-tls", "the earlier certificate revoked", func(_ *structs.CustomObject, st *structs.CertificateStatus) bool {
		e := earlierCertificate(st, first.Serial)
		return e != nil && e.Revoked
	})
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return len(events()) > 0 }),
		wait.Timeout(30*time.Second), wait.Gap(50*time.Millisecond)))
	time.Sleep(time.Second)
	messages := events()
	must.Len(t, 1, messages)
	must.StrContains(t, messages[0], first.Serial+" is revoked")
}
