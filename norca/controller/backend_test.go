// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/shoenig/test/must"
)

// varServer answers the Variables API like the servers do for one path: a
// delete with check-and-set of a Variable that does not exist is a conflict
// whose body is a Variable without indexes.
type varServer struct {
	mu     sync.Mutex
	stored *api.Variable
}

func (s *varServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		if s.stored == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(s.stored)
	case http.MethodDelete:
		cas := r.URL.Query().Get("cas")
		switch {
		case s.stored == nil && cas != "":
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(&api.Variable{Namespace: "default", Path: "crd/x/K/a"})
		case s.stored != nil && cas != "" && cas != "7":
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(s.stored)
		default:
			s.stored = nil
			w.WriteHeader(http.StatusNoContent)
		}
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func TestAPIBackend_DeleteVariable(t *testing.T) {
	srv := &varServer{}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	client, err := api.NewClient(&api.Config{Address: ts.URL})
	must.NoError(t, err)
	b := NewBackend(client)
	ctx := context.Background()

	// Deleting a missing Variable is not an error, with or without
	// check-and-set (as the VariablesAPI documents and the fake does).
	must.NoError(t, b.DeleteVariable(ctx, "default", "crd/x/K/a", 0))
	must.NoError(t, b.DeleteVariable(ctx, "default", "crd/x/K/a", 7))

	// A Variable that changed is still a conflict.
	srv.stored = &api.Variable{Namespace: "default", Path: "crd/x/K/a", CreateIndex: 5, ModifyIndex: 7,
		Items: api.VariableItems{"spec": "{}"}}
	must.ErrorIs(t, b.DeleteVariable(ctx, "default", "crd/x/K/a", 6), ErrConflict)
	must.NoError(t, b.DeleteVariable(ctx, "default", "crd/x/K/a", 7))
	must.Nil(t, srv.stored)
}
