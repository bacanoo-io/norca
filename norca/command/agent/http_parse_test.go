// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/shoenig/test/must"
)

// A handler stops when parse answered the request with an error: with
// ?stale&consistent the answer is 400, and the request is not run. Without
// the stop these requests go on to the RPC, which fails here with an unknown
// allocation (and on a real allocation restarts, signals or collects it).
func TestHTTP_ParseErrorStopsHandler(t *testing.T) {
	ci.Parallel(t)

	httpTest(t, nil, func(s *TestAgent) {
		allocID := uuid.Generate()
		cases := []struct {
			method, path string
		}{
			{http.MethodPut, fmt.Sprintf("/v1/client/allocation/%s/restart", allocID)},
			{http.MethodPut, fmt.Sprintf("/v1/client/allocation/%s/signal", allocID)},
			{http.MethodGet, fmt.Sprintf("/v1/client/allocation/%s/gc", allocID)},
			{http.MethodGet, fmt.Sprintf("/v1/client/allocation/%s/stats", allocID)},
		}
		for _, tc := range cases {
			req, err := http.NewRequest(tc.method, tc.path+"?stale&consistent", encodeReq(map[string]string{}))
			must.NoError(t, err)
			respW := httptest.NewRecorder()
			out, err := s.Server.ClientAllocRequest(respW, req)
			must.NoError(t, err, must.Sprint(tc.path))
			must.Nil(t, out, must.Sprint(tc.path))
			must.Eq(t, http.StatusBadRequest, respW.Code, must.Sprint(tc.path))
			must.StrContains(t, respW.Body.String(), "Cannot specify both")
		}

		// the event stream does not start under the 400 either
		req, err := http.NewRequest(http.MethodGet, "/v1/event/stream?stale&consistent", nil)
		must.NoError(t, err)
		respW := httptest.NewRecorder()
		out, err := s.Server.EventStream(respW, req)
		must.NoError(t, err)
		must.Nil(t, out)
		must.Eq(t, http.StatusBadRequest, respW.Code)
		must.Eq(t, "Cannot specify both `stale` and `consistent` query string parameters", respW.Body.String())
	})
}
