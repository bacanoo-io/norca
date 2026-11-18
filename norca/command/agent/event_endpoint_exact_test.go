// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
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

		// Drop the first events of the topic from the small buffer. The
		// state of the server is at the index before the last event, as it
		// would be had the events come from Raft: an exact subscription
		// needs the server to have reached the index before the requested
		// one.
		const first, last = 1_000_001, 1_000_008
		must.NoError(t, s.Agent.server.State().UpsertNamespaces(last-1, []*structs.Namespace{mock.Namespace()}))
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

// An exact subscription at an index the server has not reached is answered
// 410 Gone: the server lost its state (it started again without its data, or
// restored an older snapshot), and the events below the index it will write
// again are not the ones the client has seen. Waiting for the index instead
// would skip them silently (review finding MUK-4).
func TestEventStream_Exact_IndexNotReached(t *testing.T) {
	ci.Parallel(t)

	httpTest(t, nil, func(s *TestAgent) {
		latest, err := s.Agent.server.State().LatestIndex()
		must.NoError(t, err)

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		// the client has seen events up to an index far above the server's
		ahead := latest + 1000
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			fmt.Sprintf("/v1/event/stream?topic=Job&exact=true&index=%d", ahead), nil)
		must.NoError(t, err)
		_, err = s.Server.EventStream(httptest.NewRecorder(), req)
		var codedErr HTTPCodedError
		must.True(t, errors.As(err, &codedErr), must.Sprintf("%v", err))
		must.Eq(t, http.StatusGone, codedErr.Code())
		must.NoError(t, ctx.Err(), must.Sprint("answered only when the request timed out"))

		// the next index of the server is served
		ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel2()
		req, err = http.NewRequestWithContext(ctx2, http.MethodGet,
			fmt.Sprintf("/v1/event/stream?topic=Job&exact=true&index=%d", latest+1), nil)
		must.NoError(t, err)
		_, err = s.Server.EventStream(httptest.NewRecorder(), req)
		must.False(t, errors.As(err, &codedErr), must.Sprintf("%v", err))
	})
}
