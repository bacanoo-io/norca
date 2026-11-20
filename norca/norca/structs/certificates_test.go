// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"strings"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
)

func TestCertificateDNSNameAllowed(t *testing.T) {
	ci.Parallel(t)

	tests := []struct {
		name     string
		dnsName  string
		suffixes []string
		expected bool
	}{
		{name: "no suffixes allow nothing", dnsName: "web.example.com", expected: false},
		{name: "the suffix itself", dnsName: "example.com", suffixes: []string{"example.com"}, expected: true},
		{name: "a name below the suffix", dnsName: "a.b.example.com", suffixes: []string{"example.com"}, expected: true},
		{name: "a name that only ends like the suffix", dnsName: "badexample.com", suffixes: []string{"example.com"}, expected: false},
		{name: "the parent of the suffix", dnsName: "example.com", suffixes: []string{"team.example.com"}, expected: false},
		{name: "a wildcard below the suffix", dnsName: "*.web.example.com", suffixes: []string{"example.com"}, expected: true},
		{name: "a wildcard of the suffix", dnsName: "*.example.com", suffixes: []string{"example.com"}, expected: true},
		{name: "a wildcard above the suffix", dnsName: "*.example.com", suffixes: []string{"team.example.com"}, expected: false},
		{name: "a suffix written with a leading dot", dnsName: "web.example.com", suffixes: []string{".example.com"}, expected: true},
		{name: "the second suffix", dnsName: "web.example.org", suffixes: []string{"example.com", "example.org"}, expected: true},
		{name: "an empty suffix allows nothing", dnsName: "web.example.com", suffixes: []string{""}, expected: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must.Eq(t, tt.expected, CertificateDNSNameAllowed(tt.dnsName, tt.suffixes))
		})
	}
}

func TestCertificateIPAllowed(t *testing.T) {
	ci.Parallel(t)

	tests := []struct {
		name     string
		addr     string
		allowed  []string
		expected bool
	}{
		{name: "nothing listed", addr: "10.0.0.5", expected: false},
		{name: "the address itself", addr: "10.0.0.5", allowed: []string{"10.0.0.5"}, expected: true},
		{name: "inside a network", addr: "10.0.0.5", allowed: []string{"10.0.0.0/24"}, expected: true},
		{name: "outside the network", addr: "10.0.1.5", allowed: []string{"10.0.0.0/24"}, expected: false},
		{name: "an ipv6 address in its network", addr: "fd00::5", allowed: []string{"fd00::/64"}, expected: true},
		{name: "not an address", addr: "web", allowed: []string{"0.0.0.0/0"}, expected: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must.Eq(t, tt.expected, CertificateIPAllowed(tt.addr, tt.allowed))
		})
	}
}

func TestCertificateSpec_Validate(t *testing.T) {
	ci.Parallel(t)

	valid := func() *CertificateSpec {
		return &CertificateSpec{Issuer: "internal", DNSNames: []string{"web.example.com"}, SecretPath: "certs/web"}
	}
	tests := []struct {
		name     string
		change   func(*CertificateSpec)
		expected string
	}{
		{name: "valid", change: func(*CertificateSpec) {}},
		{name: "wildcard", change: func(s *CertificateSpec) { s.DNSNames = []string{"*.example.com"} }},
		{name: "only an ip address", change: func(s *CertificateSpec) { s.DNSNames, s.IPAddresses = nil, []string{"10.0.0.5"} }},
		{name: "no name", change: func(s *CertificateSpec) { s.DNSNames = nil }, expected: "needs a DNS name"},
		{name: "upper case name", change: func(s *CertificateSpec) { s.DNSNames = []string{"Web.example.com"} }, expected: "invalid DNS name"},
		{name: "wildcard in the middle", change: func(s *CertificateSpec) { s.DNSNames = []string{"a.*.example.com"} }, expected: "invalid DNS name"},
		{name: "wildcard right under a top level name", change: func(s *CertificateSpec) { s.DNSNames = []string{"*.lab"} },
			expected: `a wildcard right under a top level name is not accepted by TLS clients, put it one level lower (for example "*.apps.lab")`},
		{name: "wildcard one level lower", change: func(s *CertificateSpec) { s.DNSNames = []string{"app.lab", "*.apps.lab"} }},
		{name: "a top level name without a wildcard", change: func(s *CertificateSpec) { s.DNSNames = []string{"lab"} }},
		{name: "name listed twice", change: func(s *CertificateSpec) { s.DNSNames = append(s.DNSNames, "web.example.com") }, expected: "listed twice"},
		{name: "invalid ip address", change: func(s *CertificateSpec) { s.IPAddresses = []string{"10.0.0"} }, expected: "invalid IP address"},
		{name: "invalid secret path", change: func(s *CertificateSpec) { s.SecretPath = "/certs" }, expected: "invalid secret path"},
		{name: "validity too long", change: func(s *CertificateSpec) { s.Validity = "9000h" }, expected: "longer than the maximum"},
		{name: "validity too short", change: func(s *CertificateSpec) { s.Validity = "10m" }, expected: "shorter than an hour"},
		{name: "renew_before not shorter than the validity", change: func(s *CertificateSpec) { s.Validity, s.RenewBefore = "24h", "24h" }, expected: "not shorter"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := valid()
			tt.change(spec)
			err := spec.Validate()
			if tt.expected == "" {
				must.NoError(t, err)
				return
			}
			must.ErrorContains(t, err, tt.expected)
		})
	}
}

func TestCheckCertificateNames(t *testing.T) {
	ci.Parallel(t)

	ns := func(meta map[string]string) *Namespace { return &Namespace{Name: "team-a", Meta: meta} }
	tests := []struct {
		name     string
		spec     *CertificateSpec
		ns       *Namespace
		builtin  bool
		expected string
	}{
		{
			name:     "nothing allowed without metadata",
			spec:     &CertificateSpec{Issuer: "internal", DNSNames: []string{"web.example.com"}},
			ns:       ns(nil),
			builtin:  true,
			expected: "may not ask for the DNS name",
		},
		{
			name:    "a name under a suffix of the namespace",
			spec:    &CertificateSpec{Issuer: "internal", DNSNames: []string{"web.team-a.example.com"}},
			ns:      ns(map[string]string{NamespaceMetaCertificateDNSSuffixes: "other.example.com, team-a.example.com"}),
			builtin: true,
		},
		{
			name:     "the name of another namespace",
			spec:     &CertificateSpec{Issuer: "internal", DNSNames: []string{"web.team-b.example.com"}},
			ns:       ns(map[string]string{NamespaceMetaCertificateDNSSuffixes: "team-a.example.com"}),
			builtin:  true,
			expected: "may not ask for the DNS name",
		},
		{
			name: "an issuer the namespace may not use",
			spec: &CertificateSpec{Issuer: "letsencrypt", DNSNames: []string{"web.team-a.example.com"}},
			ns: ns(map[string]string{
				NamespaceMetaCertificateDNSSuffixes: "team-a.example.com",
				NamespaceMetaCertificateIssuers:     "internal",
			}),
			expected: "may not use the issuer",
		},
		{
			name:     "an ip address with an external issuer",
			spec:     &CertificateSpec{Issuer: "letsencrypt", IPAddresses: []string{"10.0.0.5"}},
			ns:       ns(map[string]string{NamespaceMetaCertificateIPAddresses: "10.0.0.0/24"}),
			expected: "built-in issuer only",
		},
		{
			name:     "an ip address that is not listed",
			spec:     &CertificateSpec{Issuer: "internal", IPAddresses: []string{"10.0.1.5"}},
			ns:       ns(map[string]string{NamespaceMetaCertificateIPAddresses: "10.0.0.0/24"}),
			builtin:  true,
			expected: "may not ask for the IP address",
		},
		{
			name:    "an ip address that is listed",
			spec:    &CertificateSpec{Issuer: "internal", IPAddresses: []string{"10.0.0.5"}},
			ns:      ns(map[string]string{NamespaceMetaCertificateIPAddresses: "10.0.0.0/24"}),
			builtin: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckCertificateNames(tt.spec, tt.ns, tt.builtin)
			if tt.expected == "" {
				must.NoError(t, err)
				return
			}
			must.ErrorContains(t, err, tt.expected)
		})
	}
}

func TestCertificateSpec_RequestHash(t *testing.T) {
	ci.Parallel(t)

	base := &CertificateSpec{Issuer: "internal", DNSNames: []string{"a.example.com", "b.example.com"}, SecretPath: "certs/a"}
	same := &CertificateSpec{Issuer: "internal", DNSNames: []string{"b.example.com", "a.example.com"},
		SecretPath: "certs/other", RenewBefore: "24h", KeyType: CertificateDefaultKeyType}
	must.Eq(t, base.RequestHash(), same.RequestHash(), must.Sprint("the order of the names, the secret path and the renewal do not count"))

	for name, change := range map[string]func(*CertificateSpec){
		"issuer":   func(s *CertificateSpec) { s.Issuer = "other" },
		"names":    func(s *CertificateSpec) { s.DNSNames = []string{"a.example.com"} },
		"key type": func(s *CertificateSpec) { s.KeyType = "rsa-2048" },
		"validity": func(s *CertificateSpec) { s.Validity = "24h" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := *base
			change(&changed)
			must.NotEq(t, base.RequestHash(), changed.RequestHash())
		})
	}
}

func TestCertificateAuthority_Copy(t *testing.T) {
	ci.Parallel(t)

	ca := &CertificateAuthority{
		ID:           "a",
		SigningKey:   []byte("key"),
		SigningKeyID: "k1",
		Revoked:      []*CertificateRevocation{{Serial: "0a", NotAfter: time.Unix(100, 0)}},
	}
	cp := ca.Copy()
	cp.SigningKey[0] = 'x'
	cp.Revoked[0].Serial = "0b"
	must.Eq(t, "key", string(ca.SigningKey))
	must.Eq(t, "0a", ca.Revoked[0].Serial)

	public := ca.Public()
	must.Nil(t, public.SigningKey)
	must.Eq(t, "", public.SigningKeyID)
	must.Eq(t, "key", string(ca.SigningKey))
}

func TestCertificateOrderName(t *testing.T) {
	ci.Parallel(t)

	name := CertificateOrderName("team-a", "web-tls", 7)
	must.Eq(t, "team-a.web-tls.7", name)
	must.True(t, validCustomObjectName.MatchString(name))
	must.False(t, strings.Contains(name, "/"))
}

// A serial number is taken in the forms a user sees it in, and compared in
// the one the status holds (review finding ATM-24b).
func TestNormalizeCertificateSerial(t *testing.T) {
	ci.Parallel(t)

	tests := []struct {
		name     string
		serial   string
		expected string
		ok       bool
	}{
		{name: "as the status shows it", serial: "4f2a9c01d37be6a8", expected: "4f2a9c01d37be6a8", ok: true},
		{name: "upper case", serial: "4F2A9C01", expected: "4f2a9c01", ok: true},
		{name: "colons between the octets", serial: "4F:2A:9C:01", expected: "4f2a9c01", ok: true},
		{name: "a leading 0x", serial: "0x4f2a9c01", expected: "4f2a9c01", ok: true},
		{name: "space around it", serial: " 4f2a9c01\n", expected: "4f2a9c01", ok: true},
		{name: "a leading zero octet", serial: "00:8F:2A", expected: "8f2a", ok: true},
		{name: "an odd number of digits", serial: "f2a", expected: "0f2a", ok: true},
		{name: "a leading zero digit kept for the octet", serial: "0a0b0c", expected: "0a0b0c", ok: true},
		{name: "twenty octets", serial: strings.Repeat("ab", 20), expected: strings.Repeat("ab", 20), ok: true},
		{name: "empty", serial: ""},
		{name: "only zeros", serial: "0000"},
		{name: "only the prefix", serial: "0x"},
		{name: "not hexadecimal", serial: "4f2g"},
		{name: "a decimal point", serial: "12.5"},
		{name: "a path", serial: "../4f2a"},
		{name: "too long", serial: strings.Repeat("ab", 40)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := NormalizeCertificateSerial(tt.serial)
			must.Eq(t, tt.ok, ok)
			must.Eq(t, tt.expected, got)
		})
	}
}

// The earlier certificates are part of the status as the object holds it.
func TestCertificateStatus_Earlier(t *testing.T) {
	ci.Parallel(t)

	in := &CertificateStatus{Serial: "0b", Earlier: []*CertificateEarlier{
		{Serial: "0a", Issuer: "internal", Authority: "ca-1", NotAfter: "2027-01-01T00:00:00Z", Revoked: true},
		{Serial: "09", Issuer: "acme", NotAfter: "2027-01-02T00:00:00Z"},
	}}
	part, err := EncodeObjectPart(in)
	must.NoError(t, err)
	must.Eq[any](t, []any{
		map[string]any{"serial": "0a", "issuer": "internal", "authority": "ca-1", "notAfter": "2027-01-01T00:00:00Z", "revoked": true},
		map[string]any{"serial": "09", "issuer": "acme", "notAfter": "2027-01-02T00:00:00Z"},
	}, part["earlier"])
	out := &CertificateStatus{}
	must.NoError(t, DecodeObjectPart(part, out))
	must.Eq(t, in, out)

	none, err := EncodeObjectPart(&CertificateStatus{Serial: "0b"})
	must.NoError(t, err)
	must.MapNotContainsKey(t, none, "earlier")
}
