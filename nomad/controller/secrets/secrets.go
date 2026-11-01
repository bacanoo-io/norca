// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package secrets serves the operator side of the dynamic secrets of Norca:
// an operator is a service that creates the credentials of its target system
// (a database, a broker, a certificate authority) on request. The leader of
// the region keeps the leases and calls the operator; the operator talks to
// the target system.
//
// An operator implements Backend and serves it with its workload certificate:
//
//	func main() {
//		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
//		defer stop()
//		err := secrets.Serve(ctx, ":8443", secrets.TLSFromEnv(), &postgres{})
//		...
//	}
//
// The job of the operator needs a service with mtls { mode = "app" } and its
// admin credential as an ordinary secret block. The engine names the service:
//
//	type = "operator"
//	operator { service = "postgres-operator" }
//	role "app" {
//	  ttl    = "1h"
//	  config = { database = "app" }
//	}
//
// The rules of the contract:
//
//   - Issue is idempotent by the lease ID: the leader may repeat it after a
//     failure, and a credential must not be created twice.
//   - Revoke gets every revoked credential that has not expired yet, again
//     with each change and periodically. It must be idempotent and accept
//     unknown references. A reference is the lease ID when the issue did not
//     finish, so keep the lease ID with what you create.
//   - The items are never logged by this package. Don't log them either.
//   - Keep no state the leader needs: after a failure, the leases of the
//     leader are the truth.
package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// The paths of the contract.
const (
	PathIssue  = "/issue"
	PathRenew  = "/renew"
	PathRevoke = "/revoke"
	PathHealth = "/health"
)

// maxRequest bounds a request: a revocation list can be long, the others are
// small.
const maxRequest = 16 << 20

// IssueRequest asks for a credential.
type IssueRequest struct {
	Namespace string
	Engine    string
	Role      string

	// Config is the configuration of the role.
	Config map[string]string

	// Params are the params of the job's secret block. The role allows
	// their keys; check their values.
	Params map[string]string

	// LeaseID makes the call idempotent.
	LeaseID string

	// The task the credential is for.
	JobID     string
	TaskGroup string
	Task      string
	AllocID   string

	// TTL is how long the lease lasts without a renewal.
	TTL time.Duration

	// MaxExpire is when the lease ends at the latest.
	MaxExpire time.Time
}

// Credential is the answer of Issue.
type Credential struct {
	// Items are the values the job maps to its files and environment. Must
	// never be logged.
	Items map[string]string

	// Ref identifies the credential in the target system; it is not secret
	// and is stored by the leader. The lease ID is used without one.
	Ref string

	// Expires is when the credential ends; zero if the lease decides.
	Expires time.Time

	// Renewable false means the credential can't be extended, for example
	// a certificate: the task gets a new one in time.
	Renewable *bool `json:",omitempty"`
}

// RenewRequest extends a credential.
type RenewRequest struct {
	LeaseID string
	Ref     string
	TTL     time.Duration
}

// Renewal is the answer of Renew.
type Renewal struct {
	// Expires is when the credential ends; zero if the lease decides.
	Expires time.Time

	// Items are new values, if the renewal replaced the credential. The
	// task gets them like a new version of the secret.
	Items map[string]string `json:",omitempty"`
}

// RevokeRequest lists the revoked credentials, by reference.
type RevokeRequest struct {
	Namespace string
	Engine    string
	Revoked   map[string]time.Time
}

// Backend creates the credentials of a target system.
type Backend interface {
	Issue(ctx context.Context, req *IssueRequest) (*Credential, error)
	Renew(ctx context.Context, req *RenewRequest) (*Renewal, error)
	Revoke(ctx context.Context, req *RevokeRequest) error
}

// Invalid is an error of the request, not of the operator: a param out of
// the bounds of the role.
type Invalid struct{ Reason string }

func (e *Invalid) Error() string { return e.Reason }

// Handler serves a backend. Failures answer with a status only: the leader
// doesn't pass a body on, and it can't hold a credential by accident.
func Handler(b Backend) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST "+PathIssue, call(func(ctx context.Context, req *IssueRequest) (*Credential, error) {
		if req.LeaseID == "" {
			return nil, &Invalid{Reason: "a lease ID is required"}
		}
		return b.Issue(ctx, req)
	}))
	mux.Handle("POST "+PathRenew, call(func(ctx context.Context, req *RenewRequest) (*Renewal, error) {
		return b.Renew(ctx, req)
	}))
	mux.Handle("POST "+PathRevoke, call(func(ctx context.Context, req *RevokeRequest) (*struct{}, error) {
		return &struct{}{}, b.Revoke(ctx, req)
	}))
	mux.HandleFunc("POST "+PathHealth, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "{}")
	})
	return mux
}

// call decodes a request, runs fn and encodes its answer.
func call[Req, Resp any](fn func(context.Context, *Req) (*Resp, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req Req
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequest)).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		resp, err := fn(r.Context(), &req)
		var invalid *Invalid
		switch {
		case errors.As(err, &invalid):
			w.WriteHeader(http.StatusBadRequest)
			return
		case err != nil:
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		data, err := json.Marshal(resp)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	})
}

// Serve serves a backend on addr with the workload certificate of the task
// until ctx ends.
func Serve(ctx context.Context, addr string, certs *TLS, b Backend) error {
	config, err := certs.Config()
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: addr, Handler: Handler(b), TLSConfig: config, ReadHeaderTimeout: 10 * time.Second}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	if err := srv.ListenAndServeTLS("", ""); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving the operator: %w", err)
	}
	<-stopped
	return nil
}
