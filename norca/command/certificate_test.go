// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/mitchellh/cli"
	"github.com/shoenig/test/must"
)

// TestCertificateRevokeCommand_Reason checks that -reason reaches the API
// (review finding ATM-25), and that without it no body is sent.
func TestCertificateRevokeCommand_Reason(t *testing.T) {
	ci.Parallel(t)

	var (
		lock   sync.Mutex
		bodies []string
		paths  []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		lock.Lock()
		bodies = append(bodies, string(body))
		paths = append(paths, r.Method+" "+r.URL.Path)
		lock.Unlock()
		in := &api.CertificateRevokeRequest{}
		if len(body) > 0 {
			_ = json.Unmarshal(body, in)
		}
		if in.Reason == "bogus" {
			http.Error(w, `unknown revocation reason "bogus"`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&api.CertificateRevokeResponse{Serial: "0a1b"})
	}))
	t.Cleanup(srv.Close)
	run := func(args ...string) (int, string, string) {
		ui := cli.NewMockUi()
		code := (&CertificateRevokeCommand{Meta: Meta{Ui: ui}}).Run(append([]string{"-address=" + srv.URL}, args...))
		return code, ui.OutputWriter.String(), ui.ErrorWriter.String()
	}

	code, out, errOut := run("-reason", "keyCompromise", "web-tls")
	must.Zero(t, code, must.Sprint(errOut))
	must.StrContains(t, out, "serial 0a1b")

	code, _, errOut = run("web-tls")
	must.Zero(t, code, must.Sprint(errOut))

	code, _, errOut = run("-reason=bogus", "web-tls")
	must.One(t, code)
	must.StrContains(t, errOut, "unknown revocation reason")

	lock.Lock()
	defer lock.Unlock()
	must.Eq(t, []string{
		"PUT /v1/certificate/web-tls/revoke",
		"PUT /v1/certificate/web-tls/revoke",
		"PUT /v1/certificate/web-tls/revoke",
	}, paths)
	must.Eq(t, `{"Reason":"keyCompromise"}`+"\n", bodies[0])
	must.Eq(t, "", bodies[1])
	must.Eq(t, `{"Reason":"bogus"}`+"\n", bodies[2])
}
