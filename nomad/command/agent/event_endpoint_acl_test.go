// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

// TestEventStream_ACLStatusCodes asserts that the event stream reports a
// token that is missing, unknown or lacks the permissions for the requested
// topics as 403 Forbidden.
func TestEventStream_ACLStatusCodes(t *testing.T) {
	ci.Parallel(t)

	httpACLTest(t, nil, func(s *TestAgent) {
		state := s.Agent.server.State()
		otherNs := mock.CreatePolicyAndToken(t, state, 1005, "other-ns",
			mock.NamespacePolicy("other", "", []string{acl.NamespaceCapabilityReadJob}))
		readJob := mock.CreatePolicyAndToken(t, state, 1006, "read-job",
			mock.NamespacePolicy(structs.DefaultNamespace, "", []string{acl.NamespaceCapabilityReadJob}))

		cases := []struct {
			name  string
			token string
			query string
			code  int
		}{
			{name: "anonymous", query: "topic=Job", code: http.StatusForbidden},
			{name: "unknown token", token: uuid.Generate(), query: "topic=Job", code: http.StatusForbidden},
			{name: "invalid token", token: "not-a-token", query: "topic=Job", code: http.StatusForbidden},
			{name: "missing permission", token: otherNs.SecretID, query: "topic=Job", code: http.StatusForbidden},
			{name: "management topic", token: readJob.SecretID, query: "topic=Node", code: http.StatusForbidden},
			{name: "exact without index", token: s.RootToken.SecretID, query: "topic=Job&exact=true", code: http.StatusBadRequest},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, "/v1/event/stream?"+tc.query, nil)
				must.NoError(t, err)
				if tc.token != "" {
					req.Header.Set("X-Nomad-Token", tc.token)
				}
				_, err = s.Server.EventStream(httptest.NewRecorder(), req)
				var codedErr HTTPCodedError
				must.True(t, errors.As(err, &codedErr), must.Sprintf("%v", err))
				must.Eq(t, tc.code, codedErr.Code(), must.Sprintf("%v", err))
			})
		}

		t.Run("allowed", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "/v1/event/stream?topic=Job", nil)
			must.NoError(t, err)
			setToken(req, readJob)

			errCh := make(chan error, 1)
			go func() {
				_, err := s.Server.EventStream(httptest.NewRecorder(), req)
				errCh <- err
			}()
			select {
			case err := <-errCh:
				t.Fatalf("stream ended early: %v", err)
			case <-time.After(500 * time.Millisecond):
			}
			cancel()
			select {
			case err := <-errCh:
				must.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("timeout waiting for the stream to end")
			}
		})
	})
}
