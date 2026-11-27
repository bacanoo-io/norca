// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"

	"github.com/bacanoo-io/norca/norca/helper/tlsutil"
	"github.com/bacanoo-io/norca/norca/norca/structs/config"
)

// clientTLSConfig builds the TLS configuration of a target that does not
// send in plaintext: the CA of the tls block or the system roots, the
// client certificate for mTLS, and the server name. It reads the files, so
// a missing CA file fails the startup.
func clientTLSConfig(t Target) (*tls.Config, error) {
	tc := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: t.Host,
	}
	b := t.TLS
	if b == nil {
		return tc, nil
	}
	if b.ServerName != "" {
		tc.ServerName = b.ServerName
	}
	// an explicit choice of the operator, off by default
	tc.InsecureSkipVerify = b.InsecureSkipVerify
	if b.CAFile != "" {
		pool := x509.NewCertPool()
		if err := (&tlsutil.Config{CAFile: b.CAFile}).AppendCA(pool); err != nil {
			return nil, fmt.Errorf("tls.ca_file: %w", err)
		}
		tc.RootCAs = pool
	}
	if b.CertFile != "" {
		loader := &config.KeyLoader{}
		if _, err := loader.LoadKeyPair(b.CertFile, b.KeyFile); err != nil {
			return nil, fmt.Errorf("tls.cert_file: %w", err)
		}
		tc.GetClientCertificate = loader.GetClientCertificate
	}
	return tc, nil
}
