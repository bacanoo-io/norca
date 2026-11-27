// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/telemetry/otlptest"
	"github.com/shoenig/test/must"
)

func TestClientTLSConfig(t *testing.T) {
	ci.Parallel(t)

	t.Run("system roots", func(t *testing.T) {
		tc, err := clientTLSConfig(Target{Host: "otlp.example.com"})
		must.NoError(t, err)
		must.Nil(t, tc.RootCAs)
		must.Eq(t, "otlp.example.com", tc.ServerName)
		must.False(t, tc.InsecureSkipVerify)
		must.Nil(t, tc.GetClientCertificate)
	})

	certs := otlptest.NewCerts(t)
	t.Run("ca, server name and client certificate", func(t *testing.T) {
		tc, err := clientTLSConfig(Target{Host: "10.0.0.1", TLS: &TLSConfig{
			CAFile:     certs.CAFile,
			CertFile:   certs.ClientCertFile,
			KeyFile:    certs.ClientKeyFile,
			ServerName: otlptest.ServerName,
		}})
		must.NoError(t, err)
		must.NotNil(t, tc.RootCAs)
		must.Eq(t, otlptest.ServerName, tc.ServerName)
		cert, err := tc.GetClientCertificate(nil)
		must.NoError(t, err)
		must.NotNil(t, cert)
	})

	tests := []struct {
		name    string
		tls     *TLSConfig
		wantErr string
	}{
		{name: "missing ca", tls: &TLSConfig{CAFile: filepath.Join(t.TempDir(), "nope.pem")}, wantErr: "tls.ca_file"},
		{name: "bad ca", tls: &TLSConfig{CAFile: certs.ClientKeyFile}, wantErr: "tls.ca_file"},
		{name: "missing cert", tls: &TLSConfig{CertFile: filepath.Join(t.TempDir(), "nope.pem"), KeyFile: certs.ClientKeyFile}, wantErr: "tls.cert_file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := clientTLSConfig(Target{Host: "h", TLS: tt.tls})
			must.ErrorContains(t, err, tt.wantErr)
		})
	}

	t.Run("not pem", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "ca.pem")
		must.NoError(t, os.WriteFile(path, []byte("not a certificate"), 0o600))
		_, err := clientTLSConfig(Target{Host: "h", TLS: &TLSConfig{CAFile: path}})
		must.ErrorContains(t, err, "tls.ca_file")
	})
}
