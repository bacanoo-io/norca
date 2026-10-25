// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

func TestMeta_Client_UnixSocket(t *testing.T) {
	ci.Parallel(t)

	// Short path: sun_path is limited to about 100 bytes.
	dir, err := os.MkdirTemp("", "sock")
	must.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "api.sock")

	ln, err := net.Listen("unix", socket)
	must.NoError(t, err)
	var gotToken string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Nomad-Token")
		if r.URL.Path != "/v1/status/leader" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`"10.0.0.1:4647"`))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	m := &Meta{flagAddress: "unix://" + socket, token: "wi-token"}
	client, err := m.Client()
	must.NoError(t, err)
	leader, err := client.Status().Leader()
	must.NoError(t, err)
	must.Eq(t, "10.0.0.1:4647", leader)
	must.Eq(t, "wi-token", gotToken)

	m = &Meta{flagAddress: "unix://"}
	_, err = m.Client()
	must.ErrorContains(t, err, "no path")
}
