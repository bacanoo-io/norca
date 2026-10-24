// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

const testObjectSchema = `{
  "type": "object",
  "properties": {
    "spec": {
      "type": "object",
      "required": ["version"],
      "properties": {
        "version":  {"type": "string"},
        "replicas": {"type": "integer", "default": 1}
      }
    }
  }
}`

func TestHTTP_Objects(t *testing.T) {
	ci.Parallel(t)
	httpTest(t, nil, func(s *TestAgent) {
		client := s.Client()

		kind := &api.ObjectKind{
			Group:      "db.example.com",
			Kind:       "PostgresCluster",
			Namespaced: true,
			Schema:     testObjectSchema,
			Rules: []*api.ObjectKindRule{{
				Expression: "!has(oldSelf) || self.spec.replicas >= oldSelf.spec.replicas",
				Message:    "Replicas cannot be reduced.",
			}},
			Columns: []*api.ObjectKindColumn{{Name: "Version", Path: "spec.version"}},
		}

		// Kinds: dry run, create, read, list.
		out, _, err := client.ObjectKinds().Register(kind, &api.ObjectWriteOptions{DryRun: true}, nil)
		must.NoError(t, err)
		must.Eq(t, uint64(0), out.CreateIndex)
		_, _, err = client.ObjectKinds().Info("db.example.com", "PostgresCluster", nil)
		must.Error(t, err)

		out, wm, err := client.ObjectKinds().Register(kind, nil, nil)
		must.NoError(t, err)
		must.NonZero(t, wm.LastIndex)
		must.Eq(t, wm.LastIndex, out.CreateIndex)

		info, _, err := client.ObjectKinds().Info("db.example.com", "PostgresCluster", nil)
		must.NoError(t, err)
		must.Eq(t, "spec.version", info.Columns[0].Path)
		kinds, _, err := client.ObjectKinds().List(nil)
		must.NoError(t, err)
		must.Len(t, 1, kinds)

		// Objects: create with defaults, read, list with labels.
		obj := &api.CustomObject{
			Group:  "db.example.com",
			Kind:   "PostgresCluster",
			Name:   "billing",
			Labels: map[string]string{"team": "finance"},
			Spec:   map[string]any{"version": "16", "replicas": 2},
		}
		created, wm, err := client.Objects().Apply(obj, nil, nil)
		must.NoError(t, err)
		must.Eq(t, uint64(1), created.Generation)
		must.Eq(t, "default", created.Namespace)
		must.Eq(t, wm.LastIndex, created.ModifyIndex)

		got, _, err := client.Objects().Get("db.example.com", "PostgresCluster", "billing", nil)
		must.NoError(t, err)
		must.Eq[any](t, float64(2), got.Spec["replicas"])
		v, err := got.Field("spec.version")
		must.NoError(t, err)
		must.Eq[any](t, "16", v)

		list, _, err := client.Objects().List("db.example.com", "PostgresCluster",
			&api.ObjectListOptions{Labels: map[string]string{"team": "finance"}}, nil)
		must.NoError(t, err)
		must.Len(t, 1, list)
		list, _, err = client.Objects().List("db.example.com", "PostgresCluster",
			&api.ObjectListOptions{Labels: map[string]string{"team": "web"}}, nil)
		must.NoError(t, err)
		must.Len(t, 0, list)

		// Status subresource; the generation stays.
		status := map[string]any{
			"observedGeneration": 1,
			"conditions":         []any{map[string]any{"type": "Ready", "status": "True"}},
		}
		updated, _, err := client.Objects().UpdateStatus("db.example.com", "PostgresCluster", "billing", status,
			&api.ObjectWriteOptions{CAS: api.CASIndex(created.ModifyIndex)}, nil)
		must.NoError(t, err)
		must.Eq(t, uint64(1), updated.Generation)
		must.Eq(t, uint64(1), updated.ObservedGeneration())
		must.Eq(t, "True", updated.Condition(api.ObjectConditionReady).Status)

		// A stale cas index is a 409 Conflict.
		obj.Spec = map[string]any{"version": "16", "replicas": 3}
		_, _, err = client.Objects().Apply(obj, &api.ObjectWriteOptions{CAS: api.CASIndex(created.ModifyIndex)}, nil)
		must.True(t, errors.Is(err, api.ErrObjectConflict))
		var respErr api.UnexpectedResponseError
		must.True(t, errors.As(err, &respErr))
		must.Eq(t, http.StatusConflict, respErr.StatusCode())

		// Spec change with the current index; the generation increases.
		changed, _, err := client.Objects().Apply(obj, &api.ObjectWriteOptions{CAS: api.CASIndex(updated.ModifyIndex)}, nil)
		must.NoError(t, err)
		must.Eq(t, uint64(2), changed.Generation)
		must.Eq(t, uint64(1), changed.ObservedGeneration())

		// Validation errors are 400 Bad Request.
		obj.Spec = map[string]any{"version": "16", "replicas": 1}
		_, _, err = client.Objects().Apply(obj, nil, nil)
		must.ErrorContains(t, err, "Replicas cannot be reduced.")
		must.True(t, errors.As(err, &respErr))
		must.Eq(t, http.StatusBadRequest, respErr.StatusCode())

		// Dry run of a delete, then delete.
		_, _, err = client.Objects().Delete("db.example.com", "PostgresCluster", "billing", &api.ObjectWriteOptions{DryRun: true}, nil)
		must.NoError(t, err)
		_, _, err = client.Objects().Get("db.example.com", "PostgresCluster", "billing", nil)
		must.NoError(t, err)
		_, _, err = client.Objects().Delete("db.example.com", "PostgresCluster", "billing", nil, nil)
		must.NoError(t, err)
		_, _, err = client.Objects().Get("db.example.com", "PostgresCluster", "billing", nil)
		must.ErrorContains(t, err, "404")

		_, err = client.ObjectKinds().Delete("db.example.com", "PostgresCluster", nil)
		must.NoError(t, err)
	})
}

func TestHTTP_Objects_Paths(t *testing.T) {
	ci.Parallel(t)
	httpTest(t, nil, func(s *TestAgent) {
		cases := []struct {
			method, path string
			handler      func(http.ResponseWriter, *http.Request) (any, error)
			code         int
		}{
			{http.MethodGet, "/v1/object-kind/onlygroup", s.Server.ObjectKindSpecificRequest, http.StatusBadRequest},
			{http.MethodPatch, "/v1/object-kind/g/K", s.Server.ObjectKindSpecificRequest, http.StatusMethodNotAllowed},
			{http.MethodPost, "/v1/object-kinds", s.Server.ObjectKindsRequest, http.StatusMethodNotAllowed},
			{http.MethodGet, "/v1/objects/g", s.Server.ObjectsRequest, http.StatusBadRequest},
			{http.MethodGet, "/v1/objects/g/K?label=nokey", s.Server.ObjectsRequest, http.StatusBadRequest},
			{http.MethodGet, "/v1/object/g/K", s.Server.ObjectSpecificRequest, http.StatusBadRequest},
			{http.MethodGet, "/v1/object/g/K/n/status", s.Server.ObjectSpecificRequest, http.StatusMethodNotAllowed},
			{http.MethodDelete, "/v1/object/g/K/n?cas=x", s.Server.ObjectSpecificRequest, http.StatusBadRequest},
			{http.MethodGet, "/v1/object/g/K/n/other", s.Server.ObjectSpecificRequest, http.StatusNotFound},
			{http.MethodDelete, "/v1/object/g/K/n/scale", s.Server.ObjectSpecificRequest, http.StatusMethodNotAllowed},
			{http.MethodGet, "/v1/object/g/K/n/finalizers", s.Server.ObjectSpecificRequest, http.StatusMethodNotAllowed},
		}
		for _, tc := range cases {
			req, err := http.NewRequest(tc.method, tc.path, nil)
			must.NoError(t, err)
			_, err = tc.handler(httptest.NewRecorder(), req)
			must.Error(t, err, must.Sprint(tc.path))
			var coded HTTPCodedError
			must.True(t, errors.As(err, &coded), must.Sprint(tc.path))
			must.Eq(t, tc.code, coded.Code(), must.Sprint(tc.path))
		}

		// The body must match the path.
		body := encodeReq(&structs.CustomObject{Group: "g.example.com", Kind: "K", Name: "other"})
		req, err := http.NewRequest(http.MethodPut, "/v1/object/g.example.com/K/n", body)
		must.NoError(t, err)
		_, err = s.Server.ObjectSpecificRequest(httptest.NewRecorder(), req)
		must.ErrorContains(t, err, "does not match")
	})
}

func TestHTTP_ObjectLifecycle(t *testing.T) {
	ci.Parallel(t)
	httpTest(t, nil, func(s *TestAgent) {
		client := s.Client()
		const group, kindName = "db.example.com", "PostgresCluster"

		kind := &api.ObjectKind{
			Group:      group,
			Kind:       kindName,
			Namespaced: true,
			Schema:     testObjectSchema,
			Scale:      &api.ObjectKindScale{SpecPath: "spec.replicas"},
		}
		_, _, err := client.ObjectKinds().Register(kind, nil, nil)
		must.NoError(t, err)

		obj := &api.CustomObject{Group: group, Kind: kindName, Name: "billing",
			Spec: map[string]any{"version": "16"}, Finalizers: []string{"example.com/cleanup"}}
		created, _, err := client.Objects().Apply(obj, nil, nil)
		must.NoError(t, err)
		must.Eq(t, []string{"example.com/cleanup"}, created.Finalizers)

		// Events.
		_, err = client.Objects().RecordEvents(group, kindName, "billing", []*api.ObjectEvent{
			{Type: api.ObjectEventTypeWarning, Reason: "BackupFailed", Message: "disk full"}}, nil)
		must.NoError(t, err)
		events, _, err := client.Objects().Events(group, kindName, "billing", nil)
		must.NoError(t, err)
		must.Len(t, 1, events)
		must.Eq(t, "disk full", events[0].Message)
		must.Eq(t, 1, events[0].Count)

		// Scale.
		scale, _, err := client.Objects().Scale(group, kindName, "billing", nil)
		must.NoError(t, err)
		must.Eq(t, int64(1), scale.Replicas)
		scale, _, err = client.Objects().SetScale(group, kindName, "billing", 3, "more", nil, nil)
		must.NoError(t, err)
		must.Eq(t, int64(3), scale.Replicas)
		events, _, err = client.Objects().Events(group, kindName, "billing", nil)
		must.NoError(t, err)
		must.Eq(t, api.ObjectScaleReason, events[len(events)-1].Reason)
		must.Eq(t, "more", events[len(events)-1].Message)

		// Deletion with a finalizer marks the object.
		deleted, _, err := client.Objects().Delete(group, kindName, "billing",
			&api.ObjectWriteOptions{Propagation: api.ObjectPropagationForeground}, nil)
		must.NoError(t, err)
		must.True(t, deleted.Deleting())
		must.Eq(t, api.ObjectPropagationForeground, deleted.DeletionPropagation)
		_, _, err = client.Objects().UpdateFinalizers(group, kindName, "billing", []string{"example.com/more"}, nil, nil, nil)
		must.ErrorContains(t, err, "409")

		got, _, err := client.Objects().UpdateFinalizers(group, kindName, "billing", nil, []string{"example.com/cleanup"}, nil, nil)
		must.NoError(t, err)
		must.Len(t, 0, got.Finalizers)

		// Without finalizers the foreground deletion completes once the
		// garbage collector found no dependents.
		must.Wait(t, wait.InitialSuccess(
			wait.BoolFunc(func() bool {
				_, _, err := client.Objects().Get(group, kindName, "billing", nil)
				return err != nil
			}),
			wait.Timeout(30*time.Second),
			wait.Gap(100*time.Millisecond),
		))
		_, _, err = client.Objects().Delete(group, kindName, "billing", &api.ObjectWriteOptions{Propagation: "sideways"}, nil)
		must.ErrorContains(t, err, "400")
	})
}