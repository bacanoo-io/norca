// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package externalsvc

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func TestRun_HTTP(t *testing.T) {
	ci.Parallel(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			must.Eq(t, "yes", r.Header.Get("X-Check"))
			w.WriteHeader(http.StatusNoContent)
		case "/busy":
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("down"))
		}
	}))
	t.Cleanup(srv.Close)

	check := func(path string) (string, string) {
		return Run(context.Background(), &structs.ExternalServiceCheck{
			Type: structs.ExternalCheckTypeHTTP, HTTP: srv.URL + path, Timeout: time.Second,
			Header: map[string][]string{"X-Check": {"yes"}},
		})
	}
	status, _ := check("/ok")
	must.Eq(t, structs.ServiceHealthPassing, status)
	status, _ = check("/busy")
	must.Eq(t, structs.ServiceHealthWarning, status)
	status, output := check("/down")
	must.Eq(t, structs.ServiceHealthCritical, status)
	must.StrContains(t, output, "503")
	must.StrContains(t, output, "down")
}

func TestRun_TCP(t *testing.T) {
	ci.Parallel(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must.NoError(t, err)
	addr := ln.Addr().String()

	def := &structs.ExternalServiceCheck{Type: structs.ExternalCheckTypeTCP, TCP: addr, Timeout: time.Second}
	status, _ := Run(context.Background(), def)
	must.Eq(t, structs.ServiceHealthPassing, status)

	ln.Close()
	status, _ = Run(context.Background(), def)
	must.Eq(t, structs.ServiceHealthCritical, status)
}
