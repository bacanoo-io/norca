// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs/config"
	"github.com/shoenig/test/must"
)

// TestHTTP_HTTP2 checks that the HTTPS API speaks HTTP/2, and that an HTTP/2
// connection outlives the handshake timeout: it never reports StateActive to
// ConnState, which clears the deadline of an HTTP/1.1 connection; net/http
// clears it when it hands the connection to HTTP/2. HTTP/1.1 clients (and
// WebSocket upgrades) keep working.
func TestHTTP_HTTP2(t *testing.T) {
	ci.Parallel(t)
	const (
		cafile  = "../../helper/tlsutil/testdata/norca-agent-ca.pem"
		foocert = "../../helper/tlsutil/testdata/regionFoo-server-norca.pem"
		fookey  = "../../helper/tlsutil/testdata/regionFoo-server-norca-key.pem"
		timeout = 300 * time.Millisecond
	)
	s := makeHTTPServer(t, func(c *Config) {
		c.Region = "regionFoo" // match the region on foocert
		c.TLSConfig = &config.TLSConfig{
			EnableHTTP: true,
			CAFile:     cafile,
			CertFile:   foocert,
			KeyFile:    fookey,
		}
		c.Limits.HTTPSHandshakeTimeout = timeout.String()
		c.LogLevel = "off"
	})
	defer s.Shutdown()

	pem, err := os.ReadFile(cafile)
	must.NoError(t, err)
	pool := x509.NewCertPool()
	must.True(t, pool.AppendCertsFromPEM(pem))
	reqURL := fmt.Sprintf("https://%s/v1/agent/self", s.Agent.config.AdvertiseAddrs.HTTP)

	// get makes a request and returns its protocol and whether it went over
	// a connection used before.
	get := func(t *testing.T, client *http.Client) (int, bool) {
		var reused bool
		trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }}
		req, err := http.NewRequest(http.MethodGet, reqURL, nil)
		must.NoError(t, err)
		resp, err := client.Do(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
		must.NoError(t, err)
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		must.EqOp(t, http.StatusOK, resp.StatusCode)
		return resp.ProtoMajor, reused
	}
	tlsConf := func() *tls.Config { return &tls.Config{RootCAs: pool, ServerName: "server.regionFoo.norca"} }

	t.Run("http2", func(t *testing.T) {
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConf(), ForceAttemptHTTP2: true}}
		defer client.CloseIdleConnections()
		proto, _ := get(t, client)
		must.EqOp(t, 2, proto)

		// Past the handshake timeout the same connection still serves.
		time.Sleep(3 * timeout)
		proto, reused := get(t, client)
		must.EqOp(t, 2, proto)
		must.True(t, reused, must.Sprint("the HTTP/2 connection was closed at the handshake timeout"))
	})

	t.Run("http1", func(t *testing.T) {
		// A non-nil, empty TLSNextProto turns HTTP/2 off in the client.
		client := &http.Client{Transport: &http.Transport{
			TLSClientConfig: tlsConf(),
			TLSNextProto:    map[string]func(string, *tls.Conn) http.RoundTripper{},
		}}
		defer client.CloseIdleConnections()
		proto, _ := get(t, client)
		must.EqOp(t, 1, proto)

		time.Sleep(3 * timeout)
		proto, reused := get(t, client)
		must.EqOp(t, 1, proto)
		must.True(t, reused, must.Sprint("the HTTP/1.1 connection was closed at the handshake timeout"))
	})
}

func TestHTTP2CipherSuites(t *testing.T) {
	ci.Parallel(t)
	must.True(t, http2CipherSuites(nil))
	must.True(t, http2CipherSuites([]uint16{
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256, tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
	}))
	must.False(t, http2CipherSuites([]uint16{
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256, tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
	}))
	must.False(t, http2CipherSuites([]uint16{tls.TLS_RSA_WITH_AES_128_GCM_SHA256}))
}
