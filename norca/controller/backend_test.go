// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// jobServer answers job registrations like the servers do: a registration
// with EnforceIndex whose JobModifyIndex is not the stored one is refused
// with an error that starts with api.RegisterEnforceIndexErrPrefix.
type jobServer struct {
	mu     sync.Mutex
	stored uint64 // JobModifyIndex of the stored job, 0: none
	deny   bool
}

func (s *jobServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Method != http.MethodPut || r.URL.Path != "/v1/jobs" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req api.JobRegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	switch {
	case s.deny:
		http.Error(w, "Permission denied", http.StatusForbidden)
	case req.EnforceIndex && req.JobModifyIndex != s.stored:
		http.Error(w, fmt.Sprintf("%s %d: job exists with conflicting job modify index: %d",
			api.RegisterEnforceIndexErrPrefix, req.JobModifyIndex, s.stored), http.StatusInternalServerError)
	default:
		s.stored += 10
		_ = json.NewEncoder(w).Encode(&api.JobRegisterResponse{JobModifyIndex: s.stored})
	}
}

func (s *jobServer) index() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stored
}

func TestAPIBackend_EnforceRegisterJob(t *testing.T) {
	srv := &jobServer{}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	client, err := api.NewClient(&api.Config{Address: ts.URL})
	must.NoError(t, err)
	b := NewBackend(client)
	ctx := context.Background()
	id := "web"
	job := &api.Job{ID: &id}

	// Absent: a registration that expects a job is a conflict.
	must.ErrorIs(t, b.EnforceRegisterJob(ctx, job, 3), ErrConflict)
	must.NoError(t, b.EnforceRegisterJob(ctx, job, 0))
	must.Eq(t, 10, srv.index())

	// Present: no job expected, or another index, is a conflict.
	err = b.EnforceRegisterJob(ctx, job, 0)
	must.ErrorIs(t, err, ErrConflict)
	must.StrContains(t, err.Error(), api.RegisterEnforceIndexErrPrefix)
	must.ErrorIs(t, b.EnforceRegisterJob(ctx, job, 9), ErrConflict)
	must.NoError(t, b.EnforceRegisterJob(ctx, job, 10))
	must.Eq(t, 20, srv.index())

	// Other errors are not conflicts.
	srv.mu.Lock()
	srv.deny = true
	srv.mu.Unlock()
	err = b.EnforceRegisterJob(ctx, job, 20)
	must.Error(t, err)
	must.False(t, errors.Is(err, ErrConflict))
	must.Eq(t, http.StatusForbidden, statusCode(err))
}
