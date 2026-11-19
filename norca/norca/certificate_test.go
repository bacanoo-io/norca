// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"slices"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// certTest drives the object API and the certificate API of a test server.
type certTest struct {
	t     *testing.T
	s     *Server
	token string
}

func (c *certTest) write(ns string) structs.WriteRequest {
	return structs.WriteRequest{Region: "global", Namespace: ns, AuthToken: c.token}
}

func (c *certTest) query(ns string) structs.QueryOptions {
	return structs.QueryOptions{Region: "global", Namespace: ns, AuthToken: c.token}
}

// allow sets what the namespace may ask certificates for.
func (c *certTest) allow(ns string, meta map[string]string) {
	c.t.Helper()
	var resp structs.GenericResponse
	must.NoError(c.t, c.s.RPC("Namespace.UpsertNamespaces", &structs.NamespaceUpsertRequest{
		Namespaces:   []*structs.Namespace{{Name: ns, Meta: meta}},
		WriteRequest: c.write(""),
	}, &resp))
}

func (c *certTest) apply(ns, kind, name string, spec map[string]any) (*structs.CustomObject, error) {
	var resp structs.CustomObjectWriteResponse
	err := c.s.RPC("CustomObject.Apply", &structs.CustomObjectApplyRequest{
		Object:       &structs.CustomObject{Group: structs.OperatorsGroup, Kind: kind, Name: name, Spec: spec},
		WriteRequest: c.write(ns),
	}, &resp)
	return resp.Object, err
}

func (c *certTest) status(ns, kind, name string, status map[string]any) error {
	var resp structs.CustomObjectWriteResponse
	return c.s.RPC("CustomObject.UpdateStatus", &structs.CustomObjectStatusRequest{
		Group: structs.OperatorsGroup, Kind: kind, Name: name, Status: status,
		WriteRequest: c.write(ns),
	}, &resp)
}

func (c *certTest) delete(ns, kind, name string) error {
	var resp structs.CustomObjectWriteResponse
	return c.s.RPC("CustomObject.Delete", &structs.CustomObjectDeleteRequest{
		Group: structs.OperatorsGroup, Kind: kind, Name: name,
		WriteRequest: c.write(ns),
	}, &resp)
}

func (c *certTest) get(ns, kind, name string) *structs.CustomObject {
	c.t.Helper()
	obj, err := c.s.State().CustomObjectByName(nil, structs.OperatorsGroup, kind, ns, name)
	must.NoError(c.t, err)
	return obj
}

// certificate returns the status of a certificate.
func (c *certTest) certificate(ns, name string) (*structs.CustomObject, *structs.CertificateStatus) {
	c.t.Helper()
	obj := c.get(ns, structs.CertificateKind, name)
	st := &structs.CertificateStatus{}
	if obj != nil {
		must.NoError(c.t, structs.DecodeObjectPart(obj.Status, st))
	}
	return obj, st
}

// waitFor waits until the certificate satisfies the condition and returns
// its status.
func (c *certTest) waitFor(ns, name, what string, cond func(*structs.CustomObject, *structs.CertificateStatus) bool) *structs.CertificateStatus {
	c.t.Helper()
	var st *structs.CertificateStatus
	must.Wait(c.t, wait.InitialSuccess(
		wait.BoolFunc(func() bool {
			var obj *structs.CustomObject
			obj, st = c.certificate(ns, name)
			return obj != nil && cond(obj, st)
		}),
		wait.Timeout(30*time.Second),
		wait.Gap(50*time.Millisecond),
	), must.Sprintf("waiting for the certificate %s/%s: %s; status: %v", ns, name, what,
		func() any { obj, _ := c.certificate(ns, name); return obj }()))
	return st
}

// secretItems returns the current items of a secret.
func (c *certTest) secretItems(ns, path string) structs.SecretItems {
	c.t.Helper()
	items, _, err := c.s.secretReadValue(ns, path, 0)
	must.NoError(c.t, err)
	return items
}

// roots returns the trust bundle of the issuing CA.
func (c *certTest) roots() *structs.CertificateRootsResponse {
	c.t.Helper()
	var resp structs.CertificateRootsResponse
	must.NoError(c.t, c.s.RPC("Certificate.Roots", &structs.CertificateRootsRequest{QueryOptions: c.query("")}, &resp))
	return &resp
}

func certPool(t *testing.T, bundle string) *x509.CertPool {
	t.Helper()
	certs, err := parseCertificatesPEM(bundle)
	must.NoError(t, err)
	must.Positive(t, len(certs))
	pool := x509.NewCertPool()
	for _, cert := range certs {
		pool.AddCert(cert)
	}
	return pool
}

func newCertTest(t *testing.T) (*certTest, func()) {
	t.Helper()
	s, _, cleanup := secretTestServer(t, false, nil)
	waitOperatorKinds(t, s)
	return &certTest{t: t, s: s}, cleanup
}

func certReady(obj *structs.CustomObject, st *structs.CertificateStatus) bool {
	return st.Serial != "" && objectConditionTrue(obj.Status, structs.CertificateConditionReady)
}

func TestCertificate_BuiltinIssuer(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{
		structs.NamespaceMetaCertificateDNSSuffixes: "example.com",
		structs.NamespaceMetaCertificateIPAddresses: "10.0.0.0/24",
	})

	// No issuing CA before a built-in issuer asks for one.
	must.Eq(t, "", c.roots().ActiveID)

	_, err := c.apply("", structs.CertificateIssuerKind, "internal", map[string]any{"builtin": map[string]any{}})
	must.NoError(t, err)
	_, err = c.apply("default", structs.CertificateKind, "web-tls", map[string]any{
		"issuer":       "internal",
		"dns_names":    []any{"web.example.com", "*.web.example.com"},
		"ip_addresses": []any{"10.0.0.5"},
		"secret_path":  "certs/web",
	})
	must.NoError(t, err)

	first := c.waitFor("default", "web-tls", "issued", certReady)
	roots := c.roots()
	must.Eq(t, roots.ActiveID, first.Authority)
	must.Eq(t, "internal", first.Issuer)

	// The secret holds the certificate, its key and the trust bundle, and
	// expires with the certificate.
	items := c.secretItems("default", "certs/web")
	must.Eq(t, []string{"ca", "cert", "key"}, items.Keys())
	must.Eq(t, roots.Roots, items[structs.CertificateSecretItemCA])
	leaf, err := parseCertificatesPEM(items[structs.CertificateSecretItemCert])
	must.NoError(t, err)
	must.Len(t, 1, leaf)
	must.Eq(t, first.Serial, certificateSerial(leaf[0]))
	must.Eq(t, []string{"web.example.com", "*.web.example.com"}, leaf[0].DNSNames)
	must.Eq(t, "10.0.0.5", leaf[0].IPAddresses[0].String())
	must.False(t, leaf[0].IsCA)
	_, err = leaf[0].Verify(x509.VerifyOptions{Roots: certPool(t, roots.Roots), DNSName: "a.web.example.com"})
	must.NoError(t, err)
	key, err := parsePrivateKeyPEM(items[structs.CertificateSecretItemKey])
	must.NoError(t, err)
	must.True(t, publicKeysEqual(key.Public(), leaf[0].PublicKey))

	secret, err := c.s.State().SecretByPath(nil, "default", "certs/web")
	must.NoError(t, err)
	must.Eq(t, "web-tls", secret.Labels[structs.SecretLabelCertificate])
	must.NotNil(t, secret.ExpiresAt)
	must.True(t, secret.ExpiresAt.Equal(leaf[0].NotAfter))
	must.Eq(t, first.SecretVersion, secret.CurrentVersion)

	// The renewal is at two thirds of the validity.
	renewAt, err := time.Parse(time.RFC3339, first.RenewAt)
	must.NoError(t, err)
	must.Eq(t, leaf[0].NotBefore.Add(leaf[0].NotAfter.Sub(leaf[0].NotBefore)*2/3).Unix(), renewAt.Unix())

	// The issuer shows the CA that signs for it.
	issuer := c.get("", structs.CertificateIssuerKind, "internal")
	must.Eq[any](t, roots.ActiveID, issuer.Status["authority"])
	must.True(t, objectConditionTrue(issuer.Status, structs.CertificateConditionReady))

	// Other names: a new certificate with a new key.
	_, err = c.apply("default", structs.CertificateKind, "web-tls", map[string]any{
		"issuer":      "internal",
		"dns_names":   []any{"api.example.com"},
		"secret_path": "certs/web",
		"key_type":    "rsa-2048",
	})
	must.NoError(t, err)
	second := c.waitFor("default", "web-tls", "issued for the new names", func(obj *structs.CustomObject, st *structs.CertificateStatus) bool {
		return certReady(obj, st) && st.Serial != first.Serial
	})
	changed := c.secretItems("default", "certs/web")
	must.NotEq(t, items[structs.CertificateSecretItemKey], changed[structs.CertificateSecretItemKey])
	leaf, err = parseCertificatesPEM(changed[structs.CertificateSecretItemCert])
	must.NoError(t, err)
	must.Eq(t, []string{"api.example.com"}, leaf[0].DNSNames)

	// A revocation lists the certificate and brings a new one.
	var revoked structs.CertificateRevokeResponse
	must.NoError(t, c.s.RPC("Certificate.Revoke", &structs.CertificateRevokeRequest{Name: "web-tls", WriteRequest: c.write("default")}, &revoked))
	must.Eq(t, second.Serial, revoked.Serial)
	third := c.waitFor("default", "web-tls", "issued after the revocation", func(obj *structs.CustomObject, st *structs.CertificateStatus) bool {
		return certReady(obj, st) && st.Serial != second.Serial && st.Revoke == ""
	})
	must.NotEq(t, second.Serial, third.Serial)

	var crlResp structs.CertificateCRLResponse
	must.NoError(t, c.s.RPC("Certificate.CRL", &structs.CertificateCRLRequest{QueryOptions: c.query("")}, &crlResp))
	block, _ := pem.Decode([]byte(crlResp.CRL))
	must.NotNil(t, block)
	crl, err := x509.ParseRevocationList(block.Bytes)
	must.NoError(t, err)
	caCerts, err := parseCertificatesPEM(roots.Roots)
	must.NoError(t, err)
	must.NoError(t, crl.CheckSignatureFrom(caCerts[0]))
	must.Len(t, 1, crl.RevokedCertificateEntries)
	must.Eq(t, second.Serial, certificateSerial(&x509.Certificate{SerialNumber: crl.RevokedCertificateEntries[0].SerialNumber}))

	// What the operator did is in the events of the object.
	var events structs.CustomObjectEventsResponse
	must.NoError(t, c.s.RPC("CustomObject.ListEvents", &structs.CustomObjectSpecificRequest{
		Group: structs.OperatorsGroup, Kind: structs.CertificateKind, Name: "web-tls", QueryOptions: c.query("default"),
	}, &events))
	var reasons []string
	for _, ev := range events.Events {
		reasons = append(reasons, ev.Reason)
	}
	must.SliceContains(t, reasons, "Issued")
	must.SliceContains(t, reasons, "Renewed")
	must.SliceContains(t, reasons, "Revoked")

	// Without delete_secret the secret stays when the certificate goes.
	must.NoError(t, c.delete("default", structs.CertificateKind, "web-tls"))
	must.Nil(t, c.get("default", structs.CertificateKind, "web-tls"))
	secret, err = c.s.State().SecretByPath(nil, "default", "certs/web")
	must.NoError(t, err)
	must.NotNil(t, secret)
}

func TestCertificate_DeleteSecret(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})

	_, err := c.apply("", structs.CertificateIssuerKind, "internal", map[string]any{"builtin": map[string]any{}})
	must.NoError(t, err)
	_, err = c.apply("default", structs.CertificateKind, "tmp", map[string]any{
		"issuer": "internal", "dns_names": []any{"tmp.example.com"}, "secret_path": "certs/tmp", "delete_secret": true,
	})
	must.NoError(t, err)
	c.waitFor("default", "tmp", "issued, with the finalizer", func(obj *structs.CustomObject, st *structs.CertificateStatus) bool {
		return certReady(obj, st) && slices.Contains(obj.Finalizers, structs.OperatorsFinalizer)
	})

	must.NoError(t, c.delete("default", structs.CertificateKind, "tmp"))
	waitForSecretStore(t, "the certificate and its secret to go", func() bool {
		secret, err := c.s.State().SecretByPath(nil, "default", "certs/tmp")
		return err == nil && secret == nil && c.get("default", structs.CertificateKind, "tmp") == nil
	})
}

// The operator writes the secret of a certificate only in the state it read
// (review finding ATM-3): a secret somebody else created or changed while
// the key was generated and signed is not overwritten.
func TestCertificate_SecretWriteIsCheckedAndSet(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	o := newCertificateOperator(c.s, c.s.logger)
	obj := &structs.CustomObject{Group: structs.OperatorsGroup, Kind: structs.CertificateKind, Namespace: "default", Name: "web-tls"}
	items := structs.SecretItems{structs.CertificateSecretItemCert: "issued", structs.CertificateSecretItemKey: "key"}
	read := func(path string) *structs.Secret {
		t.Helper()
		secret, err := c.s.State().SecretByPath(nil, "default", path)
		must.NoError(t, err)
		return secret
	}
	var w structs.SecretWriteResponse

	// Absent when the pass read it, created by somebody else since.
	must.NoError(t, c.s.RPC(structs.SecretUpsertRPCMethod, secretWriteReq("certs/new", "", structs.SecretItems{"cert": "theirs"}), &w))
	_, err := o.writeSecret(obj, &structs.CertificateSpec{SecretPath: "certs/new"}, nil, items, nil)
	must.ErrorIs(t, err, structs.ErrSecretConflict)
	must.Eq(t, "theirs", c.secretItems("default", "certs/new")["cert"])
	must.Eq(t, "", read("certs/new").Labels[structs.SecretLabelCertificate])

	// The certificate's own secret, changed by somebody else after the pass
	// read it.
	spec := &structs.CertificateSpec{SecretPath: "certs/web"}
	_, err = o.writeSecret(obj, spec, nil, items, nil)
	must.NoError(t, err)
	stale := read("certs/web")
	must.Eq(t, "web-tls", stale.Labels[structs.SecretLabelCertificate])
	must.NoError(t, c.s.RPC(structs.SecretUpsertRPCMethod, secretWriteReq("certs/web", "", structs.SecretItems{"cert": "rotated"}), &w))
	_, err = o.writeSecret(obj, spec, stale, items, nil)
	must.ErrorIs(t, err, structs.ErrSecretConflict)
	must.Eq(t, "rotated", c.secretItems("default", "certs/web")["cert"])

	// Read again, it is written.
	_, err = o.writeSecret(obj, spec, read("certs/web"), items, nil)
	must.NoError(t, err)
	must.Eq(t, "issued", c.secretItems("default", "certs/web")[structs.CertificateSecretItemCert])
}

func TestCertificate_WriteChecks(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "team-a.example.com"})
	_, err := c.apply("", structs.CertificateIssuerKind, "internal", map[string]any{"builtin": map[string]any{}})
	must.NoError(t, err)
	_, err = c.apply("", structs.CertificateIssuerKind, "acme", map[string]any{"external": map[string]any{"controller": "test"}})
	must.NoError(t, err)

	spec := func(change func(map[string]any)) map[string]any {
		m := map[string]any{"issuer": "internal", "dns_names": []any{"web.team-a.example.com"}, "secret_path": "certs/web"}
		change(m)
		return m
	}
	tests := []struct {
		name     string
		spec     map[string]any
		code     int
		expected string
	}{
		{
			name:     "a name of another namespace",
			spec:     spec(func(m map[string]any) { m["dns_names"] = []any{"web.team-b.example.com"} }),
			code:     403,
			expected: "may not ask for the DNS name",
		},
		{
			name:     "an ip address that is not listed",
			spec:     spec(func(m map[string]any) { m["ip_addresses"] = []any{"10.0.0.5"} }),
			code:     403,
			expected: "may not ask for the IP address",
		},
		{
			name:     "an ip address with an external issuer",
			spec:     spec(func(m map[string]any) { m["issuer"], m["ip_addresses"] = "acme", []any{"10.0.0.5"} }),
			code:     403,
			expected: "built-in issuer only",
		},
		{
			name:     "an invalid name",
			spec:     spec(func(m map[string]any) { m["dns_names"] = []any{"Web.team-a.example.com"} }),
			code:     400,
			expected: "invalid DNS name",
		},
		{
			name:     "a validity over the maximum",
			spec:     spec(func(m map[string]any) { m["validity"] = "20000h" }),
			code:     400,
			expected: "longer than the maximum",
		},
		{
			name:     "a field the kind does not know",
			spec:     spec(func(m map[string]any) { m["common_name"] = "web" }),
			code:     400,
			expected: "common_name",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.apply("default", structs.CertificateKind, "web-tls", tt.spec)
			must.Eq(t, tt.code, errCode(t, err))
			must.ErrorContains(t, err, tt.expected)
		})
	}

	// An issuer is one of the two.
	_, err = c.apply("", structs.CertificateIssuerKind, "both", map[string]any{
		"builtin": map[string]any{}, "external": map[string]any{"controller": "x"}})
	must.ErrorContains(t, err, "either builtin or external")

	// Orders are the servers'; the status of a certificate and of a
	// built-in issuer too.
	_, err = c.apply("", structs.CertificateOrderKind, "default.web-tls.1", map[string]any{"issuer": "acme", "certificate": "default/web-tls"})
	must.Eq(t, 403, errCode(t, err))
	_, err = c.apply("default", structs.CertificateKind, "web-tls", spec(func(map[string]any) {}))
	must.NoError(t, err)
	must.Eq(t, 403, errCode(t, c.status("default", structs.CertificateKind, "web-tls", map[string]any{"serial": "01"})))
	must.Eq(t, 403, errCode(t, c.status("", structs.CertificateIssuerKind, "internal", map[string]any{"authority": "x"})))
	must.NoError(t, c.status("", structs.CertificateIssuerKind, "acme", map[string]any{"heartbeat": time.Now().UTC().Format(time.RFC3339)}))

	// The secret path of a certificate is fixed.
	_, err = c.apply("default", structs.CertificateKind, "web-tls", spec(func(m map[string]any) { m["secret_path"] = "certs/other" }))
	must.ErrorContains(t, err, "secret_path can't be changed")

	// The kinds are built in.
	var resp structs.GenericResponse
	err = c.s.RPC("ObjectKind.DeleteObjectKind", &structs.ObjectKindDeleteRequest{
		Group: structs.OperatorsGroup, Kind: structs.CertificateKind, WriteRequest: c.write(""),
	}, &resp)
	must.ErrorContains(t, err, "built in")
}

func TestCertificate_Refusals(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})

	failed := func(reason string) func(*structs.CustomObject, *structs.CertificateStatus) bool {
		return func(obj *structs.CustomObject, _ *structs.CertificateStatus) bool {
			got, _ := certificateFailure(obj.Status)
			return got == reason
		}
	}

	// The issuer does not exist yet: the certificate waits for it.
	_, err := c.apply("default", structs.CertificateKind, "web-tls", map[string]any{
		"issuer": "internal", "dns_names": []any{"web.example.com"}, "secret_path": "certs/web"})
	must.NoError(t, err)
	c.waitFor("default", "web-tls", "IssuerNotFound", failed("IssuerNotFound"))
	_, err = c.apply("", structs.CertificateIssuerKind, "internal", map[string]any{"builtin": map[string]any{}})
	must.NoError(t, err)
	issued := c.waitFor("default", "web-tls", "issued", certReady)

	// The namespace loses the name: nothing new is issued, and the
	// certificate it has is taken back (TestCertificate_LostNames).
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.org"})
	withdrawn := c.waitFor("default", "web-tls", "NamesNotAllowed", func(obj *structs.CustomObject, st *structs.CertificateStatus) bool {
		return failed("NamesNotAllowed")(obj, st) && st.Serial == ""
	})
	must.Eq(t, issued.Serial, withdrawn.Revoke)
	obj, _ := c.certificate("default", "web-tls")
	must.False(t, objectConditionTrue(obj.Status, structs.CertificateConditionReady))
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	again := c.waitFor("default", "web-tls", "allowed again", func(obj *structs.CustomObject, st *structs.CertificateStatus) bool {
		return failed("")(obj, st) && certReady(obj, st)
	})
	must.NotEq(t, issued.Serial, again.Serial)

	// A secret that is someone else's is not overwritten.
	var w structs.SecretWriteResponse
	must.NoError(t, c.s.RPC(structs.SecretUpsertRPCMethod, secretWriteReq("certs/taken", "", structs.SecretItems{"cert": "mine"}), &w))
	_, err = c.apply("default", structs.CertificateKind, "taken", map[string]any{
		"issuer": "internal", "dns_names": []any{"taken.example.com"}, "secret_path": "certs/taken"})
	must.NoError(t, err)
	c.waitFor("default", "taken", "SecretConflict", failed("SecretConflict"))
	must.Eq(t, "mine", c.secretItems("default", "certs/taken")["cert"])
}

// eventReasons returns the reasons of the events of an object.
func (c *certTest) eventReasons(ns, kind, name string) []string {
	c.t.Helper()
	var events structs.CustomObjectEventsResponse
	must.NoError(c.t, c.s.RPC("CustomObject.ListEvents", &structs.CustomObjectSpecificRequest{
		Group: structs.OperatorsGroup, Kind: kind, Name: name, QueryOptions: c.query(ns),
	}, &events))
	var reasons []string
	for _, ev := range events.Events {
		reasons = append(reasons, ev.Reason)
	}
	return reasons
}

// eventMessages returns the messages of the events of an object with a
// reason.
func (c *certTest) eventMessages(ns, kind, name, reason string) []string {
	c.t.Helper()
	var events structs.CustomObjectEventsResponse
	must.NoError(c.t, c.s.RPC("CustomObject.ListEvents", &structs.CustomObjectSpecificRequest{
		Group: structs.OperatorsGroup, Kind: kind, Name: name, QueryOptions: c.query(ns),
	}, &events))
	var messages []string
	for _, ev := range events.Events {
		if ev.Reason == reason {
			messages = append(messages, ev.Message)
		}
	}
	return messages
}

// crlSerials returns the serial numbers in the revocation list of the active
// issuing CA.
func (c *certTest) crlSerials() []string {
	c.t.Helper()
	var resp structs.CertificateCRLResponse
	must.NoError(c.t, c.s.RPC("Certificate.CRL", &structs.CertificateCRLRequest{QueryOptions: c.query("")}, &resp))
	block, _ := pem.Decode([]byte(resp.CRL))
	must.NotNil(c.t, block)
	crl, err := x509.ParseRevocationList(block.Bytes)
	must.NoError(c.t, err)
	var serials []string
	for _, entry := range crl.RevokedCertificateEntries {
		serials = append(serials, certificateSerial(&x509.Certificate{SerialNumber: entry.SerialNumber}))
	}
	return serials
}

// withdrawn reports whether the operator took back the certificate of a
// namespace that lost its names.
func withdrawn(obj *structs.CustomObject, st *structs.CertificateStatus) bool {
	reason, _ := certificateFailure(obj.Status)
	return reason == "NamesNotAllowed" && st.Serial == ""
}

// A namespace that loses the right to the names of a certificate (its
// certificate_dns_suffixes, certificate_ip_addresses or certificate_issuers
// change) does not keep the key and a valid certificate for names that may
// now be another namespace's (review finding ATM-21): the certificate is
// revoked and taken out of the secret, and the status no longer says it is
// held.
func TestCertificate_LostNames(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	_, err := c.apply("", structs.CertificateIssuerKind, "internal", map[string]any{"builtin": map[string]any{}})
	must.NoError(t, err)

	readSecret := func(path string) *structs.Secret {
		t.Helper()
		secret, err := c.s.State().SecretByPath(nil, "default", path)
		must.NoError(t, err)
		return secret
	}

	// Without delete_secret the secret stays, with every version that held
	// the certificate destroyed; with it the secret goes.
	_, err = c.apply("default", structs.CertificateKind, "kept", map[string]any{
		"issuer": "internal", "dns_names": []any{"kept.example.com"}, "secret_path": "certs/kept"})
	must.NoError(t, err)
	_, err = c.apply("default", structs.CertificateKind, "gone", map[string]any{
		"issuer": "internal", "dns_names": []any{"gone.example.com"}, "secret_path": "certs/gone", "delete_secret": true})
	must.NoError(t, err)
	kept := c.waitFor("default", "kept", "issued", certReady)
	gone := c.waitFor("default", "gone", "issued", certReady)

	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.org"})
	for _, tc := range []struct {
		name, path string
		issued     *structs.CertificateStatus
	}{
		{"kept", "certs/kept", kept},
		{"gone", "certs/gone", gone},
	} {
		st := c.waitFor("default", tc.name, "withdrawn", withdrawn)
		must.Eq(t, structs.CertificateStatus{Orders: st.Orders, Revoke: tc.issued.Serial}, *st,
			must.Sprint("nothing in the status says a certificate is held"))
		obj, _ := c.certificate("default", tc.name)
		must.False(t, objectConditionTrue(obj.Status, structs.CertificateConditionReady))
		must.SliceContains(t, c.eventReasons("default", structs.CertificateKind, tc.name), "Withdrawn")
		must.SliceContains(t, c.crlSerials(), tc.issued.Serial)
		_, _, err := c.s.secretReadValue("default", tc.path, tc.issued.SecretVersion)
		must.Error(t, err, must.Sprint("the version with the certificate can't be read"))
	}
	secret := readSecret("certs/kept")
	must.NotNil(t, secret)
	must.Eq(t, "kept", secret.Labels[structs.SecretLabelCertificate])
	for _, v := range secret.Versions {
		must.True(t, v.Destroyed, must.Sprintf("version %d is destroyed", v.Version))
	}
	must.Nil(t, readSecret("certs/gone"))

	// Allowed again, a new certificate is issued into the same secret.
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	again := c.waitFor("default", "kept", "issued again", certReady)
	must.NotEq(t, kept.Serial, again.Serial)
	must.Eq(t, []string{"ca", "cert", "key"}, c.secretItems("default", "certs/kept").Keys())
	c.waitFor("default", "gone", "issued again", certReady)

	// A namespace that may no longer use the issuer loses its certificates
	// too.
	c.allow("default", map[string]string{
		structs.NamespaceMetaCertificateDNSSuffixes: "example.com",
		structs.NamespaceMetaCertificateIssuers:     "other",
	})
	st := c.waitFor("default", "kept", "withdrawn for the issuer", withdrawn)
	must.Eq(t, again.Serial, st.Revoke)
}

// A certificate is taken back only when the namespace lost the names, not
// when the issuer it names can't sign them: an IP address certificate of the
// built-in issuer whose issuer was recreated as an external one stays.
func TestCertificate_LostNamesRefusal(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{
		structs.NamespaceMetaCertificateDNSSuffixes: "example.com",
		structs.NamespaceMetaCertificateIPAddresses: "10.0.0.0/24",
	})
	o := newCertificateOperator(c.s, c.s.logger)
	p := &certificatePass{now: time.Now(), store: c.s.State()}
	obj := &structs.CustomObject{Group: structs.OperatorsGroup, Kind: structs.CertificateKind, Namespace: "default", Name: "web-tls"}
	issuerObj := &structs.CustomObject{Group: structs.OperatorsGroup, Kind: structs.CertificateIssuerKind, Name: "issuer"}
	external := &structs.CertificateIssuerSpec{External: &structs.CertificateIssuerExternal{Controller: "test"}}
	builtin := &structs.CertificateIssuerSpec{Builtin: &structs.CertificateIssuerBuiltin{}}

	tests := []struct {
		name   string
		spec   *structs.CertificateSpec
		issuer *structs.CertificateIssuerSpec // nil: the issuer is gone
		reason string
		lost   bool
	}{
		{
			name:   "an IP address with an external issuer",
			spec:   &structs.CertificateSpec{Issuer: "issuer", IPAddresses: []string{"10.0.0.5"}, SecretPath: "certs/web"},
			issuer: external,
			reason: "NamesNotAllowed",
		},
		{
			name:   "an IP address the namespace lost",
			spec:   &structs.CertificateSpec{Issuer: "issuer", IPAddresses: []string{"10.0.1.5"}, SecretPath: "certs/web"},
			issuer: builtin,
			reason: "NamesNotAllowed",
			lost:   true,
		},
		{
			name:   "a DNS name the namespace lost",
			spec:   &structs.CertificateSpec{Issuer: "issuer", DNSNames: []string{"web.example.org"}, SecretPath: "certs/web"},
			issuer: external,
			reason: "NamesNotAllowed",
			lost:   true,
		},
		{
			name:   "a DNS name the namespace lost, the issuer gone",
			spec:   &structs.CertificateSpec{Issuer: "issuer", DNSNames: []string{"web.example.org"}, SecretPath: "certs/web"},
			reason: "NamesNotAllowed",
			lost:   true,
		},
		{
			name:   "the issuer gone",
			spec:   &structs.CertificateSpec{Issuer: "issuer", DNSNames: []string{"web.example.com"}, SecretPath: "certs/web"},
			reason: "IssuerNotFound",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issuer, issuerSpec := issuerObj, tt.issuer
			if issuerSpec == nil {
				issuer, issuerSpec = nil, &structs.CertificateIssuerSpec{}
			}
			refused := o.refusal(p, obj, tt.spec, issuer, issuerSpec, nil, false)
			must.NotNil(t, refused)
			must.Eq(t, tt.reason, refused.reason)
			must.Eq(t, tt.lost, refused.lostNames)
		})
	}
}

func TestCertificate_ACL(t *testing.T) {
	ci.Parallel(t)

	s, root, cleanup := secretTestServer(t, true, nil)
	defer cleanup()
	waitOperatorKinds(t, s)
	admin := &certTest{t: t, s: s, token: root.SecretID}
	admin.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	_, err := admin.apply("", structs.CertificateIssuerKind, "internal", map[string]any{"builtin": map[string]any{}})
	must.NoError(t, err)

	objects := mock.CreatePolicyAndToken(t, s.State(), 1001, "objects",
		`namespace "default" { object "norca.io/Certificate" { capabilities = ["read", "write"] } }`)
	both := mock.CreatePolicyAndToken(t, s.State(), 1003, "both", `namespace "default" {
  object "norca.io/Certificate" { capabilities = ["read", "write"] }
  secret "certs/*" { capabilities = ["write-secret"] }
}`)
	spec := map[string]any{"issuer": "internal", "dns_names": []any{"web.example.com"}, "secret_path": "certs/web"}

	// Writing a certificate takes the right to write its secret.
	noSecret := &certTest{t: t, s: s, token: objects.SecretID}
	_, err = noSecret.apply("default", structs.CertificateKind, "web-tls", spec)
	must.Eq(t, 403, errCode(t, err))
	must.ErrorContains(t, err, "right to write its secret")

	writer := &certTest{t: t, s: s, token: both.SecretID}
	_, err = writer.apply("default", structs.CertificateKind, "web-tls", spec)
	must.NoError(t, err)
	admin.waitFor("default", "web-tls", "issued", certReady)

	// The CA is the operators' of the cluster.
	var caResp structs.CertificateAuthorityResponse
	err = s.RPC("Certificate.RotateAuthority", &structs.CertificateAuthorityRotateRequest{WriteRequest: writer.write("")}, &caResp)
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
	err = s.RPC("Certificate.ImportAuthority", &structs.CertificateAuthorityImportRequest{WriteRequest: writer.write("")}, &caResp)
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())

	// Revoking takes the write of the object.
	var revoked structs.CertificateRevokeResponse
	anonymous := &certTest{t: t, s: s}
	err = s.RPC("Certificate.Revoke", &structs.CertificateRevokeRequest{Name: "web-tls", WriteRequest: anonymous.write("default")}, &revoked)
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
	must.NoError(t, s.RPC("Certificate.Revoke", &structs.CertificateRevokeRequest{Name: "web-tls", WriteRequest: writer.write("default")}, &revoked))
}

// testCA is a CA of a test: a root, or an intermediate under one.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  string
}

func newTestCA(t *testing.T, name string, parent *testCA) *testCA {
	t.Helper()
	return newTestCAUntil(t, name, parent, time.Now().Add(240*time.Hour))
}

// newTestCAUntil returns a CA that is valid until notAfter.
func newTestCAUntil(t *testing.T, name string, parent *testCA, notAfter time.Time) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must.NoError(t, err)
	serial, err := randomSerial()
	must.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	signer, signerKey := template, key
	if parent != nil {
		signer, signerKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, template, signer, key.Public(), signerKey)
	must.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	must.NoError(t, err)
	return &testCA{cert: cert, key: key, pem: encodeCertificatePEM(der)}
}

func (ca *testCA) keyPEM(t *testing.T) string {
	t.Helper()
	out, err := encodePrivateKeyPEM(ca.key)
	must.NoError(t, err)
	return out
}

// sign issues a leaf for a public key.
func (ca *testCA) sign(t *testing.T, pub crypto.PublicKey, names []string, change func(*x509.Certificate)) string {
	t.Helper()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		DNSNames:     names,
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if change != nil {
		change(template)
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, pub, ca.key)
	must.NoError(t, err)
	return encodeCertificatePEM(der)
}

func TestCheckOrderChain(t *testing.T) {
	ci.Parallel(t)

	root := newTestCA(t, "root", nil)
	intermediate := newTestCA(t, "intermediate", root)
	other := newTestCA(t, "other", nil)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must.NoError(t, err)
	otherKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must.NoError(t, err)
	names := []string{"web.example.com", "*.web.example.com"}
	spec := &structs.CertificateSpec{DNSNames: []string{"*.web.example.com", "web.example.com"}}
	now := time.Now()

	tests := []struct {
		name     string
		chain    string
		roots    string
		expected string
	}{
		{name: "leaf and intermediate", chain: intermediate.sign(t, key.Public(), names, nil) + intermediate.pem, roots: root.pem},
		{name: "the root at the end is accepted and not stored", chain: intermediate.sign(t, key.Public(), names, nil) + intermediate.pem + root.pem, roots: root.pem},
		{name: "no chain", chain: "", roots: root.pem, expected: "holds no certificate"},
		{name: "not PEM", chain: "garbage", roots: root.pem, expected: "data after the last PEM block"},
		{name: "another key", chain: intermediate.sign(t, otherKey.Public(), names, nil) + intermediate.pem, roots: root.pem, expected: "not for the key"},
		{name: "a name too many", chain: intermediate.sign(t, key.Public(), append(slices.Clone(names), "admin.example.com"), nil) + intermediate.pem, roots: root.pem, expected: "is for the names"},
		{name: "a name missing", chain: intermediate.sign(t, key.Public(), names[:1], nil) + intermediate.pem, roots: root.pem, expected: "is for the names"},
		{name: "the intermediate is missing", chain: intermediate.sign(t, key.Public(), names, nil), roots: root.pem, expected: "unknown authority"},
		{name: "a root that was not declared", chain: other.sign(t, key.Public(), names, nil), roots: root.pem, expected: "unknown authority"},
		{name: "no roots declared", chain: other.sign(t, key.Public(), names, nil), roots: "", expected: "declared no roots"},
		{name: "a CA certificate", chain: intermediate.sign(t, key.Public(), names, func(c *x509.Certificate) {
			c.IsCA, c.BasicConstraintsValid, c.KeyUsage = true, true, x509.KeyUsageCertSign
		}) + intermediate.pem, roots: root.pem, expected: "CA certificate"},
		{name: "expired", chain: intermediate.sign(t, key.Public(), names, func(c *x509.Certificate) {
			c.NotBefore, c.NotAfter = now.Add(-2*time.Hour), now.Add(-time.Hour)
		}) + intermediate.pem, roots: root.pem, expected: "expired"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			leaf, stored, err := checkOrderChain(tt.chain, key.Public(), spec, tt.roots, now)
			if tt.expected != "" {
				must.ErrorContains(t, err, tt.expected)
				return
			}
			must.NoError(t, err)
			must.NotNil(t, leaf)
			certs, err := parseCertificatesPEM(stored)
			must.NoError(t, err)
			must.Len(t, 2, certs)
			must.True(t, certs[1].Equal(intermediate.cert))
		})
	}
}

func TestCheckImportedCA(t *testing.T) {
	ci.Parallel(t)

	root := newTestCA(t, "root", nil)
	intermediate := newTestCA(t, "intermediate", root)
	other := newTestCA(t, "other", nil)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must.NoError(t, err)
	now := time.Now()

	tests := []struct {
		name     string
		cert     string
		key      string
		above    int
		expected string
	}{
		{name: "a self-signed CA", cert: root.pem, key: root.keyPEM(t)},
		{name: "an intermediate with its root", cert: intermediate.pem + root.pem, key: intermediate.keyPEM(t), above: 1},
		{name: "an intermediate without its root", cert: intermediate.pem, key: intermediate.keyPEM(t), expected: "not self-signed"},
		{name: "an intermediate with the wrong root", cert: intermediate.pem + other.pem, key: intermediate.keyPEM(t), expected: "does not sign"},
		{name: "a root followed by a certificate", cert: root.pem + other.pem, key: root.keyPEM(t), expected: "must not be followed"},
		{name: "the key of another CA", cert: root.pem, key: other.keyPEM(t), expected: "not the key of the certificate"},
		{name: "a leaf", cert: root.sign(t, leafKey.Public(), []string{"web.example.com"}, nil), key: root.keyPEM(t), expected: "not a CA certificate"},
		{name: "no certificate", cert: "", key: root.keyPEM(t), expected: "no certificate"},
		{name: "no key", cert: root.pem, key: "", expected: "one PEM block"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ca, above, key, err := checkImportedCA(tt.cert, tt.key, now)
			if tt.expected != "" {
				must.ErrorContains(t, err, tt.expected)
				return
			}
			must.NoError(t, err)
			must.NotNil(t, ca)
			must.NotNil(t, key)
			must.Len(t, tt.above, above)
		})
	}
}

func TestCertificate_ImportedAndRotatedAuthority(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	_, err := c.apply("", structs.CertificateIssuerKind, "internal", map[string]any{"builtin": map[string]any{}})
	must.NoError(t, err)
	_, err = c.apply("default", structs.CertificateKind, "web-tls", map[string]any{
		"issuer": "internal", "dns_names": []any{"web.example.com"}, "secret_path": "certs/web"})
	must.NoError(t, err)
	first := c.waitFor("default", "web-tls", "issued", certReady)
	own := c.roots()

	// An intermediate CA of the organisation takes the place of the CA of
	// the region: the certificate is issued again, served with the
	// intermediate, and the bundle holds the root of the organisation and,
	// until its certificates expire, the retired CA.
	root := newTestCA(t, "org root", nil)
	intermediate := newTestCA(t, "org issuing", root)
	var caResp structs.CertificateAuthorityResponse
	must.NoError(t, c.s.RPC("Certificate.ImportAuthority", &structs.CertificateAuthorityImportRequest{
		Cert: intermediate.pem + root.pem, Key: intermediate.keyPEM(t), WriteRequest: c.write(""),
	}, &caResp))
	must.True(t, caResp.Authority.Imported)
	must.Nil(t, caResp.Authority.SigningKey)

	second := c.waitFor("default", "web-tls", "issued by the imported CA", func(obj *structs.CustomObject, st *structs.CertificateStatus) bool {
		return certReady(obj, st) && st.Authority == caResp.Authority.ID
	})
	must.NotEq(t, first.Serial, second.Serial)
	items := c.secretItems("default", "certs/web")
	served, err := parseCertificatesPEM(items[structs.CertificateSecretItemCert])
	must.NoError(t, err)
	must.Len(t, 2, served)
	must.True(t, served[1].Equal(intermediate.cert))
	bundle := c.roots()
	must.Eq(t, root.pem+own.Roots, bundle.Roots)
	must.Eq(t, bundle.Roots, items[structs.CertificateSecretItemCA])
	intermediates := x509.NewCertPool()
	intermediates.AddCert(served[1])
	_, err = served[0].Verify(x509.VerifyOptions{Roots: certPool(t, root.pem), Intermediates: intermediates, DNSName: "web.example.com"})
	must.NoError(t, err)

	// A rotation on request: a new CA of the leader, a new certificate.
	must.NoError(t, c.s.RPC("Certificate.RotateAuthority", &structs.CertificateAuthorityRotateRequest{WriteRequest: c.write("")}, &caResp))
	must.False(t, caResp.Authority.Imported)
	third := c.waitFor("default", "web-tls", "issued by the rotated CA", func(obj *structs.CustomObject, st *structs.CertificateStatus) bool {
		return certReady(obj, st) && st.Authority == caResp.Authority.ID
	})
	must.NotEq(t, second.Serial, third.Serial)
	must.Len(t, 3, c.roots().Authorities)

	// A snapshot carries the CAs with their keys; a restore brings them
	// back.
	all, err := c.s.State().CertificateAuthorities(nil)
	must.NoError(t, err)
	must.Len(t, 3, all)
	for _, ca := range all {
		must.Positive(t, len(ca.SigningKey), must.Sprint("a retired CA keeps its key for its revocation list"))
		inUse, err := c.s.State().IsRootKeyMetaInUse(ca.SigningKeyID)
		must.NoError(t, err)
		must.True(t, inUse)
	}
}

func TestCertificate_ExternalIssuer(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})

	root := newTestCA(t, "acme root", nil)
	intermediate := newTestCA(t, "acme issuing", root)
	_, err := c.apply("", structs.CertificateIssuerKind, "acme", map[string]any{"external": map[string]any{"controller": "test"}})
	must.NoError(t, err)
	_, err = c.apply("default", structs.CertificateKind, "web-tls", map[string]any{
		"issuer": "acme", "dns_names": []any{"web.example.com"}, "secret_path": "certs/web"})
	must.NoError(t, err)

	// Nothing is ordered while the controller of the issuer is away.
	c.waitFor("default", "web-tls", "IssuerUnavailable", func(obj *structs.CustomObject, st *structs.CertificateStatus) bool {
		return objectConditionTrue(obj.Status, structs.CertificateConditionIssuerUnavailable) && st.Order == ""
	})
	must.Eq(t, 0, certificateOrderCount(t, c.s))

	// The controller reports: the order is placed with a signing request,
	// and the key waits in the secret.
	heartbeat := func() {
		must.NoError(t, c.status("", structs.CertificateIssuerKind, "acme", map[string]any{
			"heartbeat": time.Now().UTC().Format(time.RFC3339), "roots": root.pem}))
	}
	heartbeat()
	ordered := c.waitFor("default", "web-tls", "an order", func(obj *structs.CustomObject, st *structs.CertificateStatus) bool {
		return st.Order != "" && objectConditionTrue(obj.Status, structs.CertificateConditionIssuing)
	})
	must.Eq(t, "default.web-tls.1", ordered.Order)
	order := c.get("", structs.CertificateOrderKind, ordered.Order)
	must.NotNil(t, order)
	orderSpec := &structs.CertificateOrderSpec{}
	must.NoError(t, structs.DecodeObjectPart(order.Spec, orderSpec))
	must.Eq(t, "default/web-tls", orderSpec.Certificate)
	csr, err := parseWorkloadCSR(orderSpec.CSR)
	must.NoError(t, err)
	must.Eq(t, []string{"web.example.com"}, csr.DNSNames)
	must.Eq(t, []string{"next_key"}, c.secretItems("default", "certs/web").Keys())

	// A chain for another name is refused; the order is dropped.
	must.NoError(t, c.status("", structs.CertificateOrderKind, ordered.Order, map[string]any{
		"state": "valid", "chain": intermediate.sign(t, csr.PublicKey, []string{"other.example.com"}, nil) + intermediate.pem}))
	c.waitFor("default", "web-tls", "ChainRefused", func(obj *structs.CustomObject, st *structs.CertificateStatus) bool {
		reason, _ := certificateFailure(obj.Status)
		return reason == "ChainRefused" && st.Order == "" && st.Failures == 1
	})
	must.Nil(t, c.get("", structs.CertificateOrderKind, ordered.Order))
}

// certificateOrderCount returns the number of certificate orders in the
// state.
func certificateOrderCount(t *testing.T, s *Server) int {
	t.Helper()
	orders, err := listObjects(s.State(), nil, structs.CertificateOrderKind, "")
	must.NoError(t, err)
	return len(orders)
}

func TestCertificate_ExternalIssuerChain(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})

	root := newTestCA(t, "acme root", nil)
	intermediate := newTestCA(t, "acme issuing", root)
	_, err := c.apply("", structs.CertificateIssuerKind, "acme", map[string]any{"external": map[string]any{"controller": "test"}})
	must.NoError(t, err)
	must.NoError(t, c.status("", structs.CertificateIssuerKind, "acme", map[string]any{
		"heartbeat": time.Now().UTC().Format(time.RFC3339), "roots": root.pem}))
	_, err = c.apply("default", structs.CertificateKind, "web-tls", map[string]any{
		"issuer": "acme", "dns_names": []any{"web.example.com"}, "secret_path": "certs/web"})
	must.NoError(t, err)

	// fulfil plays the controller: it signs the request of the open order.
	fulfil := func(orderName string, renewAfter string) *x509.Certificate {
		order := c.get("", structs.CertificateOrderKind, orderName)
		must.NotNil(t, order)
		orderSpec := &structs.CertificateOrderSpec{}
		must.NoError(t, structs.DecodeObjectPart(order.Spec, orderSpec))
		csr, err := parseWorkloadCSR(orderSpec.CSR)
		must.NoError(t, err)
		leafPEM := intermediate.sign(t, csr.PublicKey, csr.DNSNames, nil)
		status := map[string]any{"state": "valid", "chain": leafPEM + intermediate.pem + root.pem}
		if renewAfter != "" {
			status["renew_after"] = renewAfter
		}
		must.NoError(t, c.status("", structs.CertificateOrderKind, orderName, status))
		certs, err := parseCertificatesPEM(leafPEM)
		must.NoError(t, err)
		return certs[0]
	}

	ordered := c.waitFor("default", "web-tls", "an order", func(_ *structs.CustomObject, st *structs.CertificateStatus) bool { return st.Order != "" })
	suggested := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	leaf := fulfil(ordered.Order, suggested.Format(time.RFC3339))
	issued := c.waitFor("default", "web-tls", "issued", certReady)
	must.Eq(t, certificateSerial(leaf), issued.Serial)
	must.Eq(t, "", issued.Order)
	must.Eq(t, "", issued.Authority)
	must.Eq(t, suggested.Format(time.RFC3339), issued.RenewAt, must.Sprint("the renewal time the issuer suggests is taken"))
	must.Nil(t, c.get("", structs.CertificateOrderKind, ordered.Order))

	// The secret: the leaf with the intermediate, without the root, and the
	// key the request was made with; the waiting key is gone.
	items := c.secretItems("default", "certs/web")
	must.Eq(t, []string{"cert", "key"}, items.Keys())
	served, err := parseCertificatesPEM(items[structs.CertificateSecretItemCert])
	must.NoError(t, err)
	must.Len(t, 2, served)
	must.True(t, served[0].Equal(leaf))
	must.True(t, served[1].Equal(intermediate.cert))
	key, err := parsePrivateKeyPEM(items[structs.CertificateSecretItemKey])
	must.NoError(t, err)
	must.True(t, publicKeysEqual(key.Public(), leaf.PublicKey))

	// A revocation goes to the issuer as an order of its own, and a new
	// certificate is ordered; the current one is served until it is there.
	var revoked structs.CertificateRevokeResponse
	must.NoError(t, c.s.RPC("Certificate.Revoke", &structs.CertificateRevokeRequest{Name: "web-tls", WriteRequest: c.write("default")}, &revoked))
	renewing := c.waitFor("default", "web-tls", "a second order", func(obj *structs.CustomObject, st *structs.CertificateStatus) bool {
		return st.Order != "" && objectConditionTrue(obj.Status, structs.CertificateConditionRenewing)
	})
	must.Eq(t, "default.web-tls.2", renewing.Order)
	must.Eq(t, issued.Serial, renewing.Serial)
	waiting := c.secretItems("default", "certs/web")
	must.Eq(t, []string{"cert", "key", "next_key"}, waiting.Keys())
	must.Eq(t, items[structs.CertificateSecretItemCert], waiting[structs.CertificateSecretItemCert])
	must.Eq(t, items[structs.CertificateSecretItemKey], waiting[structs.CertificateSecretItemKey])

	orders, err := listObjects(c.s.State(), nil, structs.CertificateOrderKind, "")
	must.NoError(t, err)
	var revocation *structs.CertificateOrderSpec
	for _, o := range orders {
		spec := &structs.CertificateOrderSpec{}
		must.NoError(t, structs.DecodeObjectPart(o.Spec, spec))
		if spec.Revoke != "" {
			revocation = spec
		}
	}
	must.NotNil(t, revocation)
	must.Eq(t, issued.Serial, revocation.Revoke)

	second := fulfil(renewing.Order, "")
	renewed := c.waitFor("default", "web-tls", "renewed", func(obj *structs.CustomObject, st *structs.CertificateStatus) bool {
		return certReady(obj, st) && st.Serial == certificateSerial(second) && st.Order == ""
	})
	must.Eq(t, "", renewed.Revoke)
	must.Eq(t, []string{"cert", "key"}, c.secretItems("default", "certs/web").Keys())
}

// The certificate of an external issuer that a namespace may no longer ask
// for is taken out of the secret and revoked by the issuer (review finding
// ATM-21), with the reason cessationOfOperation.
func TestCertificate_ExternalIssuerLostNames(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})

	root := newTestCA(t, "acme root", nil)
	intermediate := newTestCA(t, "acme issuing", root)
	_, err := c.apply("", structs.CertificateIssuerKind, "acme", map[string]any{"external": map[string]any{"controller": "test"}})
	must.NoError(t, err)
	must.NoError(t, c.status("", structs.CertificateIssuerKind, "acme", map[string]any{
		"heartbeat": time.Now().UTC().Format(time.RFC3339), "roots": root.pem}))
	_, err = c.apply("default", structs.CertificateKind, "web-tls", map[string]any{
		"issuer": "acme", "dns_names": []any{"web.example.com"}, "secret_path": "certs/web"})
	must.NoError(t, err)

	ordered := c.waitFor("default", "web-tls", "an order", func(_ *structs.CustomObject, st *structs.CertificateStatus) bool { return st.Order != "" })
	order := c.get("", structs.CertificateOrderKind, ordered.Order)
	must.NotNil(t, order)
	orderSpec := &structs.CertificateOrderSpec{}
	must.NoError(t, structs.DecodeObjectPart(order.Spec, orderSpec))
	csr, err := parseWorkloadCSR(orderSpec.CSR)
	must.NoError(t, err)
	must.NoError(t, c.status("", structs.CertificateOrderKind, ordered.Order, map[string]any{
		"state": "valid", "chain": intermediate.sign(t, csr.PublicKey, csr.DNSNames, nil) + intermediate.pem}))
	issued := c.waitFor("default", "web-tls", "issued", certReady)

	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.org"})
	st := c.waitFor("default", "web-tls", "withdrawn", withdrawn)
	must.Eq(t, structs.CertificateStatus{Orders: st.Orders, Revoke: issued.Serial}, *st)
	must.SliceContains(t, c.eventReasons("default", structs.CertificateKind, "web-tls"), "Withdrawn")

	// Every version of the secret that held the key is destroyed.
	secret, err := c.s.State().SecretByPath(nil, "default", "certs/web")
	must.NoError(t, err)
	must.NotNil(t, secret)
	for _, v := range secret.Versions {
		must.True(t, v.Destroyed, must.Sprintf("version %d is destroyed", v.Version))
	}

	// The issuer has a revocation order for the serial, and no order for a
	// new certificate is placed.
	orders, err := listObjects(c.s.State(), nil, structs.CertificateOrderKind, "")
	must.NoError(t, err)
	must.Len(t, 1, orders)
	revocation := &structs.CertificateOrderSpec{}
	must.NoError(t, structs.DecodeObjectPart(orders[0].Spec, revocation))
	must.Eq(t, issued.Serial, revocation.Revoke)
	must.Eq(t, "default/web-tls", revocation.Certificate)
	must.Eq[any](t, "cessationOfOperation", orders[0].Spec["reason"])
}

// A namespace that loses the names of a certificate whose issuer is gone too
// loses the certificate all the same (review finding ATM-21): the lost right
// is found before the missing issuer. The certificate of the built-in issuer
// is listed in the revocation list of its CA, which outlives the issuer; for
// that of an external issuer nobody is left to revoke it, which the event
// and the Failed condition say.
func TestCertificate_LostNamesIssuerGone(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})

	// A certificate of the built-in issuer.
	_, err := c.apply("", structs.CertificateIssuerKind, "internal", map[string]any{"builtin": map[string]any{}})
	must.NoError(t, err)
	_, err = c.apply("default", structs.CertificateKind, "internal-tls", map[string]any{
		"issuer": "internal", "dns_names": []any{"internal.example.com"}, "secret_path": "certs/internal"})
	must.NoError(t, err)
	builtin := c.waitFor("default", "internal-tls", "issued", certReady)

	// A certificate of an external issuer.
	root := newTestCA(t, "acme root", nil)
	intermediate := newTestCA(t, "acme issuing", root)
	_, err = c.apply("", structs.CertificateIssuerKind, "acme", map[string]any{"external": map[string]any{"controller": "test"}})
	must.NoError(t, err)
	must.NoError(t, c.status("", structs.CertificateIssuerKind, "acme", map[string]any{
		"heartbeat": time.Now().UTC().Format(time.RFC3339), "roots": root.pem}))
	_, err = c.apply("default", structs.CertificateKind, "acme-tls", map[string]any{
		"issuer": "acme", "dns_names": []any{"acme.example.com"}, "secret_path": "certs/acme"})
	must.NoError(t, err)
	ordered := c.waitFor("default", "acme-tls", "an order", func(_ *structs.CustomObject, st *structs.CertificateStatus) bool { return st.Order != "" })
	order := c.get("", structs.CertificateOrderKind, ordered.Order)
	must.NotNil(t, order)
	orderSpec := &structs.CertificateOrderSpec{}
	must.NoError(t, structs.DecodeObjectPart(order.Spec, orderSpec))
	csr, err := parseWorkloadCSR(orderSpec.CSR)
	must.NoError(t, err)
	must.NoError(t, c.status("", structs.CertificateOrderKind, ordered.Order, map[string]any{
		"state": "valid", "chain": intermediate.sign(t, csr.PublicKey, csr.DNSNames, nil) + intermediate.pem}))
	external := c.waitFor("default", "acme-tls", "issued", certReady)

	// Both issuers go, then the names.
	must.NoError(t, c.delete("", structs.CertificateIssuerKind, "internal"))
	must.NoError(t, c.delete("", structs.CertificateIssuerKind, "acme"))
	c.waitFor("default", "internal-tls", "IssuerNotFound", func(obj *structs.CustomObject, _ *structs.CertificateStatus) bool {
		reason, _ := certificateFailure(obj.Status)
		return reason == "IssuerNotFound"
	})
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.org"})

	st := c.waitFor("default", "internal-tls", "withdrawn", withdrawn)
	must.Eq(t, builtin.Serial, st.Revoke)
	must.SliceContains(t, c.crlSerials(), builtin.Serial)
	must.Nil(t, c.get("", structs.CertificateIssuerKind, "internal"))

	st = c.waitFor("default", "acme-tls", "withdrawn", withdrawn)
	must.Eq(t, "", st.Revoke, must.Sprint("nothing was revoked"))
	messages := c.eventMessages("default", structs.CertificateKind, "acme-tls", "Withdrawn")
	must.Len(t, 1, messages)
	must.StrContains(t, messages[0], `the issuer "acme" is gone`)
	must.StrContains(t, messages[0], external.Serial)
	must.Eq(t, 0, certificateOrderCount(t, c.s))
	for _, path := range []string{"certs/internal", "certs/acme"} {
		secret, err := c.s.State().SecretByPath(nil, "default", path)
		must.NoError(t, err)
		for _, v := range secret.Versions {
			must.True(t, v.Destroyed, must.Sprintf("%s: version %d is destroyed", path, v.Version))
		}
	}
}

// An imported CA is never rotated by the leader. When it is about to expire
// or has expired nothing is issued with it (review finding SRV-16): a leaf
// can't outlive its CA, so it would be due for renewal as soon as it is
// issued, and the operator would issue it again on every run.
func TestCertificate_ExpiredImportedAuthority(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	_, err := c.apply("", structs.CertificateIssuerKind, "internal", map[string]any{"builtin": map[string]any{}})
	must.NoError(t, err)
	_, err = c.apply("default", structs.CertificateKind, "web-tls", map[string]any{
		"issuer": "internal", "dns_names": []any{"web.example.com"}, "secret_path": "certs/web"})
	must.NoError(t, err)
	first := c.waitFor("default", "web-tls", "issued", certReady)

	ca := newTestCAUntil(t, "org issuing", nil, time.Now().Add(3*time.Second))
	var caResp structs.CertificateAuthorityResponse
	must.NoError(t, c.s.RPC("Certificate.ImportAuthority", &structs.CertificateAuthorityImportRequest{
		Cert: ca.pem, Key: ca.keyPEM(t), WriteRequest: c.write(""),
	}, &caResp))

	// Past the end of the CA.
	time.Sleep(5 * time.Second)
	_, before := c.certificate("default", "web-tls")
	time.Sleep(2 * time.Second)
	obj, after := c.certificate("default", "web-tls")
	must.Eq(t, before.Serial, after.Serial, must.Sprint("the certificate is issued again and again"))
	must.Eq(t, first.Serial, after.Serial, must.Sprint("nothing is issued with a CA that ends within the hour"))
	must.Positive(t, after.Failures)
	must.False(t, objectConditionTrue(obj.Status, structs.CertificateConditionIssuing))

	issuer := c.get("", structs.CertificateIssuerKind, "internal")
	must.False(t, objectConditionTrue(issuer.Status, structs.CertificateConditionReady))
	must.True(t, objectConditionTrue(issuer.Status, structs.CertificateConditionDegraded))
}

func TestSignCertificate_ExpiredCA(t *testing.T) {
	ci.Parallel(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must.NoError(t, err)
	spec := &structs.CertificateSpec{DNSNames: []string{"web.example.com"}}

	ca := newTestCAUntil(t, "expired", nil, time.Now().Add(-time.Minute))
	_, _, err = signCertificate(&mtlsSigner{cert: ca.cert, key: ca.key}, key.Public(), spec, time.Hour)
	must.ErrorContains(t, err, "expired")

	ca = newTestCA(t, "valid", nil)
	_, cert, err := signCertificate(&mtlsSigner{cert: ca.cert, key: ca.key}, key.Public(), spec, time.Hour)
	must.NoError(t, err)
	must.True(t, cert.NotAfter.After(cert.NotBefore))
}
