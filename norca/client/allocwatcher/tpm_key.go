// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package allocwatcher

import (
	"crypto/tls"
	"net/http"
	"os"
	"time"

	nomadapi "github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/helper/tpmkey"
	structsc "github.com/bacanoo-io/norca/norca/norca/structs/config"
	cleanhttp "github.com/hashicorp/go-cleanhttp"
)

// useTPMKey gives the API client of a migration the certificate the agent
// loaded, when the key of the node is in its TPM (a TSS2 key file): the API
// client can not load such a key from its file. Other keys are left to the
// API client.
func useTPMKey(conf *nomadapi.Config, tlsConf *structsc.TLSConfig) error {
	if tlsConf == nil || tlsConf.KeyFile == "" || conf.TLSConfig == nil {
		return nil
	}
	keyPEM, err := os.ReadFile(tlsConf.KeyFile)
	if err != nil || !tpmkey.IsPEM(keyPEM) {
		return nil
	}
	tc := conf.TLSConfig.Copy()
	tc.ClientCert, tc.ClientKey = "", ""
	hc := cleanhttp.DefaultPooledClient()
	tr := hc.Transport.(*http.Transport)
	tr.TLSHandshakeTimeout = 10 * time.Second
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if err := nomadapi.ConfigureTLS(hc, tc); err != nil {
		return err
	}
	kl := tlsConf.GetKeyLoader()
	certFile, keyFile := tlsConf.CertFile, tlsConf.KeyFile
	tr.TLSClientConfig.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		if cert := kl.GetCertificate(); cert != nil {
			return cert, nil
		}
		return kl.LoadKeyPair(certFile, keyFile)
	}
	conf.TLSConfig = tc
	conf.HttpClient = hc
	return nil
}
