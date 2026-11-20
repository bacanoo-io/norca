// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

func TestHTTP_Certificates(t *testing.T) {
	ci.Parallel(t)

	httpTest(t, nil, func(s *TestAgent) {
		certs := s.Client().Certificates()

		// No CA before one is asked for.
		roots, _, err := certs.Roots(nil)
		must.NoError(t, err)
		must.Eq(t, "", roots.ActiveID)
		must.Len(t, 0, roots.Authorities)
		_, _, err = certs.CRL("", nil)
		must.ErrorContains(t, err, "no such issuing certificate authority")

		// A rotation creates one, once the keyring is there.
		var rotated *api.CertificateAuthorityResponse
		must.Wait(t, wait.InitialSuccess(
			wait.ErrorFunc(func() error {
				rotated, err = certs.RotateAuthority(nil)
				return err
			}),
			wait.Timeout(20*time.Second),
			wait.Gap(100*time.Millisecond),
		))
		must.True(t, rotated.Authority.Active)
		must.False(t, rotated.Authority.Imported)

		roots, _, err = certs.Roots(nil)
		must.NoError(t, err)
		must.Eq(t, rotated.Authority.ID, roots.ActiveID)
		must.Len(t, 1, roots.Authorities)
		must.Eq(t, rotated.Authority.Cert, roots.Roots)
		block, _ := pem.Decode([]byte(roots.Roots))
		must.NotNil(t, block)
		ca, err := x509.ParseCertificate(block.Bytes)
		must.NoError(t, err)
		must.True(t, ca.IsCA)

		// Its revocation list is signed by it, and empty.
		crl, _, err := certs.CRL("", nil)
		must.NoError(t, err)
		block, _ = pem.Decode([]byte(crl.CRL))
		must.NotNil(t, block)
		list, err := x509.ParseRevocationList(block.Bytes)
		must.NoError(t, err)
		must.NoError(t, list.CheckSignatureFrom(ca))
		must.Len(t, 0, list.RevokedCertificateEntries)
		byID, _, err := certs.CRL(roots.ActiveID, nil)
		must.NoError(t, err)
		must.StrContains(t, byID.CRL, "X509 CRL")

		// What is refused.
		_, err = certs.ImportAuthority(&api.CertificateAuthorityImportRequest{Cert: "x", Key: "y"}, nil)
		must.ErrorContains(t, err, "400")
		_, err = certs.Revoke("missing", nil, nil)
		must.ErrorContains(t, err, "404")
		_, err = certs.Revoke("missing", &api.CertificateRevokeRequest{Reason: "keyCompromise"}, nil)
		must.ErrorContains(t, err, "404")
		_, err = certs.Revoke("", nil, nil)
		must.ErrorContains(t, err, "missing certificate name")

		// A reason that is not one a subscriber may give, or a body that
		// can't be decoded (review finding ATM-25).
		for _, reason := range []string{"caCompromise", "bogus"} {
			_, err = certs.Revoke("missing", &api.CertificateRevokeRequest{Reason: reason}, nil)
			must.ErrorContains(t, err, "400", must.Sprint(reason))
			must.ErrorContains(t, err, "unknown revocation reason", must.Sprint(reason))
		}
		req, err := http.NewRequest(http.MethodPut, "/v1/certificate/web/revoke", strings.NewReader("{"))
		must.NoError(t, err)
		_, err = s.Server.CertificateSpecificRequest(httptest.NewRecorder(), req)
		var coded HTTPCodedError
		must.True(t, errors.As(err, &coded), must.Sprint(err))
		must.Eq(t, http.StatusBadRequest, coded.Code())

		// Only the methods the routes know.
		for path, handler := range map[string]func(http.ResponseWriter, *http.Request) (any, error){
			"/v1/certificates/roots":     s.Server.CertificateRootsRequest,
			"/v1/certificates/crl":       s.Server.CertificateCRLRequest,
			"/v1/certificates/ca/import": s.Server.CertificateAuthorityImportRequest,
			"/v1/certificates/ca/rotate": s.Server.CertificateAuthorityRotateRequest,
			"/v1/certificate/web/revoke": s.Server.CertificateSpecificRequest,
		} {
			req, err := http.NewRequest(http.MethodDelete, path, nil)
			must.NoError(t, err)
			_, err = handler(httptest.NewRecorder(), req)
			must.ErrorContains(t, err, ErrInvalidMethod, must.Sprint(path))
		}
		req, err = http.NewRequest(http.MethodPut, "/v1/certificate/web", nil)
		must.NoError(t, err)
		_, err = s.Server.CertificateSpecificRequest(httptest.NewRecorder(), req)
		must.Error(t, err)
	})
}

// The reason of a revocation reaches the API (review finding ATM-25).
func TestCertificateAuthorityToAPI_Reason(t *testing.T) {
	ci.Parallel(t)

	out := certificateAuthorityToAPI(&structs.CertificateAuthority{
		ID: "ca",
		Revoked: []*structs.CertificateRevocation{
			{Serial: "01", Reason: structs.CertificateRevokeReasonKeyCompromise},
			{Serial: "02"},
		},
	})
	must.Len(t, 2, out.Revoked)
	must.Eq(t, "keyCompromise", out.Revoked[0].Reason)
	must.Eq(t, "", out.Revoked[1].Reason)
}
