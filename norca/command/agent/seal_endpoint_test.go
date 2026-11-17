// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
)

// The server of the agent is the default server of an unseal request in the
// region of the agent only: a request to another region has to name its
// server, the name of the local one is unknown there.
func TestHTTP_OperatorUnseal_OtherRegionNeedsServerID(t *testing.T) {
	ci.Parallel(t)

	httpTest(t, nil, func(s *TestAgent) {
		body := func() *operatorUnsealBody { return &operatorUnsealBody{Reset: true} }

		req, err := http.NewRequest(http.MethodPost, "/v1/operator/unseal?region=other", encodeReq(body()))
		must.NoError(t, err)
		_, err = s.Server.OperatorUnsealRequest(httptest.NewRecorder(), req)
		must.ErrorContains(t, err, "server_id is required for a request to another region")
		must.Eq(t, http.StatusBadRequest, err.(HTTPCodedError).Code())

		// a named server is passed on to the region (which does not exist
		// here)
		req, err = http.NewRequest(http.MethodPost, "/v1/operator/unseal?region=other&server_id=x", encodeReq(body()))
		must.NoError(t, err)
		_, err = s.Server.OperatorUnsealRequest(httptest.NewRecorder(), req)
		must.ErrorContains(t, err, "No path to region")

		// the own region, named or not, keeps the default
		for _, query := range []string{"", "?region=" + s.Config.Region} {
			req, err = http.NewRequest(http.MethodPost, "/v1/operator/unseal"+query, encodeReq(body()))
			must.NoError(t, err)
			_, err = s.Server.OperatorUnsealRequest(httptest.NewRecorder(), req)
			if err != nil {
				must.StrNotContains(t, err.Error(), "server_id is required")
				must.StrNotContains(t, err.Error(), "No path to region")
			}
		}
	})
}
