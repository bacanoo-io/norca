// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package allocwatcher

import (
	"crypto/tls"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	nomadapi "github.com/bacanoo-io/norca/norca/api"
	structsc "github.com/bacanoo-io/norca/norca/norca/structs/config"
)

// The API client of a migration loads a key file itself; a key in the TPM
// it takes from the agent.
func TestUseTPMKey(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain.pem")
	tss2 := filepath.Join(dir, "tss2.pem")
	if err := os.WriteFile(plain, []byte("-----BEGIN EC PRIVATE KEY-----\nAAAA\n-----END EC PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tss2, []byte("-----BEGIN TSS2 PRIVATE KEY-----\nAAAA\n-----END TSS2 PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	newConf := func(key string) *nomadapi.Config {
		return &nomadapi.Config{TLSConfig: &nomadapi.TLSConfig{ClientCert: "cert.pem", ClientKey: key, TLSServerName: "client.global.norca"}}
	}

	conf := newConf(plain)
	if err := useTPMKey(conf, &structsc.TLSConfig{CertFile: "cert.pem", KeyFile: plain}); err != nil {
		t.Fatal(err)
	}
	if conf.HttpClient != nil || conf.TLSConfig.ClientKey != plain {
		t.Fatal("a key file was taken from the API client")
	}

	tlsConf := &structsc.TLSConfig{CertFile: filepath.Join(dir, "cert.pem"), KeyFile: tss2}
	conf = newConf(tss2)
	if err := useTPMKey(conf, tlsConf); err != nil {
		t.Fatal(err)
	}
	if conf.HttpClient == nil || conf.TLSConfig.ClientKey != "" || conf.TLSConfig.ClientCert != "" {
		t.Fatalf("the API client keeps the TPM key file: %+v", conf.TLSConfig)
	}
	tc := conf.HttpClient.Transport.(*http.Transport).TLSClientConfig
	if tc.ServerName != "client.global.norca" {
		t.Fatalf("server name %q", tc.ServerName)
	}
	// The certificate comes from the key loader of the agent, which loads
	// the TPM key pair (here a key file that is no key: an error).
	if _, err := tc.GetClientCertificate(&tls.CertificateRequestInfo{}); err == nil ||
		!strings.Contains(err.Error(), "TPM key pair") {
		t.Fatalf("the client certificate does not come from the key loader: %v", err)
	}
}
