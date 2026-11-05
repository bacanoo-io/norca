// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/tlsutil"
	"github.com/bacanoo-io/norca/norca/norca/structs/config"
	"github.com/shoenig/test/must"
)

func readTestCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	data, err := os.ReadFile(path)
	must.NoError(t, err)
	block, _ := pem.Decode(data)
	must.NotNil(t, block)
	cert, err := x509.ParseCertificate(block.Bytes)
	must.NoError(t, err)
	return cert
}

// TestServer_RPCDistrustedCAWithHostnameVerification checks that the check of
// the name and region of an RPC peer (verify_server_hostname) comes after the
// distrusted CAs of the TLS configuration and does not replace it: in the lab
// the nodes of a distrusted node CA kept their connections because it did.
func TestServer_RPCDistrustedCAWithHostnameVerification(t *testing.T) {
	ci.Parallel(t)
	const (
		cafile  = "../helper/tlsutil/testdata/norca-agent-ca.pem"
		badca   = "../helper/tlsutil/testdata/bad-agent-ca.pem"
		foocert = "../helper/tlsutil/testdata/regionFoo-client-norca.pem"
		fookey  = "../helper/tlsutil/testdata/regionFoo-client-norca-key.pem"
	)
	root := readTestCert(t, cafile)
	leaf := readTestCert(t, foocert)
	chains := [][]*x509.Certificate{{leaf, root}}

	incoming := func(distrusted string) *tlsutilVerifier {
		t.Helper()
		conf := &config.TLSConfig{
			EnableRPC: true, VerifyServerHostname: true,
			CAFile: cafile, CertFile: foocert, KeyFile: fookey, DistrustedCAFile: distrusted,
		}
		tc, err := tlsutil.NewTLSConfiguration(conf, true, true)
		must.NoError(t, err)
		itls, _, err := getTLSConf(true, tc, "regionFoo")
		must.NoError(t, err)
		return &tlsutilVerifier{itls.VerifyPeerCertificate}
	}

	// Another CA is distrusted: the client of the region passes, one of
	// another region does not (the name check still runs).
	v := incoming(badca)
	must.NoError(t, v.check(chains))
	other := readTestCert(t, "../helper/tlsutil/testdata/global-client-norca.pem")
	must.ErrorContains(t, v.check([][]*x509.Certificate{{other, root}}), "invalid role or region")

	// The CA of the chain is distrusted: refused, although the name fits.
	v = incoming(cafile)
	must.ErrorContains(t, v.check(chains), "distrusted CA")
}

type tlsutilVerifier struct {
	fn func([][]byte, [][]*x509.Certificate) error
}

func (v *tlsutilVerifier) check(chains [][]*x509.Certificate) error {
	return v.fn(nil, chains)
}
