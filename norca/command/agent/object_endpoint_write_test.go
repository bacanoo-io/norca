// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

// The cas option of a kind write reaches the server: a kind that exists
// can't be written with cas=0, nor with another modify index.
func TestHTTP_ObjectKind_CAS(t *testing.T) {
	ci.Parallel(t)
	httpTest(t, nil, func(s *TestAgent) {
		kinds := s.Client().ObjectKinds()
		kind := &api.ObjectKind{
			Group:      "db.example.com",
			Kind:       "PostgresCluster",
			Namespaced: true,
			Schema:     testObjectSchema,
		}

		created, _, err := kinds.Register(kind, &api.ObjectWriteOptions{CAS: api.CASIndex(0)}, nil)
		must.NoError(t, err)

		// cas=0: the kind must not exist
		_, _, err = kinds.Register(kind, &api.ObjectWriteOptions{CAS: api.CASIndex(0)}, nil)
		must.Error(t, err)
		must.True(t, errors.Is(err, api.ErrObjectConflict), must.Sprint(err))

		// a stale modify index
		_, _, err = kinds.Register(kind, &api.ObjectWriteOptions{CAS: api.CASIndex(created.ModifyIndex + 1)}, nil)
		must.Error(t, err)
		must.True(t, errors.Is(err, api.ErrObjectConflict), must.Sprint(err))

		// the current modify index
		kind.Columns = []*api.ObjectKindColumn{{Name: "Version", Path: "spec.version"}}
		_, _, err = kinds.Register(kind, &api.ObjectWriteOptions{CAS: api.CASIndex(created.ModifyIndex)}, nil)
		must.NoError(t, err)

		// a cas value that is not a number
		req, err := http.NewRequest(http.MethodPut, "/v1/object-kind/db.example.com/PostgresCluster?cas=x", encodeReq(kind))
		must.NoError(t, err)
		_, err = s.Server.ObjectKindSpecificRequest(httptest.NewRecorder(), req)
		must.ErrorContains(t, err, "cas")
	})
}

// A raw object write without ?namespace= goes to the namespace of the object
// in the body; with the parameter the two have to agree.
func TestHTTP_ObjectApply_BodyNamespace(t *testing.T) {
	ci.Parallel(t)
	httpTest(t, nil, func(s *TestAgent) {
		client := s.Client()
		_, err := client.Namespaces().Register(&api.Namespace{Name: "prod"}, nil)
		must.NoError(t, err)
		_, _, err = client.ObjectKinds().Register(&api.ObjectKind{
			Group:      "db.example.com",
			Kind:       "PostgresCluster",
			Namespaced: true,
			Schema:     testObjectSchema,
		}, nil, nil)
		must.NoError(t, err)

		body := func() *api.CustomObject {
			return &api.CustomObject{
				Group:     "db.example.com",
				Kind:      "PostgresCluster",
				Name:      "billing",
				Namespace: "prod",
				Spec:      map[string]any{"version": "16"},
			}
		}
		const path = "/v1/object/db.example.com/PostgresCluster/billing"

		req, err := http.NewRequest(http.MethodPut, path, encodeReq(body()))
		must.NoError(t, err)
		out, err := s.Server.ObjectSpecificRequest(httptest.NewRecorder(), req)
		must.NoError(t, err)
		must.Eq(t, "prod", out.(*structs.CustomObject).Namespace)

		req, err = http.NewRequest(http.MethodPut, path+"?namespace=default", encodeReq(body()))
		must.NoError(t, err)
		_, err = s.Server.ObjectSpecificRequest(httptest.NewRecorder(), req)
		must.ErrorContains(t, err, "does not match the request namespace")

		// no namespace anywhere: the default namespace
		obj := body()
		obj.Namespace = ""
		req, err = http.NewRequest(http.MethodPut, path, encodeReq(obj))
		must.NoError(t, err)
		out, err = s.Server.ObjectSpecificRequest(httptest.NewRecorder(), req)
		must.NoError(t, err)
		must.Eq(t, structs.DefaultNamespace, out.(*structs.CustomObject).Namespace)
	})
}
