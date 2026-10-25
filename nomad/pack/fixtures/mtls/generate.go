// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build ignore

// This program regenerates the mTLS test fixtures in this directory with the
// fork's own helper/tlsutil: a CA for the "nomad" domain and one server and one
// client certificate for the "global" region, with the subject alternative
// names `nomad tls cert create` uses and a long validity so the fixtures don't
// expire.
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

	"github.com/hashicorp/nomad/helper/tlsutil"
)

// days is the validity of the generated CA and certificates.
const days = 7300

func main() {
	caPEM, caKeyPEM, err := tlsutil.GenerateCA(tlsutil.CAOpts{Days: days})
	check(err)
	writeFile("nomad-agent-ca.pem", caPEM)
	writeFile("nomad-agent-ca-key.pem", caKeyPEM)

	signer, err := tlsutil.ParseSigner(caKeyPEM)
	check(err)

	for _, kind := range []string{"server", "client"} {
		name := fmt.Sprintf("%s.global.nomad", kind)
		certPEM, keyPEM, err := tlsutil.GenerateCert(tlsutil.CertOpts{
			Signer:      signer,
			CA:          caPEM,
			Name:        name,
			Days:        days,
			DNSNames:    []string{name, "localhost"},
			IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		})
		check(err)
		writeFile(fmt.Sprintf("global-%s-nomad-0.pem", kind), certPEM)
		writeFile(fmt.Sprintf("global-%s-nomad-0-key.pem", kind), keyPEM)
	}
}

func writeFile(path, content string) {
	check(os.WriteFile(path, []byte(content), 0o644))
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
