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

// TestCertificateRevokeCommand_Serial checks that -serial reaches the API
// and that the output says which certificate was revoked: an earlier one,
// with nothing issued, or the current one (review finding ATM-24b).
func TestCertificateRevokeCommand_Serial(t *testing.T) {
	ci.Parallel(t)

	var (
		lock   sync.Mutex
		bodies []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		lock.Lock()
		bodies = append(bodies, string(body))
		lock.Unlock()
		in := &api.CertificateRevokeRequest{}
		if len(body) > 0 {
			_ = json.Unmarshal(body, in)
		}
		if in.Serial == "ff" {
			http.Error(w, `certificate "web-tls" has no certificate with the serial ff that can be revoked`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		out := &api.CertificateRevokeResponse{Serial: "0c0d"}
		if in.Serial != "" && in.Serial != "0c0d" {
			out = &api.CertificateRevokeResponse{Serial: "0a1b", Earlier: true}
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	run := func(args ...string) (int, string, string) {
		ui := cli.NewMockUi()
		code := (&CertificateRevokeCommand{Meta: Meta{Ui: ui}}).Run(append([]string{"-address=" + srv.URL}, args...))
		return code, ui.OutputWriter.String(), ui.ErrorWriter.String()
	}

	code, out, errOut := run("-serial", "0A:1B", "-reason=keyCompromise", "web-tls")
	must.Zero(t, code, must.Sprint(errOut))
	must.StrContains(t, out, "earlier certificate with the serial 0a1b")
	must.StrContains(t, out, "the current certificate stays")
	must.StrNotContains(t, out, "a new one is being issued")

	code, out, errOut = run("-serial=0c0d", "web-tls")
	must.Zero(t, code, must.Sprint(errOut))
	must.StrContains(t, out, "serial 0c0d")
	must.StrContains(t, out, "a new one is being issued")

	code, _, errOut = run("-serial=ff", "web-tls")
	must.One(t, code)
	must.StrContains(t, errOut, "has no certificate with the serial")

	lock.Lock()
	defer lock.Unlock()
	must.Eq(t, []string{
		`{"Reason":"keyCompromise","Serial":"0A:1B"}` + "\n",
		`{"Serial":"0c0d"}` + "\n",
		`{"Serial":"ff"}` + "\n",
	}, bodies)
}

// TestCertificateRevokeCommand_Pending checks that the output does not say
// "Revoked" for a certificate its external issuer was only asked to revoke
// (review finding ATM-24g).
func TestCertificateRevokeCommand_Pending(t *testing.T) {
	ci.Parallel(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		in := &api.CertificateRevokeRequest{}
		_ = json.NewDecoder(r.Body).Decode(in)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&api.CertificateRevokeResponse{Serial: "0a1b", Earlier: in.Serial != "", Pending: true})
	}))
	t.Cleanup(srv.Close)
	run := func(args ...string) string {
		ui := cli.NewMockUi()
		code := (&CertificateRevokeCommand{Meta: Meta{Ui: ui}}).Run(append([]string{"-address=" + srv.URL}, args...))
		must.Zero(t, code, must.Sprint(ui.ErrorWriter.String()))
		return ui.OutputWriter.String()
	}

	out := run("-serial=0a1b", "web-tls")
	must.StrNotContains(t, out, "Revoked the")
	must.StrContains(t, out, "Asked the issuer to revoke the earlier certificate with the serial 0a1b")
	must.StrContains(t, out, "the current certificate stays")

	out = run("web-tls")
	must.StrNotContains(t, out, "Revoked the")
	must.StrContains(t, out, "Asked the issuer to revoke the certificate with the serial 0a1b")
	must.StrContains(t, out, "a new one is being issued")
}

// The earlier certificates of a Certificate object are in what "norca object
// get" prints: its status, with their serial numbers and the end of their
// validity (review finding ATM-24b).
func TestFormatObject_CertificateEarlier(t *testing.T) {
	ci.Parallel(t)

	out := formatObject(&api.CustomObject{
		Group: "norca.io", Kind: "Certificate", Namespace: "default", Name: "web-tls",
		Status: map[string]any{
			"serial": "0c0d",
			"earlier": []any{
				map[string]any{"serial": "0a1b", "issuer": "internal", "notAfter": "2027-02-01T00:00:00Z", "revoked": true},
			},
		},
	}, nil)
	must.StrContains(t, out, `"earlier"`)
	must.StrContains(t, out, `"serial": "0a1b"`)
	must.StrContains(t, out, `"notAfter": "2027-02-01T00:00:00Z"`)
	must.StrContains(t, out, `"revoked": true`)
}
