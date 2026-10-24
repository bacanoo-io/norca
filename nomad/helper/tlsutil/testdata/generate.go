// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build ignore

// This program regenerates the TLS test fixtures in this directory with the
// same CA and certificate settings as the `nomad tls` commands described in
// README.md, but with a long validity so the fixtures don't expire.
//
// Run it from this directory:
//
//	go run generate.go
package main

import (
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/hashicorp/nomad/helper/tlsutil"
)

// days is the validity of every generated CA and certificate.
const days = 7300

func main() {
	nomadCA, nomadKey := ca("nomad", nil)
	badCA, badKey := ca("bad", []string{"bad", "localhost"})
	ca("whitespace", []string{"whitespace", "localhost"})

	// The whitespace test needs trailing whitespace after the PEM block
	appendFile("whitespace-agent-ca.pem", "\n\n\n")

	for _, region := range []string{"global", "regionFoo"} {
		for _, kind := range []string{"server", "client"} {
			cert(nomadCA, nomadKey, kind, region, "nomad")
		}
	}
	for _, kind := range []string{"server", "client"} {
		cert(badCA, badKey, kind, "badRegion", "bad")
	}
}

// ca creates <domain>-agent-ca.pem and <domain>-agent-ca-key.pem. A non-empty
// permitted list adds a critical name constraint, as `nomad tls ca create
// -name-constraint` does.
func ca(domain string, permitted []string) (string, string) {
	caPEM, keyPEM, err := tlsutil.GenerateCA(tlsutil.CAOpts{
		Days:                days,
		PermittedDNSDomains: permitted,
	})
	check(err)
	writeFile(domain+"-agent-ca.pem", caPEM)
	writeFile(domain+"-agent-ca-key.pem", keyPEM)
	return caPEM, keyPEM
}

// cert creates <region>-<kind>-<domain>.pem and its key with the subject
// alternative names `nomad tls cert create` uses.
func cert(caPEM, caKeyPEM, kind, region, domain string) {
	signer, err := tlsutil.ParseSigner(caKeyPEM)
	check(err)

	name := fmt.Sprintf("%s.%s.%s", kind, region, domain)
	regionName := fmt.Sprintf("%s.%s.nomad", kind, region)
	globalName := fmt.Sprintf("%s.global.nomad", kind)

	var dnsNames []string
	switch {
	case region != "global" && domain != "nomad":
		dnsNames = []string{name, regionName, globalName, "localhost"}
	case region != "global":
		dnsNames = []string{regionName, globalName, "localhost"}
	case domain != "nomad":
		dnsNames = []string{regionName, name, "localhost"}
	default:
		dnsNames = []string{name, "localhost"}
	}

	certPEM, keyPEM, err := tlsutil.GenerateCert(tlsutil.CertOpts{
		Signer:      signer,
		CA:          caPEM,
		Name:        name,
		Days:        days,
		DNSNames:    dnsNames,
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	})
	check(err)

	prefix := strings.Join([]string{region, kind, domain}, "-")
	writeFile(prefix+".pem", certPEM)
	writeFile(prefix+"-key.pem", keyPEM)
}

func writeFile(path, content string) {
	check(os.WriteFile(path, []byte(content), 0o644))
}

func appendFile(path, content string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	check(err)
	defer f.Close()
	_, err = f.WriteString(content)
	check(err)
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
