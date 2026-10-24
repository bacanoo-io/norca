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

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
)

func TestEventStream_Exact(t *testing.T) {
	ci.Parallel(t)

	httpTest(t, func(c *Config) {
		c.Server.EventBufferSize = pointer.Of(3)
	}, func(s *TestAgent) {
		t.Run("invalid parameters", func(t *testing.T) {
			for _, query := range []string{"exact=foo", "exact=true", "exact=true&index=0"} {
				req, err := http.NewRequest(http.MethodGet, "/v1/event/stream?"+query, nil)
				must.NoError(t, err)
				_, err = s.Server.EventStream(httptest.NewRecorder(), req)
				var codedErr HTTPCodedError
				must.True(t, errors.As(err, &codedErr), must.Sprintf("query %q: %v", query, err))
				must.Eq(t, http.StatusBadRequest, codedErr.Code())
			}
		})

		pub, err := s.Agent.server.State().EventBroker()
		must.NoError(t, err)

		// Drop the first events of the topic from the small buffer.
		const first, last = 1_000_001, 1_000_008
		for i := uint64(first); i <= last; i++ {
			pub.Publish(&structs.Events{Index: i, Events: []structs.Event{
				{Topic: "test", Index: i, Payload: testEvent{ID: "x"}},
			}})
		}
		testutil.WaitForResult(func() (bool, error) {
			return pub.Len() == 3, nil
		}, func(err error) { t.Fatalf("buffer not filled: %v", err) })

		t.Run("gone over HTTP", func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, "/v1/event/stream?topic=test&exact=true&index=1000001", nil)
			must.NoError(t, err)
			_, err = s.Server.EventStream(httptest.NewRecorder(), req)
			var codedErr HTTPCodedError
			must.True(t, errors.As(err, &codedErr), must.Sprintf("%v", err))
			must.Eq(t, http.StatusGone, codedErr.Code())
		})

		t.Run("api client", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			topics := map[api.Topic][]string{"test": {"*"}}

			_, err := s.Client().EventStream().StreamExact(ctx, topics, first, nil)
			must.ErrorIs(t, err, api.ErrEventIndexGone)

			eventsCh, err := s.Client().EventStream().StreamExact(ctx, topics, last, nil)
			must.NoError(t, err)
			select {
			case events := <-eventsCh:
				must.NoError(t, events.Err)
				must.Eq(t, last, events.Index)
			case <-ctx.Done():
				t.Fatal("timeout waiting for events")
			}
		})
	})
}
