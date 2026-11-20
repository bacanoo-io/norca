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
	"strings"
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

// crlReasons returns the CRLReason codes of the entries in the revocation
// list of the active issuing CA, by serial number; 0 for an entry without
// one.
func (c *certTest) crlReasons() map[string]int {
	c.t.Helper()
	var resp structs.CertificateCRLResponse
	must.NoError(c.t, c.s.RPC("Certificate.CRL", &structs.CertificateCRLRequest{QueryOptions: c.query("")}, &resp))
	block, _ := pem.Decode([]byte(resp.CRL))
	must.NotNil(c.t, block)
	crl, err := x509.ParseRevocationList(block.Bytes)
	must.NoError(c.t, err)
	reasons := map[string]int{}
	for _, entry := range crl.RevokedCertificateEntries {
		reasons[certificateSerial(&x509.Certificate{SerialNumber: entry.SerialNumber})] = entry.ReasonCode
	}
	return reasons
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
		// What was held is an earlier certificate now, revoked (review
		// finding ATM-24b).
		must.Eq(t, structs.CertificateStatus{Orders: st.Orders, Revoke: tc.issued.Serial, Earlier: []*structs.CertificateEarlier{{
			Serial: tc.issued.Serial, Issuer: "internal", Authority: tc.issued.Authority, NotAfter: tc.issued.NotAfter, Revoked: true,
		}}}, *st, must.Sprint("nothing in the status says a certificate is held"))
		obj, _ := c.certificate("default", tc.name)
		must.False(t, objectConditionTrue(obj.Status, structs.CertificateConditionReady))
		must.SliceContains(t, c.eventReasons("default", structs.CertificateKind, tc.name), "Withdrawn")
		must.SliceContains(t, c.crlSerials(), tc.issued.Serial)
		must.Eq(t, 5, c.crlReasons()[tc.issued.Serial], must.Sprint(
			"the revocation list gives the reason cessationOfOperation (review finding ATM-25)"))
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

// A user's revocation may give a reason (review finding ATM-25): the
// revocation list of the built-in issuer carries its code, and the CA keeps
// it. A reason other than those a subscriber may give is refused.
func TestCertificate_RevokeReason(t *testing.T) {
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

	revoke := func(reason string) (string, error) {
		var resp structs.CertificateRevokeResponse
		err := c.s.RPC("Certificate.Revoke", &structs.CertificateRevokeRequest{
			Name: "web-tls", Reason: reason, WriteRequest: c.write("default")}, &resp)
		return resp.Serial, err
	}

	for _, reason := range []string{"caCompromise", "certificateHold", "KeyCompromise", "bogus"} {
		_, err := revoke(reason)
		must.Error(t, err, must.Sprint(reason))
		code, _, ok := structs.CodeFromRPCCodedErr(err)
		must.True(t, ok, must.Sprint(reason))
		must.Eq(t, 400, code, must.Sprint(reason))
	}
	must.SliceEmpty(t, c.crlSerials(), must.Sprint("nothing is revoked for a refused reason"))

	tests := []struct {
		reason   string
		code     int
		recorded string
	}{
		{reason: structs.CertificateRevokeReasonKeyCompromise, code: 1, recorded: "keyCompromise"},
		{reason: structs.CertificateRevokeReasonSuperseded, code: 4, recorded: "superseded"},
		{reason: structs.CertificateRevokeReasonUnspecified, code: 0, recorded: ""},
		{reason: "", code: 0, recorded: ""},
	}
	for _, tt := range tests {
		serial, err := revoke(tt.reason)
		must.NoError(t, err, must.Sprint(tt.reason))
		must.Eq(t, held.Serial, serial)
		must.Eq(t, tt.code, c.crlReasons()[serial], must.Sprint(tt.reason))

		ca := c.roots().Authorities[0]
		i := slices.IndexFunc(ca.Revoked, func(r *structs.CertificateRevocation) bool { return r.Serial == serial })
		must.NonNegative(t, i)
		must.Eq(t, tt.recorded, ca.Revoked[i].Reason, must.Sprint(tt.reason))

		held = c.waitFor("default", "web-tls", "issued after the revocation", func(obj *structs.CustomObject, st *structs.CertificateStatus) bool {
			return certReady(obj, st) && st.Serial != serial && st.Revoke == ""
		})
	}
	messages := c.eventMessages("default", structs.CertificateKind, "web-tls", "Revoked")
	must.True(t, slices.ContainsFunc(messages, func(m string) bool { return strings.Contains(m, "for the reason keyCompromise") }),
		must.Sprint("the event says the reason"))
}

// The reason of a certificate that is revoked already may still be raised to
// keyCompromise, and to nothing else (review finding ATM-25b): the entry of
// the revocation list keeps the time of the revocation and carries the new
// code. Any other reason for a revoked certificate changes nothing, without
// an error.
func TestCertificate_RevokeReasonRaised(t *testing.T) {
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

	// Without its issuer nothing new is issued after the revocation: the
	// revoked certificate stays the current one.
	must.NoError(t, c.delete("", structs.CertificateIssuerKind, "internal"))
	c.waitFor("default", "web-tls", "IssuerNotFound", func(obj *structs.CustomObject, _ *structs.CertificateStatus) bool {
		reason, _ := certificateFailure(obj.Status)
		return reason == "IssuerNotFound"
	})

	revoke := func(reason string) {
		t.Helper()
		var resp structs.CertificateRevokeResponse
		must.NoError(t, c.s.RPC("Certificate.Revoke", &structs.CertificateRevokeRequest{
			Name: "web-tls", Reason: reason, WriteRequest: c.write("default")}, &resp), must.Sprint(reason))
		must.Eq(t, held.Serial, resp.Serial, must.Sprint(reason))
	}
	entry := func() *structs.CertificateRevocation {
		t.Helper()
		ca := c.roots().Authorities[0]
		i := slices.IndexFunc(ca.Revoked, func(r *structs.CertificateRevocation) bool { return r.Serial == held.Serial })
		must.NonNegative(t, i)
		must.Len(t, 1, ca.Revoked)
		return ca.Revoked[i]
	}

	revoke(structs.CertificateRevokeReasonSuperseded)
	first := entry()
	must.Eq(t, "superseded", first.Reason)
	_, st := c.certificate("default", "web-tls")
	must.Eq(t, held.Serial, st.Serial)
	must.Eq(t, held.Serial, st.Revoke)

	// Another reason, or none, for the revoked certificate: nothing changes.
	for _, reason := range []string{structs.CertificateRevokeReasonAffiliationChanged, structs.CertificateRevokeReasonUnspecified, ""} {
		revoke(reason)
		must.Eq(t, first, entry(), must.Sprint(reason))
	}
	must.Eq(t, 4, c.crlReasons()[held.Serial])
	must.SliceEmpty(t, c.eventMessages("default", structs.CertificateKind, "web-tls", "RevocationReason"))

	// keyCompromise takes its place, once.
	revoke(structs.CertificateRevokeReasonKeyCompromise)
	raised := entry()
	must.Eq(t, "keyCompromise", raised.Reason, must.Sprint("the reason of a revoked certificate is raised to keyCompromise"))
	must.Eq(t, first.RevokedAt, raised.RevokedAt)
	must.Eq(t, 1, c.crlReasons()[held.Serial], must.Sprint("the revocation list gives the reason keyCompromise"))
	revoke(structs.CertificateRevokeReasonKeyCompromise)
	revoke(structs.CertificateRevokeReasonSuperseded)
	must.Eq(t, raised, entry(), must.Sprint("keyCompromise is not taken back"))
	messages := c.eventMessages("default", structs.CertificateKind, "web-tls", "RevocationReason")
	must.Len(t, 1, messages)
	must.StrContains(t, messages[0], "keyCompromise")
	must.StrContains(t, messages[0], held.Serial)

	// The time of the revocation is that of the first one, whenever the
	// reason is raised.
	authority := c.roots().ActiveID
	at := time.Now().UTC().Truncate(time.Second)
	revokeAt := func(reason string, after time.Duration) (bool, *structs.CertificateRevocation) {
		t.Helper()
		found, changed, err := c.s.certificateCA.revoke(authority, &structs.CertificateRevocation{
			Serial: "0a0b0c", Certificate: "default/other", RevokedAt: at.Add(after), NotAfter: at.Add(24 * time.Hour), Reason: reason})
		must.NoError(t, err)
		must.True(t, found)
		ca := c.roots().Authorities[0]
		i := slices.IndexFunc(ca.Revoked, func(r *structs.CertificateRevocation) bool { return r.Serial == "0a0b0c" })
		must.NonNegative(t, i)
		return changed, ca.Revoked[i]
	}
	changed, rev := revokeAt("", 0)
	must.True(t, changed)
	changed, rev = revokeAt(structs.CertificateRevokeReasonCessationOfOperation, time.Hour)
	must.False(t, changed)
	must.Eq(t, "", rev.Reason)
	changed, rev = revokeAt(structs.CertificateRevokeReasonKeyCompromise, 2*time.Hour)
	must.True(t, changed)
	must.Eq(t, "keyCompromise", rev.Reason)
	must.True(t, rev.RevokedAt.Equal(at), must.Sprintf("revoked at %s, not at %s", rev.RevokedAt, at))
	changed, rev = revokeAt(structs.CertificateRevokeReasonSuperseded, 3*time.Hour)
	must.False(t, changed)
	must.Eq(t, "keyCompromise", rev.Reason)
	must.Eq(t, 1, c.crlReasons()["0a0b0c"])
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

	// A revocation goes to the issuer as an order of its own, with the
	// reason (review finding ATM-25), and a new certificate is ordered; the
	// current one is served until it is there. While the controller has not
	// finished the order, a repeated revocation raises its reason to
	// keyCompromise; another reason leaves it (review finding ATM-25b).
	revocationReason := func() string {
		t.Helper()
		o := c.get("", structs.CertificateOrderKind, certificateRevokeOrderName("default", "web-tls", issued.Serial))
		must.NotNil(t, o)
		spec := &structs.CertificateOrderSpec{}
		must.NoError(t, structs.DecodeObjectPart(o.Spec, spec))
		return spec.Reason
	}
	var revoked structs.CertificateRevokeResponse
	must.NoError(t, c.s.RPC("Certificate.Revoke", &structs.CertificateRevokeRequest{
		Name: "web-tls", Reason: structs.CertificateRevokeReasonSuperseded, WriteRequest: c.write("default")}, &revoked))
	must.Eq(t, structs.CertificateRevokeReasonSuperseded, revocationReason())
	must.NoError(t, c.s.RPC("Certificate.Revoke", &structs.CertificateRevokeRequest{
		Name: "web-tls", Reason: structs.CertificateRevokeReasonAffiliationChanged, WriteRequest: c.write("default")}, &revoked))
	must.Eq(t, structs.CertificateRevokeReasonSuperseded, revocationReason())
	must.NoError(t, c.s.RPC("Certificate.Revoke", &structs.CertificateRevokeRequest{
		Name: "web-tls", Reason: structs.CertificateRevokeReasonKeyCompromise, WriteRequest: c.write("default")}, &revoked))
	must.Eq(t, structs.CertificateRevokeReasonKeyCompromise, revocationReason(),
		must.Sprint("the reason of the open revocation order is raised to keyCompromise"))
	must.Eq(t, issued.Serial, revoked.Serial)
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
	must.Eq(t, structs.CertificateRevokeReasonKeyCompromise, revocation.Reason)

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

	// revocationOrder waits for the revocation order of the serial with the
	// state and returns its name.
	revocationOrder := func(state string) string {
		t.Helper()
		var name string
		must.Wait(t, wait.InitialSuccess(
			wait.BoolFunc(func() bool {
				orders, err := listObjects(c.s.State(), nil, structs.CertificateOrderKind, "")
				must.NoError(t, err)
				name = ""
				for _, o := range orders {
					spec := &structs.CertificateOrderSpec{}
					must.NoError(t, structs.DecodeObjectPart(o.Spec, spec))
					status := &structs.CertificateOrderStatus{}
					must.NoError(t, structs.DecodeObjectPart(o.Status, status))
					if spec.Revoke == issued.Serial && status.State == state {
						must.Eq(t, "default/web-tls", spec.Certificate)
						must.Eq(t, structs.CertificateRevokeReasonCessationOfOperation, spec.Reason)
						name = o.Name
					}
				}
				return name != ""
			}),
			wait.Timeout(30*time.Second),
			wait.Gap(50*time.Millisecond),
		), must.Sprintf("waiting for a revocation order in the state %q", state))
		return name
	}

	// The key leaves the secret at once; the serial stays in the status
	// until the issuer has revoked the certificate.
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.org"})
	first := revocationOrder(structs.CertificateOrderPending)
	waiting := c.waitFor("default", "web-tls", "waiting for the revocation", func(obj *structs.CustomObject, st *structs.CertificateStatus) bool {
		reason, message := certificateFailure(obj.Status)
		return reason == "NamesNotAllowed" && strings.Contains(message, "waiting for the issuer") &&
			!objectConditionTrue(obj.Status, structs.CertificateConditionReady)
	})
	must.Eq(t, issued.Serial, waiting.Serial)
	secret, err := c.s.State().SecretByPath(nil, "default", "certs/web")
	must.NoError(t, err)
	must.NotNil(t, secret)
	for _, v := range secret.Versions {
		must.True(t, v.Destroyed, must.Sprintf("version %d is destroyed", v.Version))
	}
	must.Eq(t, 1, certificateOrderCount(t, c.s), must.Sprint("no order for a new certificate"))

	// The issuer fails the revocation: the certificate says so and does not
	// forget the serial; after the wait a new revocation order is placed.
	must.NoError(t, c.status("", structs.CertificateOrderKind, first, map[string]any{"state": "failed", "problem": "the CA is down"}))
	failed := c.waitFor("default", "web-tls", "the failed revocation", func(_ *structs.CustomObject, st *structs.CertificateStatus) bool {
		return st.Failures == 1 && st.RetryAt != ""
	})
	must.Eq(t, issued.Serial, failed.Serial)
	messages := c.eventMessages("default", structs.CertificateKind, "web-tls", "RevocationFailed")
	must.Len(t, 1, messages)
	must.StrContains(t, messages[0], "the CA is down")
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return c.get("", structs.CertificateOrderKind, first) == nil }),
		wait.Timeout(30*time.Second),
		wait.Gap(50*time.Millisecond),
	), must.Sprint("the failed order is removed"))

	// The wait is cut short.
	must.Wait(t, wait.InitialSuccess(
		wait.ErrorFunc(func() error {
			obj, _ := c.certificate("default", "web-tls")
			changed := obj.Copy()
			changed.Status["retryAt"] = time.Now().Add(-time.Second).UTC().Format(time.RFC3339)
			_, err := c.s.writeOperatorObject(changed)
			return err
		}),
		wait.Timeout(30*time.Second),
		wait.Gap(50*time.Millisecond),
	))
	second := revocationOrder(structs.CertificateOrderPending)
	must.NoError(t, c.status("", structs.CertificateOrderKind, second, map[string]any{"state": "valid"}))

	st := c.waitFor("default", "web-tls", "withdrawn", withdrawn)
	// What was held is an earlier certificate now, revoked, with the reason
	// of the order the issuer has done (review finding ATM-24g): an external
	// issuer has no list of its revocations, and the reason of this one can
	// still be raised by its serial (review finding ATM-24b).
	must.Eq(t, structs.CertificateStatus{Orders: st.Orders, Revoke: issued.Serial, Earlier: []*structs.CertificateEarlier{{
		Serial: issued.Serial, Issuer: "acme", NotAfter: issued.NotAfter, Revoked: true, RevokeReason: structs.CertificateRevokeReasonCessationOfOperation,
	}}}, *st)
	messages = c.eventMessages("default", structs.CertificateKind, "web-tls", "Withdrawn")
	must.Len(t, 1, messages)
	must.StrContains(t, messages[0], issued.Serial+" is revoked")
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return certificateOrderCount(t, c.s) == 0 }),
		wait.Timeout(30*time.Second),
		wait.Gap(50*time.Millisecond),
	), must.Sprint("the done revocation order is removed"))
}

// A namespace that gets the names of a certificate of an external issuer back
// gets a new certificate, whether the withdrawal still waits for the
// revocation or is over and left the secret with its versions destroyed
// (review finding ATM-21).
func TestCertificate_ExternalIssuerNamesBack(t *testing.T) {
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

	// issue plays the controller for the next issuance order.
	issue := func(previous string) *structs.CertificateStatus {
		t.Helper()
		ordered := c.waitFor("default", "web-tls", "an order", func(_ *structs.CustomObject, st *structs.CertificateStatus) bool { return st.Order != "" })
		order := c.get("", structs.CertificateOrderKind, ordered.Order)
		must.NotNil(t, order)
		orderSpec := &structs.CertificateOrderSpec{}
		must.NoError(t, structs.DecodeObjectPart(order.Spec, orderSpec))
		csr, err := parseWorkloadCSR(orderSpec.CSR)
		must.NoError(t, err)
		must.NoError(t, c.status("", structs.CertificateOrderKind, ordered.Order, map[string]any{
			"state": "valid", "chain": intermediate.sign(t, csr.PublicKey, csr.DNSNames, nil) + intermediate.pem}))
		issued := c.waitFor("default", "web-tls", "issued", func(obj *structs.CustomObject, st *structs.CertificateStatus) bool {
			return certReady(obj, st) && st.Serial != previous
		})
		must.Eq(t, []string{"cert", "key"}, c.secretItems("default", "certs/web").Keys())
		return issued
	}
	first := issue("")

	// Back while the withdrawal waits for the revocation.
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.org"})
	revocation := certificateRevokeOrderName("default", "web-tls", first.Serial)
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return c.get("", structs.CertificateOrderKind, revocation) != nil }),
		wait.Timeout(30*time.Second),
		wait.Gap(50*time.Millisecond),
	), must.Sprint("the revocation order is placed"))
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	second := issue(first.Serial)

	// Back after the withdrawal is over.
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.org"})
	revocation = certificateRevokeOrderName("default", "web-tls", second.Serial)
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return c.get("", structs.CertificateOrderKind, revocation) != nil }),
		wait.Timeout(30*time.Second),
		wait.Gap(50*time.Millisecond),
	), must.Sprint("the revocation order is placed"))
	must.NoError(t, c.status("", structs.CertificateOrderKind, revocation, map[string]any{"state": "valid"}))
	c.waitFor("default", "web-tls", "withdrawn", withdrawn)
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	issue(second.Serial)
}

// secretVersions soft deletes (structs.SecretOpDelete) or destroys
// (structs.SecretOpDestroy) versions of a secret, as a user can.
func (c *certTest) secretVersions(op, ns, path string, versions ...uint64) {
	c.t.Helper()
	args := &structs.SecretVersionsRequest{Path: path, Versions: versions}
	args.Namespace = ns
	args.Region = c.s.Region()
	_, _, err := c.s.secretVersionsOp(op, args)
	must.NoError(c.t, err)
}

// A certificate whose current version in the secret is soft deleted or
// destroyed is not held: nobody can read it, so a new certificate is issued
// into a new version at once (review finding ATM-24). The old one is not
// revoked: a deletion is no key compromise, and a soft delete can be undone.
func TestCertificate_DeletedVersion(t *testing.T) {
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

	for _, op := range []string{structs.SecretOpDelete, structs.SecretOpDestroy} {
		c.secretVersions(op, "default", "certs/web", held.SecretVersion)
		again := c.waitFor("default", "web-tls", "issued again after "+op, func(obj *structs.CustomObject, st *structs.CertificateStatus) bool {
			return certReady(obj, st) && st.Serial != held.Serial
		})
		must.Greater(t, held.SecretVersion, again.SecretVersion)
		items := c.secretItems("default", "certs/web")
		must.Eq(t, []string{"ca", "cert", "key"}, items.Keys())
		leaf, err := parseCertificatesPEM(items[structs.CertificateSecretItemCert])
		must.NoError(t, err)
		must.Eq(t, again.Serial, certificateSerial(leaf[0]))
		must.SliceNotContains(t, c.crlSerials(), held.Serial, must.Sprint("a deleted version is not revoked"))
		held = again
	}
}

// The same for the certificate of an external issuer: a new order is placed
// at once, and its key waits in a new version of the secret (review finding
// ATM-24). A version with the key of an open order that is deleted loses
// that key: the order is dropped (KeyLost), not waited for.
func TestCertificate_ExternalIssuerDeletedVersion(t *testing.T) {
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

	// ordered waits for an order other than the previous one, with the
	// condition Issuing (no certificate held) or Renewing.
	ordered := func(previous, condition string) *structs.CertificateStatus {
		t.Helper()
		return c.waitFor("default", "web-tls", "an order, "+condition, func(obj *structs.CustomObject, st *structs.CertificateStatus) bool {
			return st.Order != "" && st.Order != previous && objectConditionTrue(obj.Status, condition)
		})
	}
	// fulfil plays the controller for the order.
	fulfil := func(st *structs.CertificateStatus) {
		t.Helper()
		order := c.get("", structs.CertificateOrderKind, st.Order)
		must.NotNil(t, order)
		orderSpec := &structs.CertificateOrderSpec{}
		must.NoError(t, structs.DecodeObjectPart(order.Spec, orderSpec))
		csr, err := parseWorkloadCSR(orderSpec.CSR)
		must.NoError(t, err)
		must.NoError(t, c.status("", structs.CertificateOrderKind, st.Order, map[string]any{
			"state": "valid", "chain": intermediate.sign(t, csr.PublicKey, csr.DNSNames, nil) + intermediate.pem}))
	}
	issued := func(previous string) *structs.CertificateStatus {
		t.Helper()
		st := c.waitFor("default", "web-tls", "issued", func(obj *structs.CustomObject, st *structs.CertificateStatus) bool {
			return certReady(obj, st) && st.Serial != previous && st.Order == ""
		})
		must.Eq(t, []string{"cert", "key"}, c.secretItems("default", "certs/web").Keys())
		return st
	}
	first := ordered("", structs.CertificateConditionIssuing)
	fulfil(first)
	held := issued("")

	for _, op := range []string{structs.SecretOpDelete, structs.SecretOpDestroy} {
		c.secretVersions(op, "default", "certs/web", held.SecretVersion)
		next := ordered(first.Order, structs.CertificateConditionIssuing)
		must.Eq(t, []string{"next_key"}, c.secretItems("default", "certs/web").Keys(),
			must.Sprintf("the key of the order waits in a new version after %s", op))
		fulfil(next)
		again := issued(held.Serial)
		must.Greater(t, held.SecretVersion, again.SecretVersion)
		first, held = next, again
	}

	// The version with the key of an open order is deleted: the order is
	// dropped, and the next attempt orders anew.
	must.NoError(t, c.s.RPC("Certificate.Revoke", &structs.CertificateRevokeRequest{Name: "web-tls", WriteRequest: c.write("default")},
		&structs.CertificateRevokeResponse{}))
	waiting := ordered(first.Order, structs.CertificateConditionRenewing)
	secret, err := c.s.State().SecretByPath(nil, "default", "certs/web")
	must.NoError(t, err)
	c.secretVersions(structs.SecretOpDelete, "default", "certs/web", secret.CurrentVersion)
	fulfil(waiting)
	c.waitFor("default", "web-tls", "KeyLost", func(obj *structs.CustomObject, st *structs.CertificateStatus) bool {
		reason, _ := certificateFailure(obj.Status)
		return reason == "KeyLost" && st.Order == ""
	})
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

// revokeSerial revokes the certificate with a serial number of a Certificate
// object, as a user does; without a serial, the current one.
func (c *certTest) revokeSerial(ns, name, serial, reason string) (*structs.CertificateRevokeResponse, error) {
	resp := &structs.CertificateRevokeResponse{}
	err := c.s.RPC("Certificate.Revoke", &structs.CertificateRevokeRequest{
		Name: name, Serial: serial, Reason: reason, WriteRequest: c.write(ns)}, resp)
	return resp, err
}

// earlierCertificate returns the entry of a serial number among the earlier
// certificates of a status, or nil.
func earlierCertificate(st *structs.CertificateStatus, serial string) *structs.CertificateEarlier {
	i := slices.IndexFunc(st.Earlier, func(e *structs.CertificateEarlier) bool { return e.Serial == serial })
	if i < 0 {
		return nil
	}
	return st.Earlier[i]
}

// colonSerial writes a serial number the way openssl prints it: upper case,
// with colons between the octets.
func colonSerial(serial string) string {
	var parts []string
	for i := 0; i+2 <= len(serial); i += 2 {
		parts = append(parts, strings.ToUpper(serial[i:i+2]))
	}
	return strings.Join(parts, ":")
}

// A certificate that a newer one replaced while it was still valid stays
// revocable, by its serial number (review finding ATM-24b): the status keeps
// it, a revocation with the serial revokes exactly it, and neither issues a
// new certificate nor touches the secret. Its reason may be raised to
// keyCompromise afterwards (review finding ATM-25b). A serial that is not
// one of the object's is refused the same way whether or not it is the
// serial of something else.
func TestCertificate_RevokeEarlier(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	_, err := c.apply("", structs.CertificateIssuerKind, "internal", map[string]any{"builtin": map[string]any{}})
	must.NoError(t, err)
	_, err = c.apply("default", structs.CertificateKind, "web-tls", map[string]any{
		"issuer": "internal", "dns_names": []any{"web.example.com"}, "secret_path": "certs/web"})
	must.NoError(t, err)
	_, err = c.apply("default", structs.CertificateKind, "other-tls", map[string]any{
		"issuer": "internal", "dns_names": []any{"other.example.com"}, "secret_path": "certs/other"})
	must.NoError(t, err)
	first := c.waitFor("default", "web-tls", "issued", certReady)
	other := c.waitFor("default", "other-tls", "issued", certReady)
	must.SliceEmpty(t, first.Earlier)

	// The current version of the secret is deleted: a new certificate is
	// issued (review finding ATM-24), and the first one, still valid and
	// not revoked, is kept as an earlier certificate.
	c.secretVersions(structs.SecretOpDelete, "default", "certs/web", first.SecretVersion)
	second := c.waitFor("default", "web-tls", "issued again", func(obj *structs.CustomObject, st *structs.CertificateStatus) bool {
		return certReady(obj, st) && st.Serial != first.Serial
	})
	must.Eq(t, []*structs.CertificateEarlier{{
		Serial: first.Serial, Issuer: "internal", Authority: first.Authority, NotAfter: first.NotAfter,
	}}, second.Earlier, must.Sprint("the replaced certificate is kept in the status"))

	// The status is the servers': a user can't write a serial into it.
	err = c.status("default", structs.CertificateKind, "web-tls", map[string]any{
		"earlier": []any{map[string]any{"serial": other.Serial, "issuer": "internal", "authority": other.Authority, "notAfter": other.NotAfter}}})
	must.ErrorContains(t, err, "written by the servers only")

	// What is not a certificate of this object is refused, in the same
	// words: a serial nobody has, the current serial of another certificate,
	// and a revoked serial of another certificate in the list of the CA.
	_, changed, err := c.s.certificateCA.revoke(first.Authority, &structs.CertificateRevocation{
		Serial: "0a0b0c", Certificate: "default/other-tls", RevokedAt: time.Now().UTC(), NotAfter: time.Now().Add(time.Hour).UTC()})
	must.NoError(t, err)
	must.True(t, changed)
	var refusals []string
	for _, serial := range []string{"0d0e0f", other.Serial, "0a0b0c"} {
		_, err := c.revokeSerial("default", "web-tls", serial, structs.CertificateRevokeReasonKeyCompromise)
		must.Error(t, err, must.Sprint(serial))
		code, msg, ok := structs.CodeFromRPCCodedErr(err)
		must.True(t, ok, must.Sprint(err))
		must.Eq(t, 404, code, must.Sprint(serial))
		refusals = append(refusals, strings.ReplaceAll(msg, serial, "<serial>"))
	}
	must.Eq(t, refusals[0], refusals[1], must.Sprint("the refusal does not tell a serial of another certificate from an unknown one"))
	must.Eq(t, refusals[0], refusals[2])
	must.SliceNotContains(t, c.crlSerials(), other.Serial)
	must.Eq(t, 0, c.crlReasons()["0a0b0c"], must.Sprint("the reason of the revocation of another certificate is not raised"))
	for _, serial := range []string{"xyz", "0x", strings.Repeat("ab", 40)} {
		_, err := c.revokeSerial("default", "web-tls", serial, "")
		must.Error(t, err, must.Sprint(serial))
		code, _, _ := structs.CodeFromRPCCodedErr(err)
		must.Eq(t, 400, code, must.Sprint(serial))
	}
	must.SliceNotContains(t, c.crlSerials(), first.Serial)
	must.SliceNotContains(t, c.crlSerials(), second.Serial)

	// The earlier certificate is revoked by its serial, given the way a
	// tool prints it. The current one stays, and so does the secret.
	secretBefore, err := c.s.State().SecretByPath(nil, "default", "certs/web")
	must.NoError(t, err)
	resp, err := c.revokeSerial("default", "web-tls", colonSerial(first.Serial), structs.CertificateRevokeReasonSuperseded)
	must.NoError(t, err)
	must.Eq(t, first.Serial, resp.Serial, must.Sprint("the certificate with the serial is the one revoked"))
	must.True(t, resp.Earlier)
	must.Eq(t, 4, c.crlReasons()[first.Serial], must.Sprint("the earlier certificate is in the revocation list"))
	must.SliceNotContains(t, c.crlSerials(), second.Serial, must.Sprint("the current certificate is not revoked"))
	_, st := c.certificate("default", "web-tls")
	must.Eq(t, []*structs.CertificateEarlier{{
		Serial: first.Serial, Issuer: "internal", Authority: first.Authority, NotAfter: first.NotAfter, Revoked: true,
	}}, st.Earlier, must.Sprint("the entry stays, marked revoked"))
	// The operator runs on the change of the status within its settle
	// time: nothing new is issued.
	time.Sleep(2 * time.Second)
	_, st = c.certificate("default", "web-tls")
	must.Eq(t, second.Serial, st.Serial, must.Sprint("the revocation of an earlier certificate issues no new one"))
	must.Eq(t, "", st.Revoke)
	secretAfter, err := c.s.State().SecretByPath(nil, "default", "certs/web")
	must.NoError(t, err)
	must.Eq(t, secretBefore.ModifyIndex, secretAfter.ModifyIndex, must.Sprint("the secret is not touched"))
	messages := c.eventMessages("default", structs.CertificateKind, "web-tls", "Revoked")
	must.Len(t, 1, messages)
	must.StrContains(t, messages[0], first.Serial)
	must.StrContains(t, messages[0], "earlier")

	// Its reason is raised to keyCompromise, and by nothing else.
	for _, reason := range []string{structs.CertificateRevokeReasonAffiliationChanged, ""} {
		_, err = c.revokeSerial("default", "web-tls", first.Serial, reason)
		must.NoError(t, err)
		must.Eq(t, 4, c.crlReasons()[first.Serial], must.Sprint(reason))
	}
	resp, err = c.revokeSerial("default", "web-tls", first.Serial, structs.CertificateRevokeReasonKeyCompromise)
	must.NoError(t, err)
	must.Eq(t, first.Serial, resp.Serial)
	must.True(t, resp.Earlier)
	must.Eq(t, 1, c.crlReasons()[first.Serial], must.Sprint("the reason of the earlier certificate is raised to keyCompromise"))
	raisedMessages := c.eventMessages("default", structs.CertificateKind, "web-tls", "RevocationReason")
	must.Len(t, 1, raisedMessages)
	must.StrContains(t, raisedMessages[0], first.Serial)

	// With the serial of the current certificate the revocation is what it
	// is without a serial: a new certificate is issued, and the revoked one
	// becomes an earlier certificate, marked revoked.
	resp, err = c.revokeSerial("default", "web-tls", strings.ToUpper(second.Serial), "")
	must.NoError(t, err)
	must.Eq(t, second.Serial, resp.Serial)
	must.False(t, resp.Earlier)
	third := c.waitFor("default", "web-tls", "issued after the revocation", func(obj *structs.CustomObject, st *structs.CertificateStatus) bool {
		return certReady(obj, st) && st.Serial != second.Serial && st.Revoke == ""
	})
	must.Len(t, 2, third.Earlier)
	must.Eq(t, &structs.CertificateEarlier{
		Serial: second.Serial, Issuer: "internal", Authority: second.Authority, NotAfter: second.NotAfter, Revoked: true,
	}, earlierCertificate(third, second.Serial))
	must.Eq(t, 0, c.crlReasons()[second.Serial])

	// After the reissue the reason of that one is raised by its serial too
	// (the limit review finding ATM-25b was left with).
	resp, err = c.revokeSerial("default", "web-tls", second.Serial, structs.CertificateRevokeReasonKeyCompromise)
	must.NoError(t, err)
	must.True(t, resp.Earlier)
	must.Eq(t, 1, c.crlReasons()[second.Serial], must.Sprint("the reason of a revoked, replaced certificate is raised by serial"))
	_, st = c.certificate("default", "web-tls")
	must.Eq(t, third.Serial, st.Serial)

	// A revoked certificate of the object that the status no longer lists
	// is found in the list of its CA: only its reason can still change.
	_, changed, err = c.s.certificateCA.revoke(third.Authority, &structs.CertificateRevocation{
		Serial: "0102ab", Certificate: "default/web-tls", RevokedAt: time.Now().UTC(), NotAfter: time.Now().Add(time.Hour).UTC()})
	must.NoError(t, err)
	must.True(t, changed)
	_, err = c.revokeSerial("default", "web-tls", "0102ab", structs.CertificateRevokeReasonSuperseded)
	must.NoError(t, err)
	must.Eq(t, 0, c.crlReasons()["0102ab"])
	resp, err = c.revokeSerial("default", "web-tls", "01:02:AB", structs.CertificateRevokeReasonKeyCompromise)
	must.NoError(t, err)
	must.Eq(t, "0102ab", resp.Serial)
	must.True(t, resp.Earlier)
	must.Eq(t, 1, c.crlReasons()["0102ab"])

	// Without a serial the revocation is of the current certificate, as
	// before.
	resp, err = c.revokeSerial("default", "web-tls", "", "")
	must.NoError(t, err)
	must.Eq(t, third.Serial, resp.Serial)
	must.False(t, resp.Earlier)
}

// setEarlier writes the earlier certificates of a certificate into its
// status, as the operator does.
func (c *certTest) setEarlier(ns, name string, change func([]*structs.CertificateEarlier) []*structs.CertificateEarlier) {
	c.t.Helper()
	must.Wait(c.t, wait.InitialSuccess(
		wait.ErrorFunc(func() error {
			obj, st := c.certificate(ns, name)
			part, err := structs.EncodeObjectPart(&structs.CertificateStatus{Earlier: change(st.Earlier)})
			if err != nil {
				return err
			}
			changed := obj.Copy()
			delete(changed.Status, "earlier")
			if list, ok := part["earlier"]; ok {
				changed.Status["earlier"] = list
			}
			_, err = c.s.writeOperatorObject(changed)
			return err
		}),
		wait.Timeout(10*time.Second),
		wait.Gap(50*time.Millisecond),
	))
}

// The earlier certificates of a status are bounded: one that has expired is
// dropped, and beyond structs.CertificateEarlierMax the oldest one goes, with
// an event that names its serial, since it can no longer be revoked by it
// (review finding ATM-24b).
func TestCertificate_EarlierBounded(t *testing.T) {
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

	serialOf := func(i int) string { return "ee00" + strings.Repeat(string("0123456789abcdef"[i]), 4) }
	notAfter := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	c.setEarlier("default", "web-tls", func([]*structs.CertificateEarlier) []*structs.CertificateEarlier {
		var full []*structs.CertificateEarlier
		for i := range structs.CertificateEarlierMax {
			full = append(full, &structs.CertificateEarlier{Serial: serialOf(i), Issuer: "internal", Authority: held.Authority, NotAfter: notAfter})
		}
		return full
	})

	// One more: the oldest is dropped, and an event says which.
	_, err = c.revokeSerial("default", "web-tls", "", "")
	must.NoError(t, err)
	next := c.waitFor("default", "web-tls", "issued after the revocation", func(obj *structs.CustomObject, st *structs.CertificateStatus) bool {
		return certReady(obj, st) && st.Serial != held.Serial
	})
	must.Len(t, structs.CertificateEarlierMax, next.Earlier)
	must.Nil(t, earlierCertificate(next, serialOf(0)), must.Sprint("the oldest earlier certificate is dropped"))
	must.Eq(t, serialOf(1), next.Earlier[0].Serial)
	must.Eq(t, held.Serial, next.Earlier[structs.CertificateEarlierMax-1].Serial)
	dropped := c.eventMessages("default", structs.CertificateKind, "web-tls", "EarlierCertificateDropped")
	must.Len(t, 1, dropped)
	must.StrContains(t, dropped[0], serialOf(0))
	_, err = c.revokeSerial("default", "web-tls", serialOf(0), "")
	must.Error(t, err, must.Sprint("a dropped serial is no longer revocable by serial"))

	// An entry whose certificate has expired is dropped by the operator,
	// and is not revocable meanwhile either.
	c.setEarlier("default", "web-tls", func(list []*structs.CertificateEarlier) []*structs.CertificateEarlier {
		out := make([]*structs.CertificateEarlier, len(list))
		for i, e := range list {
			cp := *e
			if cp.Serial == serialOf(3) {
				cp.NotAfter = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
			}
			out[i] = &cp
		}
		return out
	})
	_, err = c.revokeSerial("default", "web-tls", serialOf(3), "")
	must.Error(t, err)
	pruned := c.waitFor("default", "web-tls", "the expired entry dropped", func(_ *structs.CustomObject, st *structs.CertificateStatus) bool {
		return earlierCertificate(st, serialOf(3)) == nil
	})
	must.Len(t, structs.CertificateEarlierMax-1, pruned.Earlier)
	must.Eq(t, next.Serial, pruned.Serial)
	must.SliceNotContains(t, c.crlSerials(), serialOf(3))
}

// An earlier certificate of an external issuer is revoked by its serial
// through a revocation order of its own, which names the issuer and the
// serial, all its controller needs (review finding ATM-24b); nothing new is
// ordered for the current certificate. The order that is placed marks
// nothing: the certificate is revoked when the issuer has done the order
// (review finding ATM-24g). A revoked one whose order is done gets a new
// order when its reason is raised to keyCompromise (review finding ATM-25b):
// an external issuer has no list to find it in, so the status keeps the
// revoked certificates too.
func TestCertificate_ExternalIssuerRevokeEarlier(t *testing.T) {
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

	// issue plays the controller for the next order of the certificate.
	issue := func(previousOrder, previousSerial string) *structs.CertificateStatus {
		t.Helper()
		ordered := c.waitFor("default", "web-tls", "an order", func(_ *structs.CustomObject, st *structs.CertificateStatus) bool {
			return st.Order != "" && st.Order != previousOrder
		})
		order := c.get("", structs.CertificateOrderKind, ordered.Order)
		must.NotNil(t, order)
		orderSpec := &structs.CertificateOrderSpec{}
		must.NoError(t, structs.DecodeObjectPart(order.Spec, orderSpec))
		csr, err := parseWorkloadCSR(orderSpec.CSR)
		must.NoError(t, err)
		must.NoError(t, c.status("", structs.CertificateOrderKind, ordered.Order, map[string]any{
			"state": "valid", "chain": intermediate.sign(t, csr.PublicKey, csr.DNSNames, nil) + intermediate.pem}))
		return c.waitFor("default", "web-tls", "issued", func(obj *structs.CustomObject, st *structs.CertificateStatus) bool {
			return certReady(obj, st) && st.Serial != previousSerial && st.Order == ""
		})
	}
	// revocation returns the revocation order of a serial, or nil.
	revocation := func(serial string) *structs.CertificateOrderSpec {
		t.Helper()
		o := c.get("", structs.CertificateOrderKind, certificateRevokeOrderName("default", "web-tls", serial))
		if o == nil {
			return nil
		}
		spec := &structs.CertificateOrderSpec{}
		must.NoError(t, structs.DecodeObjectPart(o.Spec, spec))
		return spec
	}
	// finish plays the controller for the revocation order of a serial; the
	// operator then marks the certificate revoked, with the reason of the
	// order, and removes the order.
	finish := func(serial, reason string) {
		t.Helper()
		name := certificateRevokeOrderName("default", "web-tls", serial)
		must.NoError(t, c.status("", structs.CertificateOrderKind, name, map[string]any{"state": "valid"}))
		c.waitFor("default", "web-tls", "the earlier certificate revoked", func(_ *structs.CustomObject, st *structs.CertificateStatus) bool {
			e := earlierCertificate(st, serial)
			return e != nil && e.Revoked && e.RevokeReason == reason && e.Revoking == ""
		})
		must.Wait(t, wait.InitialSuccess(
			wait.BoolFunc(func() bool { return c.get("", structs.CertificateOrderKind, name) == nil }),
			wait.Timeout(30*time.Second), wait.Gap(50*time.Millisecond)))
	}

	first := issue("", "")
	must.Eq(t, uint64(1), first.Orders)

	// The current version of the secret is deleted: the second certificate
	// replaces the first, which is neither expired nor revoked.
	c.secretVersions(structs.SecretOpDelete, "default", "certs/web", first.SecretVersion)
	second := issue("default.web-tls.1", first.Serial)
	must.Eq(t, []*structs.CertificateEarlier{{Serial: first.Serial, Issuer: "acme", NotAfter: first.NotAfter}}, second.Earlier)
	must.Nil(t, revocation(first.Serial))

	// Revoked by its serial: an order for the issuer with the serial and
	// the reason, and nothing ordered for the current certificate.
	resp, err := c.revokeSerial("default", "web-tls", first.Serial, structs.CertificateRevokeReasonSuperseded)
	must.NoError(t, err)
	must.Eq(t, first.Serial, resp.Serial)
	must.True(t, resp.Earlier)
	must.Eq(t, &structs.CertificateOrderSpec{
		Issuer: "acme", Certificate: "default/web-tls", Revoke: first.Serial, Reason: structs.CertificateRevokeReasonSuperseded,
	}, revocation(first.Serial), must.Sprint("the revocation order names the issuer and the serial of the earlier certificate"))
	must.Nil(t, revocation(second.Serial))
	must.True(t, resp.Pending)
	_, st := c.certificate("default", "web-tls")
	must.Eq(t, &structs.CertificateEarlier{Serial: first.Serial, Issuer: "acme", NotAfter: first.NotAfter,
		Revoking: structs.CertificateRevokeReasonSuperseded}, earlierCertificate(st, first.Serial),
		must.Sprint("the order that is placed revokes nothing yet"))

	// While the order is open its reason is raised; a done order is gone,
	// and the raise places a new one.
	_, err = c.revokeSerial("default", "web-tls", first.Serial, structs.CertificateRevokeReasonAffiliationChanged)
	must.NoError(t, err)
	must.Eq(t, structs.CertificateRevokeReasonSuperseded, revocation(first.Serial).Reason)
	_, err = c.revokeSerial("default", "web-tls", first.Serial, structs.CertificateRevokeReasonKeyCompromise)
	must.NoError(t, err)
	must.Eq(t, structs.CertificateRevokeReasonKeyCompromise, revocation(first.Serial).Reason)
	finish(first.Serial, structs.CertificateRevokeReasonKeyCompromise)
	_, err = c.revokeSerial("default", "web-tls", first.Serial, structs.CertificateRevokeReasonSuperseded)
	must.NoError(t, err)
	must.Nil(t, revocation(first.Serial), must.Sprint("another reason for a revoked certificate tells the issuer nothing"))
	_, err = c.revokeSerial("default", "web-tls", first.Serial, structs.CertificateRevokeReasonKeyCompromise)
	must.NoError(t, err)
	must.Eq(t, &structs.CertificateOrderSpec{
		Issuer: "acme", Certificate: "default/web-tls", Revoke: first.Serial, Reason: structs.CertificateRevokeReasonKeyCompromise,
	}, revocation(first.Serial))
	_, st = c.certificate("default", "web-tls")
	must.Eq(t, &structs.CertificateEarlier{Serial: first.Serial, Issuer: "acme", NotAfter: first.NotAfter, Revoked: true,
		RevokeReason: structs.CertificateRevokeReasonKeyCompromise, Revoking: structs.CertificateRevokeReasonKeyCompromise},
		earlierCertificate(st, first.Serial), must.Sprint("the order of the raise is open"))
	finish(first.Serial, structs.CertificateRevokeReasonKeyCompromise)

	_, st = c.certificate("default", "web-tls")
	must.Eq(t, second.Serial, st.Serial, must.Sprint("the current certificate stays"))
	must.Eq(t, "", st.Order)
	must.Eq(t, second.Orders, st.Orders, must.Sprint("nothing is ordered for the current certificate"))

	// The current certificate is revoked and replaced: it is kept with its
	// revocation under way, marked revoked when the issuer has done the
	// order, and its reason can be raised by its serial after that.
	resp, err = c.revokeSerial("default", "web-tls", "", "")
	must.NoError(t, err)
	must.Eq(t, second.Serial, resp.Serial)
	must.False(t, resp.Earlier)
	third := issue("default.web-tls.2", second.Serial)
	must.Eq(t, &structs.CertificateEarlier{Serial: second.Serial, Issuer: "acme", NotAfter: second.NotAfter,
		Revoking: structs.CertificateRevokeReasonUnspecified}, earlierCertificate(third, second.Serial))
	finish(second.Serial, structs.CertificateRevokeReasonUnspecified)
	resp, err = c.revokeSerial("default", "web-tls", second.Serial, structs.CertificateRevokeReasonKeyCompromise)
	must.NoError(t, err)
	must.True(t, resp.Earlier)
	must.Eq(t, &structs.CertificateOrderSpec{
		Issuer: "acme", Certificate: "default/web-tls", Revoke: second.Serial, Reason: structs.CertificateRevokeReasonKeyCompromise,
	}, revocation(second.Serial), must.Sprint("the reason of a revoked, replaced certificate of an external issuer is raised by serial"))

	// An earlier certificate whose issuer is gone can't be revoked, and the
	// answer says so.
	must.NoError(t, c.delete("", structs.CertificateIssuerKind, "acme"))
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return c.get("", structs.CertificateIssuerKind, "acme") == nil }),
		wait.Timeout(30*time.Second), wait.Gap(50*time.Millisecond)))
	_, err = c.revokeSerial("default", "web-tls", first.Serial, structs.CertificateRevokeReasonKeyCompromise)
	must.ErrorContains(t, err, "is gone")
	// It was revoked before the issuer went: what is refused is the new
	// reason, and the answer does not call a revoked certificate valid.
	must.ErrorContains(t, err, "the certificate with the serial "+first.Serial+" is revoked already")
	must.ErrorContains(t, err, "is not changed to keyCompromise")
	must.StrNotContains(t, err.Error(), "is not revoked")
	must.StrNotContains(t, err.Error(), "stays valid")
}

// A withdrawal takes back the earlier certificates too (review finding
// ATM-24e): a certificate that a renewal, a changed spec or a deleted version
// of the secret replaced is as valid for the names the namespace lost as the
// current one. With the built-in issuer every earlier certificate that is
// still valid and not revoked is listed in the revocation list at once, with
// the reason cessationOfOperation, and the event names it. One a user
// revoked before stays as it is.
func TestCertificate_LostNamesEarlier(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	_, err := c.apply("", structs.CertificateIssuerKind, "internal", map[string]any{"builtin": map[string]any{}})
	must.NoError(t, err)
	_, err = c.apply("default", structs.CertificateKind, "web-tls", map[string]any{
		"issuer": "internal", "dns_names": []any{"web.example.com"}, "secret_path": "certs/web"})
	must.NoError(t, err)

	// Three certificates: the first replaced and revoked by the user, the
	// second replaced and not revoked, the third held.
	replace := func(held *structs.CertificateStatus) *structs.CertificateStatus {
		t.Helper()
		c.secretVersions(structs.SecretOpDelete, "default", "certs/web", held.SecretVersion)
		return c.waitFor("default", "web-tls", "issued again", func(obj *structs.CustomObject, st *structs.CertificateStatus) bool {
			return certReady(obj, st) && st.Serial != held.Serial
		})
	}
	first := c.waitFor("default", "web-tls", "issued", certReady)
	second := replace(first)
	_, err = c.revokeSerial("default", "web-tls", first.Serial, structs.CertificateRevokeReasonSuperseded)
	must.NoError(t, err)
	third := replace(second)
	must.Len(t, 2, third.Earlier)
	must.SliceNotContains(t, c.crlSerials(), second.Serial)

	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.org"})
	st := c.waitFor("default", "web-tls", "withdrawn", withdrawn)
	must.Eq(t, structs.CertificateStatus{Orders: st.Orders, Revoke: third.Serial, Earlier: []*structs.CertificateEarlier{
		{Serial: first.Serial, Issuer: "internal", Authority: first.Authority, NotAfter: first.NotAfter, Revoked: true},
		{Serial: second.Serial, Issuer: "internal", Authority: second.Authority, NotAfter: second.NotAfter, Revoked: true},
		{Serial: third.Serial, Issuer: "internal", Authority: third.Authority, NotAfter: third.NotAfter, Revoked: true},
	}}, *st, must.Sprint("the earlier certificate that was still valid is marked revoked"))
	reasons := c.crlReasons()
	must.Eq(t, 5, reasons[third.Serial])
	must.MapContainsKey(t, reasons, second.Serial, must.Sprint("the earlier certificate is in the revocation list"))
	must.Eq(t, 5, reasons[second.Serial], must.Sprint("with the reason cessationOfOperation"))
	must.Eq(t, 4, reasons[first.Serial], must.Sprint("what the user revoked keeps its reason"))

	messages := c.eventMessages("default", structs.CertificateKind, "web-tls", "Withdrawn")
	must.Len(t, 1, messages)
	must.StrContains(t, messages[0], third.Serial+" is revoked")
	must.StrContains(t, messages[0], "the earlier certificate with the serial "+second.Serial+" is revoked")
	must.StrNotContains(t, messages[0], first.Serial)

	// The next passes say nothing again.
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.net"})
	time.Sleep(2 * time.Second)
	must.Len(t, 1, c.eventMessages("default", structs.CertificateKind, "web-tls", "Withdrawn"))
	_, after := c.certificate("default", "web-tls")
	must.Eq(t, st.Earlier, after.Earlier)
}

// With the list of the earlier certificates full, a withdrawal revokes every
// one of them before the oldest leaves the list for the certificate that was
// held (review finding ATM-24e).
func TestCertificate_LostNamesEarlierFull(t *testing.T) {
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

	serialOf := func(i int) string { return "ee00" + strings.Repeat(string("0123456789abcdef"[i]), 4) }
	notAfter := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	c.setEarlier("default", "web-tls", func([]*structs.CertificateEarlier) []*structs.CertificateEarlier {
		var full []*structs.CertificateEarlier
		for i := range structs.CertificateEarlierMax {
			full = append(full, &structs.CertificateEarlier{Serial: serialOf(i), Issuer: "internal", Authority: held.Authority, NotAfter: notAfter})
		}
		return full
	})

	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.org"})
	st := c.waitFor("default", "web-tls", "withdrawn", withdrawn)
	must.Len(t, structs.CertificateEarlierMax, st.Earlier)
	must.Eq(t, held.Serial, st.Earlier[structs.CertificateEarlierMax-1].Serial)
	for _, e := range st.Earlier {
		must.True(t, e.Revoked, must.Sprint(e.Serial))
	}
	revoked := c.crlSerials()
	must.SliceContains(t, revoked, held.Serial)
	for i := range structs.CertificateEarlierMax {
		must.SliceContains(t, revoked, serialOf(i), must.Sprintf("earlier certificate %d is revoked, the dropped one too", i))
	}
}

// acmeTest plays the controller of the external issuer "acme" for the
// certificate default/web-tls with the secret certs/web.
type acmeTest struct {
	*certTest
	root, intermediate *testCA
}

func newAcmeTest(t *testing.T, c *certTest) *acmeTest {
	t.Helper()
	a := &acmeTest{certTest: c, root: newTestCA(t, "acme root", nil)}
	a.intermediate = newTestCA(t, "acme issuing", a.root)
	a.issuer()
	_, err := c.apply("default", structs.CertificateKind, "web-tls", map[string]any{
		"issuer": "acme", "dns_names": []any{"web.example.com"}, "secret_path": "certs/web"})
	must.NoError(t, err)
	return a
}

// issuer creates the issuer, with a heartbeat of its controller.
func (a *acmeTest) issuer() {
	a.t.Helper()
	_, err := a.apply("", structs.CertificateIssuerKind, "acme", map[string]any{"external": map[string]any{"controller": "test"}})
	must.NoError(a.t, err)
	must.NoError(a.t, a.status("", structs.CertificateIssuerKind, "acme", map[string]any{
		"heartbeat": time.Now().UTC().Format(time.RFC3339), "roots": a.root.pem}))
}

// issue fulfils the next issuance order and returns the status with the
// certificate that takes the place of the one with the previous serial.
func (a *acmeTest) issue(previous string) *structs.CertificateStatus {
	a.t.Helper()
	ordered := a.waitFor("default", "web-tls", "an order", func(_ *structs.CustomObject, st *structs.CertificateStatus) bool { return st.Order != "" })
	order := a.get("", structs.CertificateOrderKind, ordered.Order)
	must.NotNil(a.t, order)
	orderSpec := &structs.CertificateOrderSpec{}
	must.NoError(a.t, structs.DecodeObjectPart(order.Spec, orderSpec))
	csr, err := parseWorkloadCSR(orderSpec.CSR)
	must.NoError(a.t, err)
	must.NoError(a.t, a.status("", structs.CertificateOrderKind, ordered.Order, map[string]any{
		"state": "valid", "chain": a.intermediate.sign(a.t, csr.PublicKey, csr.DNSNames, nil) + a.intermediate.pem}))
	return a.waitFor("default", "web-tls", "issued", func(obj *structs.CustomObject, st *structs.CertificateStatus) bool {
		return certReady(obj, st) && st.Serial != previous && st.Order == ""
	})
}

// replaced has the held certificate replaced by a new one while it is valid:
// the version of the secret with it is deleted.
func (a *acmeTest) replaced(held *structs.CertificateStatus) *structs.CertificateStatus {
	a.t.Helper()
	a.secretVersions(structs.SecretOpDelete, "default", "certs/web", held.SecretVersion)
	return a.issue(held.Serial)
}

// revocation returns the revocation order of a serial, or nil.
func (a *acmeTest) revocation(serial string) *structs.CustomObject {
	a.t.Helper()
	return a.get("", structs.CertificateOrderKind, certificateRevokeOrderName("default", "web-tls", serial))
}

// revocationPlaced waits for the revocation order of a serial, which a
// withdrawal places with the reason cessationOfOperation.
func (a *acmeTest) revocationPlaced(serial string) {
	a.t.Helper()
	var order *structs.CustomObject
	must.Wait(a.t, wait.InitialSuccess(
		wait.BoolFunc(func() bool {
			order = a.revocation(serial)
			return order != nil
		}),
		wait.Timeout(30*time.Second), wait.Gap(50*time.Millisecond),
	), must.Sprintf("waiting for the revocation order of the serial %s", serial))
	spec := &structs.CertificateOrderSpec{}
	must.NoError(a.t, structs.DecodeObjectPart(order.Spec, spec))
	must.Eq(a.t, &structs.CertificateOrderSpec{
		Issuer: "acme", Certificate: "default/web-tls", Revoke: serial, Reason: structs.CertificateRevokeReasonCessationOfOperation,
	}, spec)
}

// revocationDone ends the revocation order of a serial as the controller
// does, and waits until the operator has removed it.
func (a *acmeTest) revocationDone(serial string, status map[string]any) {
	a.t.Helper()
	name := certificateRevokeOrderName("default", "web-tls", serial)
	must.NoError(a.t, a.status("", structs.CertificateOrderKind, name, status))
	must.Wait(a.t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return a.get("", structs.CertificateOrderKind, name) == nil }),
		wait.Timeout(30*time.Second), wait.Gap(50*time.Millisecond),
	), must.Sprintf("the finished revocation order of the serial %s is removed", serial))
}

// containing returns the messages that contain a text.
func containing(messages []string, text string) []string {
	var out []string
	for _, m := range messages {
		if strings.Contains(m, text) {
			out = append(out, m)
		}
	}
	return out
}

// With an external issuer a withdrawal places a revocation order for every
// earlier certificate that is still valid and not revoked, as for the
// current one (review finding ATM-24e). Each waits for its own order: one
// that fails is counted and placed again after its own wait, and holds up
// neither the others nor the certificate that was held. An earlier
// certificate is marked revoked when its issuer has revoked it.
func TestCertificate_ExternalIssuerLostNamesEarlier(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	a := newAcmeTest(t, c)
	first := a.issue("")
	second := a.replaced(first)
	must.Eq(t, []*structs.CertificateEarlier{{Serial: first.Serial, Issuer: "acme", NotAfter: first.NotAfter}}, second.Earlier)

	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.org"})
	a.revocationPlaced(second.Serial)
	a.revocationPlaced(first.Serial)
	_, st := c.certificate("default", "web-tls")
	must.False(t, earlierCertificate(st, first.Serial).Revoked, must.Sprint("an order that is placed revokes nothing yet"))

	// The issuer fails the revocation of the earlier certificate.
	a.revocationDone(first.Serial, map[string]any{"state": "failed", "problem": "the CA is down"})
	failed := c.waitFor("default", "web-tls", "the failed revocation of the earlier certificate", func(_ *structs.CustomObject, st *structs.CertificateStatus) bool {
		e := earlierCertificate(st, first.Serial)
		return e != nil && e.Failures == 1 && e.RetryAt != ""
	})
	must.False(t, earlierCertificate(failed, first.Serial).Revoked)
	must.Eq(t, second.Serial, failed.Serial, must.Sprint("the current certificate still waits for its own order"))
	must.Eq(t, uint64(0), failed.Failures, must.Sprint("the failure is the earlier certificate's"))
	messages := c.eventMessages("default", structs.CertificateKind, "web-tls", "RevocationFailed")
	must.Len(t, 1, messages)
	must.StrContains(t, messages[0], first.Serial)
	must.StrContains(t, messages[0], "the CA is down")

	// The current certificate is revoked meanwhile: the wait of the earlier
	// one does not hold it up.
	a.revocationDone(second.Serial, map[string]any{"state": "valid"})
	st = c.waitFor("default", "web-tls", "withdrawn", withdrawn)
	must.Eq(t, &structs.CertificateEarlier{Serial: second.Serial, Issuer: "acme", NotAfter: second.NotAfter, Revoked: true, RevokeReason: structs.CertificateRevokeReasonCessationOfOperation},
		earlierCertificate(st, second.Serial))
	waiting := earlierCertificate(st, first.Serial)
	must.False(t, waiting.Revoked)
	must.Eq(t, uint64(1), waiting.Failures)
	must.Nil(t, a.revocation(first.Serial), must.Sprint("no new order before the wait is over"))
	messages = c.eventMessages("default", structs.CertificateKind, "web-tls", "Withdrawn")
	must.Len(t, 1, messages)
	must.StrContains(t, messages[0], second.Serial+" is revoked")
	must.StrNotContains(t, messages[0], first.Serial)

	// The wait of the earlier certificate is cut short: a new order, and
	// with it done the certificate is marked revoked, which an event says.
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
	a.revocationDone(first.Serial, map[string]any{"state": "valid"})
	st = c.waitFor("default", "web-tls", "the earlier certificate revoked", func(_ *structs.CustomObject, st *structs.CertificateStatus) bool {
		e := earlierCertificate(st, first.Serial)
		return e != nil && e.Revoked
	})
	must.Eq(t, structs.CertificateStatus{Orders: st.Orders, Revoke: second.Serial, Earlier: []*structs.CertificateEarlier{
		{Serial: first.Serial, Issuer: "acme", NotAfter: first.NotAfter, Revoked: true, RevokeReason: structs.CertificateRevokeReasonCessationOfOperation},
		{Serial: second.Serial, Issuer: "acme", NotAfter: second.NotAfter, Revoked: true, RevokeReason: structs.CertificateRevokeReasonCessationOfOperation},
	}}, *st, must.Sprint("both are revoked, and nothing is left of the failure"))
	messages = c.eventMessages("default", structs.CertificateKind, "web-tls", "Withdrawn")
	must.Len(t, 2, messages)
	must.Len(t, 1, containing(messages, "the earlier certificate with the serial "+first.Serial+" is revoked"))

	// Nothing more is ordered or said.
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.net"})
	time.Sleep(2 * time.Second)
	must.Eq(t, 0, certificateOrderCount(t, c.s))
	must.Len(t, 2, c.eventMessages("default", structs.CertificateKind, "web-tls", "Withdrawn"))
}

// An earlier certificate that no issuer is left to revoke stays unrevoked
// and unmarked in a withdrawal (review finding ATM-24e): the event names its
// serial and says why, once, and the status keeps the reason. When there is
// an issuer for it again, it is revoked.
func TestCertificate_LostNamesEarlierNotRevocable(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	_, err := c.apply("", structs.CertificateIssuerKind, "internal", map[string]any{"builtin": map[string]any{}})
	must.NoError(t, err)
	a := newAcmeTest(t, c)
	first := a.issue("")

	// The certificate moves to the built-in issuer, and the external one
	// that signed the first certificate goes.
	_, err = c.apply("default", structs.CertificateKind, "web-tls", map[string]any{
		"issuer": "internal", "dns_names": []any{"web.example.com"}, "secret_path": "certs/web"})
	must.NoError(t, err)
	second := c.waitFor("default", "web-tls", "issued by the built-in issuer", func(obj *structs.CustomObject, st *structs.CertificateStatus) bool {
		return certReady(obj, st) && st.Serial != first.Serial && st.Authority != ""
	})
	must.Eq(t, []*structs.CertificateEarlier{{Serial: first.Serial, Issuer: "acme", NotAfter: first.NotAfter}}, second.Earlier)
	must.NoError(t, c.delete("", structs.CertificateIssuerKind, "acme"))
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return c.get("", structs.CertificateIssuerKind, "acme") == nil }),
		wait.Timeout(30*time.Second), wait.Gap(50*time.Millisecond)))

	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.org"})
	st := c.waitFor("default", "web-tls", "withdrawn", withdrawn)
	must.SliceContains(t, c.crlSerials(), second.Serial)
	must.True(t, earlierCertificate(st, second.Serial).Revoked)
	left := earlierCertificate(st, first.Serial)
	must.NotNil(t, left)
	must.False(t, left.Revoked, must.Sprint("what nobody revoked is not marked revoked"))
	must.StrContains(t, left.NotRevocable, `the issuer "acme" is gone`)
	must.Eq(t, 0, certificateOrderCount(t, c.s))
	messages := c.eventMessages("default", structs.CertificateKind, "web-tls", "Withdrawn")
	must.Len(t, 1, messages)
	must.StrContains(t, messages[0], second.Serial+" is revoked")
	must.StrContains(t, messages[0], "the earlier certificate with the serial "+first.Serial+" is not revoked")
	must.StrContains(t, messages[0], `the issuer "acme" is gone`)

	// The next passes do not repeat the event; the Failed condition keeps
	// saying it.
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.net"})
	time.Sleep(2 * time.Second)
	must.Len(t, 1, c.eventMessages("default", structs.CertificateKind, "web-tls", "Withdrawn"))
	obj, _ := c.certificate("default", "web-tls")
	_, message := certificateFailure(obj.Status)
	must.StrContains(t, message, first.Serial+" is not revoked")

	// The issuer is there again: the certificate is revoked after all.
	a.issuer()
	a.revocationPlaced(first.Serial)
	a.revocationDone(first.Serial, map[string]any{"state": "valid"})
	st = c.waitFor("default", "web-tls", "the earlier certificate revoked", func(_ *structs.CustomObject, st *structs.CertificateStatus) bool {
		e := earlierCertificate(st, first.Serial)
		return e != nil && e.Revoked
	})
	must.Eq(t, &structs.CertificateEarlier{Serial: first.Serial, Issuer: "acme", NotAfter: first.NotAfter, Revoked: true, RevokeReason: structs.CertificateRevokeReasonCessationOfOperation},
		earlierCertificate(st, first.Serial))
	messages = c.eventMessages("default", structs.CertificateKind, "web-tls", "Withdrawn")
	must.Len(t, 2, messages)
	must.Len(t, 1, containing(messages, "the earlier certificate with the serial "+first.Serial+" is revoked"))
}

// A namespace that gets the names back while a withdrawal still waits for
// the revocation of an earlier certificate gets a new certificate, and the
// withdrawal ends (review finding ATM-24e): as for the certificate that was
// held, an order that is placed runs its course, and nothing is placed or
// tried again. An order that fails leaves the earlier certificate valid and
// unmarked; one that is done marks it revoked. The next withdrawal takes up
// what is still unrevoked.
func TestCertificate_ExternalIssuerNamesBackEarlier(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	a := newAcmeTest(t, c)
	first := a.issue("")
	second := a.replaced(first)

	// Back while both revocations are waited for; that of the earlier
	// certificate then fails.
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.org"})
	a.revocationPlaced(second.Serial)
	a.revocationPlaced(first.Serial)
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	third := a.issue(second.Serial)
	must.False(t, earlierCertificate(third, first.Serial).Revoked)
	a.revocationDone(first.Serial, map[string]any{"state": "failed", "problem": "the CA is down"})
	time.Sleep(2 * time.Second)
	must.Nil(t, a.revocation(first.Serial), must.Sprint("with the names back the revocation is not tried again"))
	_, st := c.certificate("default", "web-tls")
	must.Eq(t, &structs.CertificateEarlier{Serial: first.Serial, Issuer: "acme", NotAfter: first.NotAfter},
		earlierCertificate(st, first.Serial), must.Sprint("the earlier certificate is not revoked, and the status says nothing else"))
	must.Eq(t, third.Serial, st.Serial)
	must.SliceEmpty(t, c.eventMessages("default", structs.CertificateKind, "web-tls", "RevocationFailed"))

	// The names go again: the earlier certificate is taken up again. Back
	// once more, its order is done by the issuer: it is marked revoked.
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.org"})
	a.revocationPlaced(third.Serial)
	a.revocationPlaced(first.Serial)
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	fourth := a.issue(third.Serial)
	must.False(t, earlierCertificate(fourth, first.Serial).Revoked)
	a.revocationDone(first.Serial, map[string]any{"state": "valid"})
	st = c.waitFor("default", "web-tls", "the earlier certificate revoked", func(_ *structs.CustomObject, st *structs.CertificateStatus) bool {
		e := earlierCertificate(st, first.Serial)
		return e != nil && e.Revoked
	})
	must.Eq(t, fourth.Serial, st.Serial, must.Sprint("the new certificate stays"))
	must.True(t, objectConditionTrue(c.get("default", structs.CertificateKind, "web-tls").Status, structs.CertificateConditionReady))
	must.Len(t, 1, containing(c.eventMessages("default", structs.CertificateKind, "web-tls", "Revoked"), first.Serial))
}

// cutRetryWait ends the wait of a certificate after a failure, as time does.
func (c *certTest) cutRetryWait(ns, name string) {
	c.t.Helper()
	must.Wait(c.t, wait.InitialSuccess(
		wait.ErrorFunc(func() error {
			obj, _ := c.certificate(ns, name)
			changed := obj.Copy()
			changed.Status["retryAt"] = time.Now().Add(-time.Second).UTC().Format(time.RFC3339)
			_, err := c.s.writeOperatorObject(changed)
			return err
		}),
		wait.Timeout(30*time.Second),
		wait.Gap(50*time.Millisecond),
	))
}

// The certificate a withdrawal was taking back enters the earlier
// certificates marked revoked only if its revocation is done (review finding
// ATM-24g): with the names back while its revocation order is placed, or
// while the withdrawal waits to try a failed revocation again and there is no
// order at all, no issuer has revoked it, and the status must not say so. An
// order that the issuer does before the new certificate is there marks it.
func TestCertificate_ExternalIssuerNamesBackHeld(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	a := newAcmeTest(t, c)
	first := a.issue("")

	// Back while the revocation order of the held certificate is placed; the
	// order then fails.
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.org"})
	a.revocationPlaced(first.Serial)
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	second := a.issue(first.Serial)
	must.NotNil(t, earlierCertificate(second, first.Serial))
	must.False(t, earlierCertificate(second, first.Serial).Revoked,
		must.Sprint("a revocation order that is placed revokes nothing yet"))
	a.revocationDone(first.Serial, map[string]any{"state": "failed", "problem": "the CA is down"})
	time.Sleep(2 * time.Second)
	_, st := c.certificate("default", "web-tls")
	must.Eq(t, &structs.CertificateEarlier{Serial: first.Serial, Issuer: "acme", NotAfter: first.NotAfter},
		earlierCertificate(st, first.Serial), must.Sprint("the certificate nobody revoked is not marked revoked"))

	// Back while the withdrawal waits to place the failed revocation again:
	// there is no order.
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.org"})
	a.revocationPlaced(second.Serial)
	a.revocationDone(second.Serial, map[string]any{"state": "failed", "problem": "the CA is down"})
	c.waitFor("default", "web-tls", "the failed revocation", func(_ *structs.CustomObject, st *structs.CertificateStatus) bool {
		return st.Serial == second.Serial && st.Failures == 1 && st.RetryAt != ""
	})
	must.Nil(t, a.revocation(second.Serial))
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	c.cutRetryWait("default", "web-tls")
	third := a.issue(second.Serial)
	must.Nil(t, a.revocation(second.Serial))
	must.Eq(t, &structs.CertificateEarlier{Serial: second.Serial, Issuer: "acme", NotAfter: second.NotAfter},
		earlierCertificate(third, second.Serial), must.Sprint("a revocation that waits to be tried again has revoked nothing"))

	// Back while the order is placed, and the issuer does it before the new
	// certificate is there: the order is gone by then, the status remembers.
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.org"})
	a.revocationPlaced(third.Serial)
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	c.waitFor("default", "web-tls", "the order of the next certificate", func(_ *structs.CustomObject, st *structs.CertificateStatus) bool {
		return st.Order != "" && st.Serial == third.Serial
	})
	a.revocationDone(third.Serial, map[string]any{"state": "valid"})
	fourth := a.issue(third.Serial)
	must.Eq(t, &structs.CertificateEarlier{Serial: third.Serial, Issuer: "acme", NotAfter: third.NotAfter,
		Revoked: true, RevokeReason: structs.CertificateRevokeReasonCessationOfOperation},
		earlierCertificate(fourth, third.Serial), must.Sprint("the certificate the issuer revoked is marked revoked"))
}

// A user's revocation by serial number with an external issuer is under way
// until the issuer has done the revocation order (review finding ATM-24g):
// the entry says revoking, with the reason asked for, and is marked revoked
// only then, with the reason of the order. A repeated revocation of a
// pending one with the same or a weaker reason changes nothing and places no
// second order; keyCompromise rewrites the open order. An order that fails
// ends the pending state without the mark, and the user can ask again. The
// current certificate goes the same way when it is replaced.
func TestCertificate_ExternalIssuerRevokePending(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	a := newAcmeTest(t, c)
	first := a.issue("")
	second := a.replaced(first)
	entry := func(serial string) *structs.CertificateEarlier {
		t.Helper()
		_, st := c.certificate("default", "web-tls")
		return earlierCertificate(st, serial)
	}
	orderReason := func(serial string) string {
		t.Helper()
		order := a.revocation(serial)
		must.NotNil(t, order)
		spec := &structs.CertificateOrderSpec{}
		must.NoError(t, structs.DecodeObjectPart(order.Spec, spec))
		return spec.Reason
	}

	// Asked for: pending, not revoked.
	resp, err := c.revokeSerial("default", "web-tls", first.Serial, structs.CertificateRevokeReasonSuperseded)
	must.NoError(t, err)
	must.True(t, resp.Earlier)
	must.True(t, resp.Pending, must.Sprint("the answer says that the revocation is only asked for"))
	must.Eq(t, &structs.CertificateEarlier{Serial: first.Serial, Issuer: "acme", NotAfter: first.NotAfter,
		Revoking: structs.CertificateRevokeReasonSuperseded}, entry(first.Serial),
		must.Sprint("a revocation order that is placed does not mark the certificate revoked"))
	placed := a.revocation(first.Serial)
	must.NotNil(t, placed)

	// The same and a weaker reason are no second order, and change nothing.
	for _, reason := range []string{structs.CertificateRevokeReasonSuperseded, structs.CertificateRevokeReasonAffiliationChanged, ""} {
		resp, err = c.revokeSerial("default", "web-tls", first.Serial, reason)
		must.NoError(t, err)
		must.True(t, resp.Pending)
		must.Eq(t, placed.ModifyIndex, a.revocation(first.Serial).ModifyIndex, must.Sprint(reason))
		must.Eq(t, structs.CertificateRevokeReasonSuperseded, entry(first.Serial).Revoking)
	}
	must.Eq(t, 1, certificateOrderCount(t, c.s))
	must.Len(t, 1, c.eventMessages("default", structs.CertificateKind, "web-tls", "RevocationOrdered"))
	must.SliceEmpty(t, c.eventMessages("default", structs.CertificateKind, "web-tls", "Revoked"))

	// The order fails: not revoked, no longer pending, and nothing is tried
	// again.
	a.revocationDone(first.Serial, map[string]any{"state": "failed", "problem": "the CA is down"})
	c.waitFor("default", "web-tls", "the end of the pending revocation", func(_ *structs.CustomObject, st *structs.CertificateStatus) bool {
		e := earlierCertificate(st, first.Serial)
		return e != nil && e.Revoking == ""
	})
	must.Eq(t, &structs.CertificateEarlier{Serial: first.Serial, Issuer: "acme", NotAfter: first.NotAfter}, entry(first.Serial),
		must.Sprint("a failed revocation order leaves the certificate unmarked"))
	time.Sleep(2 * time.Second)
	must.Nil(t, a.revocation(first.Serial))

	// Asked again: a new order. keyCompromise rewrites it while it is open,
	// and the mark carries what the issuer revoked with.
	resp, err = c.revokeSerial("default", "web-tls", first.Serial, "")
	must.NoError(t, err)
	must.True(t, resp.Pending)
	must.Eq(t, "", orderReason(first.Serial))
	must.Eq(t, structs.CertificateRevokeReasonUnspecified, entry(first.Serial).Revoking)
	resp, err = c.revokeSerial("default", "web-tls", first.Serial, structs.CertificateRevokeReasonKeyCompromise)
	must.NoError(t, err)
	must.True(t, resp.Pending)
	must.Eq(t, structs.CertificateRevokeReasonKeyCompromise, orderReason(first.Serial))
	must.Eq(t, &structs.CertificateEarlier{Serial: first.Serial, Issuer: "acme", NotAfter: first.NotAfter,
		Revoking: structs.CertificateRevokeReasonKeyCompromise}, entry(first.Serial))
	a.revocationDone(first.Serial, map[string]any{"state": "valid"})
	c.waitFor("default", "web-tls", "the earlier certificate revoked", func(_ *structs.CustomObject, st *structs.CertificateStatus) bool {
		e := earlierCertificate(st, first.Serial)
		return e != nil && e.Revoked
	})
	must.Eq(t, &structs.CertificateEarlier{Serial: first.Serial, Issuer: "acme", NotAfter: first.NotAfter,
		Revoked: true, RevokeReason: structs.CertificateRevokeReasonKeyCompromise}, entry(first.Serial))
	must.Len(t, 1, containing(c.eventMessages("default", structs.CertificateKind, "web-tls", "Revoked"), first.Serial))
	resp, err = c.revokeSerial("default", "web-tls", first.Serial, structs.CertificateRevokeReasonSuperseded)
	must.NoError(t, err)
	must.True(t, resp.Earlier)
	must.False(t, resp.Pending)
	must.Nil(t, a.revocation(first.Serial))
	_, st := c.certificate("default", "web-tls")
	must.Eq(t, second.Serial, st.Serial, must.Sprint("the current certificate stays"))
	must.Eq(t, "", st.Revoke)

	// The current certificate, replaced while its revocation order is open:
	// it becomes an earlier certificate that is pending, and is marked when
	// the issuer has done the order.
	resp, err = c.revokeSerial("default", "web-tls", "", structs.CertificateRevokeReasonSuperseded)
	must.NoError(t, err)
	must.False(t, resp.Earlier)
	must.True(t, resp.Pending)
	_, st = c.certificate("default", "web-tls")
	must.Eq(t, second.Serial, st.Revoke)
	must.False(t, st.Revoked)
	must.Eq(t, structs.CertificateRevokeReasonSuperseded, st.Revoking)
	third := a.issue(second.Serial)
	must.Eq(t, &structs.CertificateEarlier{Serial: second.Serial, Issuer: "acme", NotAfter: second.NotAfter,
		Revoking: structs.CertificateRevokeReasonSuperseded}, earlierCertificate(third, second.Serial),
		must.Sprint("a certificate replaced before its issuer revoked it is not marked revoked"))
	must.False(t, third.Revoked)
	must.Eq(t, "", third.Revoking)
	a.revocationDone(second.Serial, map[string]any{"state": "valid"})
	c.waitFor("default", "web-tls", "the replaced certificate revoked", func(_ *structs.CustomObject, st *structs.CertificateStatus) bool {
		e := earlierCertificate(st, second.Serial)
		return e != nil && e.Revoked
	})
	must.Eq(t, &structs.CertificateEarlier{Serial: second.Serial, Issuer: "acme", NotAfter: second.NotAfter,
		Revoked: true, RevokeReason: structs.CertificateRevokeReasonSuperseded}, entry(second.Serial))

	// The current certificate whose order the issuer does before the next
	// one is there: the order is gone by then, the status remembers.
	_, err = c.revokeSerial("default", "web-tls", "", structs.CertificateRevokeReasonKeyCompromise)
	must.NoError(t, err)
	c.waitFor("default", "web-tls", "the order of the next certificate", func(_ *structs.CustomObject, st *structs.CertificateStatus) bool {
		return st.Order != "" && st.Serial == third.Serial
	})
	a.revocationDone(third.Serial, map[string]any{"state": "valid"})
	done := c.waitFor("default", "web-tls", "the current certificate revoked", func(_ *structs.CustomObject, st *structs.CertificateStatus) bool {
		return st.Revoked
	})
	must.Eq(t, third.Serial, done.Serial)
	must.Eq(t, structs.CertificateRevokeReasonKeyCompromise, done.RevokeReason)
	must.Eq(t, "", done.Revoking)
	fourth := a.issue(third.Serial)
	must.Eq(t, &structs.CertificateEarlier{Serial: third.Serial, Issuer: "acme", NotAfter: third.NotAfter,
		Revoked: true, RevokeReason: structs.CertificateRevokeReasonKeyCompromise}, earlierCertificate(fourth, third.Serial))
	must.False(t, fourth.Revoked)

	// A certificate its issuer has revoked is not revoked again by a
	// withdrawal: no second order for it, and nothing to wait for.
	_, err = c.revokeSerial("default", "web-tls", "", "")
	must.NoError(t, err)
	c.waitFor("default", "web-tls", "the order of the next certificate", func(_ *structs.CustomObject, st *structs.CertificateStatus) bool {
		return st.Order != "" && st.Serial == fourth.Serial
	})
	a.revocationDone(fourth.Serial, map[string]any{"state": "valid"})
	c.waitFor("default", "web-tls", "the current certificate revoked", func(_ *structs.CustomObject, st *structs.CertificateStatus) bool {
		return st.Revoked
	})
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.org"})
	st = c.waitFor("default", "web-tls", "withdrawn", withdrawn)
	must.Eq(t, &structs.CertificateEarlier{Serial: fourth.Serial, Issuer: "acme", NotAfter: fourth.NotAfter,
		Revoked: true, RevokeReason: structs.CertificateRevokeReasonUnspecified}, earlierCertificate(st, fourth.Serial))
	must.Nil(t, a.revocation(fourth.Serial))
	// The one failure is that of the order of the user above (review
	// finding ATM-24d): the withdrawal had none.
	failed := c.eventMessages("default", structs.CertificateKind, "web-tls", "RevocationFailed")
	must.Len(t, 1, failed)
	must.StrContains(t, failed[0], first.Serial)
}

// A revocation a user asked an external issuer for, and that the issuer
// fails or does not answer in time, is told by an event on the certificate
// (review finding ATM-24d): one RevocationFailed for every such order, with
// the serial, the issuer and why. Nothing is tried again, the conditions of
// the certificate stay as they are, and the user can ask again: a new order
// is placed, for an earlier certificate and for the current one alike.
func TestCertificate_ExternalIssuerRevocationFailed(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	a := newAcmeTest(t, c)
	first := a.issue("")
	second := a.replaced(first)
	failures := func() []string {
		t.Helper()
		return c.eventMessages("default", structs.CertificateKind, "web-tls", "RevocationFailed")
	}
	notPending := func(serial string) {
		t.Helper()
		c.waitFor("default", "web-tls", "the end of the pending revocation", func(_ *structs.CustomObject, st *structs.CertificateStatus) bool {
			e := earlierCertificate(st, serial)
			return e != nil && e.Revoking == ""
		})
	}

	// The issuer fails the order of an earlier certificate.
	resp, err := c.revokeSerial("default", "web-tls", first.Serial, structs.CertificateRevokeReasonSuperseded)
	must.NoError(t, err)
	must.True(t, resp.Pending)
	a.revocationDone(first.Serial, map[string]any{"state": "failed", "problem": "the CA is down"})
	notPending(first.Serial)
	messages := failures()
	must.Len(t, 1, messages, must.Sprint("a failed revocation order of a user is told by an event"))
	must.StrContains(t, messages[0], "earlier certificate with the serial "+first.Serial)
	must.StrContains(t, messages[0], `"acme"`)
	must.StrContains(t, messages[0], "the CA is down")
	time.Sleep(2 * time.Second)
	must.Len(t, 1, failures(), must.Sprint("one event for a failed order, not one for every pass"))
	obj, st := c.certificate("default", "web-tls")
	must.Eq(t, &structs.CertificateEarlier{Serial: first.Serial, Issuer: "acme", NotAfter: first.NotAfter},
		earlierCertificate(st, first.Serial))
	must.True(t, objectConditionTrue(obj.Status, structs.CertificateConditionReady))
	must.False(t, objectConditionTrue(obj.Status, structs.CertificateConditionFailed),
		must.Sprint("the certificate in use is not failed by the revocation of an earlier one"))

	// Asked again: a new order. The issuer does not answer it in time.
	resp, err = c.revokeSerial("default", "web-tls", first.Serial, structs.CertificateRevokeReasonSuperseded)
	must.NoError(t, err)
	must.True(t, resp.Pending)
	must.NotNil(t, a.revocation(first.Serial), must.Sprint("a second request places a new order"))
	must.Wait(t, wait.InitialSuccess(
		wait.ErrorFunc(func() error {
			aged := a.revocation(first.Serial).Copy()
			aged.CreateTime = time.Now().Add(-certificateRevokeOrderKeep - time.Minute).UnixNano()
			_, err := c.s.writeOperatorObject(aged)
			return err
		}),
		wait.Timeout(10*time.Second), wait.Gap(50*time.Millisecond)))
	notPending(first.Serial)
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return a.revocation(first.Serial) == nil }),
		wait.Timeout(30*time.Second), wait.Gap(50*time.Millisecond)), must.Sprint("the order that was not answered is removed"))
	messages = failures()
	must.Len(t, 2, messages, must.Sprint("a revocation order that is not answered in time is told by an event"))
	must.StrContains(t, messages[1], "earlier certificate with the serial "+first.Serial)
	must.StrContains(t, messages[1], `"acme"`)
	must.StrContains(t, messages[1], "did not answer within "+certificateRevokeOrderKeep.String())
	time.Sleep(2 * time.Second)
	must.Len(t, 2, failures())
	_, st = c.certificate("default", "web-tls")
	must.Eq(t, &structs.CertificateEarlier{Serial: first.Serial, Issuer: "acme", NotAfter: first.NotAfter},
		earlierCertificate(st, first.Serial))

	// The current certificate: the same event, and it can be asked for again
	// before the next certificate is there.
	resp, err = c.revokeSerial("default", "web-tls", "", structs.CertificateRevokeReasonSuperseded)
	must.NoError(t, err)
	must.True(t, resp.Pending)
	a.revocationDone(second.Serial, map[string]any{"state": "failed", "problem": "rate limited"})
	st = c.waitFor("default", "web-tls", "the end of the pending revocation", func(_ *structs.CustomObject, st *structs.CertificateStatus) bool {
		return st.Revoking == ""
	})
	must.Eq(t, second.Serial, st.Serial)
	must.Eq(t, second.Serial, st.Revoke)
	must.False(t, st.Revoked)
	messages = failures()
	must.Len(t, 3, messages, must.Sprint("a failed revocation order of the current certificate is told by an event"))
	must.StrContains(t, messages[2], "certificate with the serial "+second.Serial)
	must.StrNotContains(t, messages[2], "earlier certificate")
	must.StrContains(t, messages[2], "rate limited")
	resp, err = c.revokeSerial("default", "web-tls", "", structs.CertificateRevokeReasonSuperseded)
	must.NoError(t, err)
	must.False(t, resp.Earlier)
	must.True(t, resp.Pending, must.Sprint("the current certificate can be asked for again"))
	must.NotNil(t, a.revocation(second.Serial), must.Sprint("a second request places a new order"))
	_, st = c.certificate("default", "web-tls")
	must.Eq(t, structs.CertificateRevokeReasonSuperseded, st.Revoking)

	// The next certificate is there before the issuer answers: what was
	// held is pending among the earlier ones, and a failure is told there.
	third := a.issue(second.Serial)
	must.Eq(t, &structs.CertificateEarlier{Serial: second.Serial, Issuer: "acme", NotAfter: second.NotAfter,
		Revoking: structs.CertificateRevokeReasonSuperseded}, earlierCertificate(third, second.Serial))
	a.revocationDone(second.Serial, map[string]any{"state": "failed"})
	notPending(second.Serial)
	messages = failures()
	must.Len(t, 4, messages)
	must.StrContains(t, messages[3], "earlier certificate with the serial "+second.Serial)
	must.StrContains(t, messages[3], "the issuer gave no reason")
}

// The current certificate that no issuer is left to revoke is not revoked by
// a request, and nothing says that it was (review finding ATM-24k): the first
// request is refused with a conflict that says why and until when the
// certificate is valid, as a repeated one and that for an earlier certificate
// are. It writes nothing: the status does not say revoke, there is no event
// Revoked and no revocation order, and no new certificate is issued in the
// place of the one that stays in use. The reason keyCompromise changes
// nothing in that. Checked with the external issuer gone, and with the issuer
// recreated as a built-in one.
func TestCertificate_ExternalIssuerRevokeNoIssuer(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	a := newAcmeTest(t, c)
	held := a.issue("")
	inUse := c.secretItems("default", "certs/web")[structs.CertificateSecretItemCert]
	must.NotEq(t, "", inUse)
	failed := func(reason string) func(*structs.CustomObject, *structs.CertificateStatus) bool {
		return func(obj *structs.CustomObject, _ *structs.CertificateStatus) bool {
			got, _ := certificateFailure(obj.Status)
			return got == reason
		}
	}
	// refused asks for the revocation of the current certificate, and checks
	// that it is refused and that nothing has changed.
	refused := func(reason, why string) {
		t.Helper()
		_, err := c.revokeSerial("default", "web-tls", "", reason)
		must.Error(t, err, must.Sprint("a certificate nobody can revoke is not answered as revoked"))
		code, msg, ok := structs.CodeFromRPCCodedErr(err)
		must.True(t, ok, must.Sprint(err))
		must.Eq(t, 409, code)
		must.StrContains(t, msg, "the certificate with the serial "+held.Serial+" is not revoked")
		must.StrContains(t, msg, why)
		must.StrContains(t, msg, "it stays valid until "+held.NotAfter)

		_, st := c.certificate("default", "web-tls")
		must.Eq(t, held.Serial, st.Serial, must.Sprint("the certificate stays the current one"))
		must.Eq(t, "", st.Revoke, must.Sprint("the status does not say that a revocation was asked for"))
		must.False(t, st.Revoked)
		must.Eq(t, "", st.Revoking)
		must.Eq(t, "", st.Order)
		must.Eq(t, held.Orders, st.Orders, must.Sprint("nothing is ordered in its place"))
		must.SliceEmpty(t, st.Earlier)
		must.SliceEmpty(t, c.eventMessages("default", structs.CertificateKind, "web-tls", "Revoked"),
			must.Sprint("no event says that the certificate was revoked"))
		must.SliceEmpty(t, c.eventMessages("default", structs.CertificateKind, "web-tls", "RevocationOrdered"))
		must.Eq(t, 0, certificateOrderCount(t, c.s))
	}

	// The external issuer is gone.
	must.NoError(t, c.delete("", structs.CertificateIssuerKind, "acme"))
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return c.get("", structs.CertificateIssuerKind, "acme") == nil }),
		wait.Timeout(30*time.Second), wait.Gap(50*time.Millisecond)))
	c.waitFor("default", "web-tls", "IssuerNotFound", failed("IssuerNotFound"))
	for _, reason := range []string{"", structs.CertificateRevokeReasonKeyCompromise, structs.CertificateRevokeReasonSuperseded} {
		refused(reason, `the issuer "acme" is gone`)
	}
	must.Eq(t, inUse, c.secretItems("default", "certs/web")[structs.CertificateSecretItemCert],
		must.Sprint("the certificate stays in use"))

	// The issuer is recreated as a built-in one. That alone has a new
	// certificate issued, after which the one of the external issuer is an
	// earlier certificate; a secret at the path that is not the certificate's
	// own keeps it the current one here.
	c.secretVersions(structs.SecretOpDestroy, "default", "certs/web")
	var w structs.SecretWriteResponse
	must.NoError(t, c.s.RPC(structs.SecretUpsertRPCMethod, secretWriteReq("certs/web", "", structs.SecretItems{"cert": "mine"}), &w))
	_, err := c.apply("", structs.CertificateIssuerKind, "acme", map[string]any{"builtin": map[string]any{}})
	must.NoError(t, err)
	c.waitFor("default", "web-tls", "SecretConflict", failed("SecretConflict"))
	for _, reason := range []string{"", structs.CertificateRevokeReasonKeyCompromise} {
		refused(reason, `the issuer "acme" is no longer the external issuer that signed it`)
	}
	must.Eq(t, "mine", c.secretItems("default", "certs/web")["cert"])
}

// The reason of a revoked current certificate can't be raised to
// keyCompromise when no issuer is left to tell: the request is refused with a
// conflict, as that for an earlier certificate is, instead of an answer that
// reads as if the reason were taken. The refusal says that the certificate is
// revoked already, not that it is valid, and the status stays as it is. A
// request that raises nothing is answered as before: nothing changes.
func TestCertificate_ExternalIssuerRaiseNoIssuer(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	a := newAcmeTest(t, c)
	held := a.issue("")

	// Revoked by its issuer; the next certificate is not there yet.
	resp, err := c.revokeSerial("default", "web-tls", "", structs.CertificateRevokeReasonSuperseded)
	must.NoError(t, err)
	must.True(t, resp.Pending)
	a.revocationDone(held.Serial, map[string]any{"state": "valid"})
	c.waitFor("default", "web-tls", "the current certificate revoked", func(_ *structs.CustomObject, st *structs.CertificateStatus) bool {
		return st.Revoked && st.Serial == held.Serial
	})

	must.NoError(t, c.delete("", structs.CertificateIssuerKind, "acme"))
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return c.get("", structs.CertificateIssuerKind, "acme") == nil }),
		wait.Timeout(30*time.Second), wait.Gap(50*time.Millisecond)))

	_, err = c.revokeSerial("default", "web-tls", "", structs.CertificateRevokeReasonKeyCompromise)
	must.Error(t, err, must.Sprint("a reason nobody can be told is not answered as taken"))
	code, msg, ok := structs.CodeFromRPCCodedErr(err)
	must.True(t, ok, must.Sprint(err))
	must.Eq(t, 409, code)
	must.StrContains(t, msg, "the certificate with the serial "+held.Serial+" is revoked already")
	must.StrContains(t, msg, "is not changed to keyCompromise")
	must.StrContains(t, msg, `the issuer "acme" is gone`)
	must.StrNotContains(t, msg, "is not revoked")
	must.StrNotContains(t, msg, "stays valid")

	resp, err = c.revokeSerial("default", "web-tls", "", structs.CertificateRevokeReasonAffiliationChanged)
	must.NoError(t, err, must.Sprint("a request that raises nothing changes nothing, as before"))
	must.False(t, resp.Pending)

	_, st := c.certificate("default", "web-tls")
	must.Eq(t, held.Serial, st.Serial)
	must.Eq(t, held.Serial, st.Revoke)
	must.True(t, st.Revoked)
	must.Eq(t, structs.CertificateRevokeReasonSuperseded, st.RevokeReason)
	must.Eq(t, "", st.Revoking)
	must.SliceEmpty(t, c.eventMessages("default", structs.CertificateKind, "web-tls", "RevocationReason"))
	must.Nil(t, a.revocation(held.Serial), must.Sprint("no revocation order is placed"))
}

// A first request for the revocation of the current certificate that finds a
// revocation order of its serial which is over without the revocation (a
// withdrawal placed it, the issuer failed it, and the namespace got the names
// back before the operator removed it) places no order, so it revokes
// nothing and says nothing else (review finding ATM-24k): it is refused with
// a conflict, as a repeated request is, and writes nothing. Asked again when
// the operator has removed the order, a new order is placed.
func TestCertificate_ExternalIssuerRevokeLeftoverOrder(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	c.allow("default", map[string]string{structs.NamespaceMetaCertificateDNSSuffixes: "example.com"})
	a := newAcmeTest(t, c)
	held := a.issue("")

	// The operator removes a finished order in its next pass, which begins
	// operatorsSettle after the order is written at the earliest. A request
	// that comes after that pass all the same places an order of its own:
	// the next certificate is taken then.
	for attempt := 1; ; attempt++ {
		spec, err := structs.EncodeObjectPart(&structs.CertificateOrderSpec{
			Issuer: "acme", Certificate: "default/web-tls", Revoke: held.Serial,
			Reason: structs.CertificateRevokeReasonCessationOfOperation,
		})
		must.NoError(t, err)
		_, err = c.s.writeOperatorObject(&structs.CustomObject{
			Group:     structs.OperatorsGroup,
			Kind:      structs.CertificateOrderKind,
			Name:      certificateRevokeOrderName("default", "web-tls", held.Serial),
			ManagedBy: structs.OperatorsManagedBy,
			Spec:      spec,
			Status:    map[string]any{"state": structs.CertificateOrderFailed, "problem": "the CA is down"},
		})
		must.NoError(t, err)
		resp, err := c.revokeSerial("default", "web-tls", "", "")
		if err == nil && resp.Pending && attempt < 5 {
			held = a.issue(held.Serial)
			continue
		}
		must.Error(t, err, must.Sprint("an order that failed has revoked nothing"))
		code, msg, ok := structs.CodeFromRPCCodedErr(err)
		must.True(t, ok, must.Sprint(err))
		must.Eq(t, 409, code)
		must.StrContains(t, msg, "the last revocation order of the certificate with the serial "+held.Serial+" is over")
		must.StrContains(t, msg, "the certificate is not revoked")
		break
	}

	_, st := c.certificate("default", "web-tls")
	must.Eq(t, held.Serial, st.Serial, must.Sprint("the certificate stays the current one"))
	must.Eq(t, "", st.Revoke, must.Sprint("the status does not say that a revocation was asked for"))
	must.False(t, st.Revoked)
	must.Eq(t, "", st.Revoking)
	must.Eq(t, "", st.Order, must.Sprint("nothing is ordered in its place"))
	must.SliceEmpty(t, c.eventMessages("default", structs.CertificateKind, "web-tls", "Revoked"),
		must.Sprint("no event says that the certificate was revoked"))
	must.SliceEmpty(t, containing(c.eventMessages("default", structs.CertificateKind, "web-tls", "RevocationOrdered"), held.Serial))

	// The operator removes the order; asked again, a new one is placed.
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return a.revocation(held.Serial) == nil }),
		wait.Timeout(30*time.Second), wait.Gap(50*time.Millisecond)), must.Sprint("the order that failed is removed"))
	resp, err := c.revokeSerial("default", "web-tls", "", "")
	must.NoError(t, err)
	must.True(t, resp.Pending)
	must.NotNil(t, a.revocation(held.Serial))
	_, st = c.certificate("default", "web-tls")
	must.Eq(t, held.Serial, st.Revoke)
	must.Eq(t, structs.CertificateRevokeReasonUnspecified, st.Revoking)
}
