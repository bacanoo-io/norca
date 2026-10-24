// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/nomad/api/internal/testutil"
	"github.com/shoenig/test/must"
)

func TestEvent_Variable(t *testing.T) {
	testutil.Parallel(t)

	raw := `{
		"Topic": "Variables",
		"Type": "VariableUpserted",
		"Key": "crd/example/Widget/a",
		"Namespace": "default",
		"Index": 12,
		"Payload": {
			"Variable": {
				"Namespace": "default",
				"Path": "crd/example/Widget/a",
				"CreateIndex": 10,
				"CreateTime": 1700000000000000000,
				"ModifyIndex": 12,
				"ModifyTime": 1700000001000000000
			}
		}
	}`
	var event Event
	must.NoError(t, json.Unmarshal([]byte(raw), &event))
	must.Eq(t, TopicVariables, event.Topic)
	must.Eq(t, "default", event.Namespace)

	meta, err := event.Variable()
	must.NoError(t, err)
	must.Eq(t, &VariableMetadata{
		Namespace:   "default",
		Path:        "crd/example/Widget/a",
		CreateIndex: 10,
		CreateTime:  1700000000000000000,
		ModifyIndex: 12,
		ModifyTime:  1700000001000000000,
	}, meta)
}

func TestEventStream_StreamExact_Gone(t *testing.T) {
	testutil.Parallel(t)

	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusGone)
		_, _ = w.Write([]byte("requested index is no longer available in the event buffer"))
	}))
	defer srv.Close()

	conf := DefaultConfig()
	conf.Address = srv.URL
	c, err := NewClient(conf)
	must.NoError(t, err)

	topics := map[Topic][]string{TopicVariables: {"crd/*"}}

	_, err = c.EventStream().StreamExact(context.Background(), topics, 0, nil)
	must.Error(t, err)

	_, err = c.EventStream().StreamExact(context.Background(), topics, 42, nil)
	must.ErrorIs(t, err, ErrEventIndexGone)
	must.StrContains(t, gotQuery, "exact=true")
	must.StrContains(t, gotQuery, "index=42")
	must.StrContains(t, gotQuery, "topic=Variables%3Acrd%2F")
}
