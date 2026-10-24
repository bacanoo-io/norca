// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api/internal/testutil"
	"github.com/shoenig/test/must"
)

// TestEventStream_PermissionDenied asserts that the event stream refuses a
// token without permission for the topics with 403 Forbidden.
func TestEventStream_PermissionDenied(t *testing.T) {
	testutil.Parallel(t)

	c, s, _ := makeACLClient(t, nil, nil)
	defer s.Stop()

	token := createTestToken(t, c, "read-job", `namespace "default" { capabilities = ["read-job"] }`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, tc := range []struct {
		name  string
		token string
	}{
		{name: "anonymous", token: ""},
		{name: "no permission for the topic", token: token},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The client's root token would be used without an explicit one.
			cfg := c.config
			cfg.SecretID = tc.token
			cfg.Headers = nil
			client, err := NewClient(&cfg)
			must.NoError(t, err)

			_, err = client.EventStream().Stream(ctx, map[Topic][]string{TopicNode: {"*"}}, 0, nil)
			var respErr UnexpectedResponseError
			must.True(t, errors.As(err, &respErr), must.Sprintf("%v", err))
			must.Eq(t, http.StatusForbidden, respErr.StatusCode())
		})
	}

	t.Run("allowed", func(t *testing.T) {
		ch, err := c.EventStream().Stream(ctx, map[Topic][]string{TopicJob: {"*"}}, 0,
			&QueryOptions{AuthToken: token})
		must.NoError(t, err)
		must.NotNil(t, ch)
	})
}
