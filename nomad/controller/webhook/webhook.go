// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package webhook serves the admission webhooks an operator binds to its
// object kind ("webhook" blocks of the kind): the servers POST an admission
// review of every write of an object of the kind, and the webhook accepts
// or rejects it ("validate" mode), or also fills in or changes the spec
// ("default" mode).
//
// A validating webhook:
//
//	mux.Handle("/validate", webhook.Validating(func(ctx context.Context, req *webhook.Request) ([]string, error) {
//		var spec Spec
//		if err := req.Object.DecodeSpec(&spec); err != nil {
//			return nil, err
//		}
//		return nil, spec.Validate() // a non-nil error denies the write
//	}))
//
// A defaulting webhook changes the spec in place; the handler sends the
// change back as a JSON patch:
//
//	mux.Handle("/default", webhook.Defaulting(func(ctx context.Context, req *webhook.Request, spec map[string]any) ([]string, error) {
//		if _, ok := spec["class"]; !ok {
//			spec["class"] = "nomad"
//		}
//		return nil, nil
//	}))
//
// The servers call webhooks over https only; serve the mux with the
// certificate whose CA the kind's ca_cert holds (see ListenAndServeTLS).
// Webhooks are called once an administrator approved them; kinds installed
// with a management token approve their webhooks implicitly.
package webhook

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"time"
)

const (
	// APIVersion and Kind identify the admission review documents.
	APIVersion = "admission.norca.io/v1"
	Kind       = "AdmissionReview"

	// ResourceObject is the resource of reviews of custom objects.
	ResourceObject = "object"

	// Operations of object reviews.
	OperationCreate = "create"
	OperationUpdate = "update"
	OperationDelete = "delete"

	// maxRequestSize bounds the body of a review request.
	maxRequestSize = 4 << 20
)

// Review is the document exchanged with the servers.
type Review struct {
	APIVersion string    `json:"api_version"`
	Kind       string    `json:"kind"`
	Request    *Request  `json:"request,omitempty"`
	Response   *Response `json:"response,omitempty"`
}

// Request is the write under review.
type Request struct {
	UID       string `json:"uid"`
	Resource  string `json:"resource,omitempty"`
	Operation string `json:"operation"`
	DryRun    bool   `json:"dry_run"`
	Namespace string `json:"namespace"`
	User      *User  `json:"user"`

	// ObjectKind is the "group/Kind" reference of the object.
	ObjectKind string `json:"object_kind,omitempty"`

	// Object is the object as it would be stored; for deletions the
	// stored object. OldObject is the stored object on update and delete.
	Object    *Object `json:"object,omitempty"`
	OldObject *Object `json:"old_object,omitempty"`
}

// User is the identity that writes the object.
type User struct {
	AccessorID string   `json:"accessor_id,omitempty"`
	Name       string   `json:"name,omitempty"`
	Type       string   `json:"type,omitempty"`
	Roles      []string `json:"roles,omitempty"`
	Policies   []string `json:"policies,omitempty"`
}

// Object is an object in a review, in the form CEL rules see it.
type Object struct {
	Kind     string          `json:"kind"`
	Metadata Metadata        `json:"metadata"`
	Spec     json.RawMessage `json:"spec,omitempty"`
	Status   json.RawMessage `json:"status,omitempty"`
}

// Metadata of an object in a review.
type Metadata struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace"`
	Labels      map[string]string `json:"labels,omitempty"`
	ManagedBy   string            `json:"managedBy,omitempty"`
	Generation  uint64            `json:"generation"`
	CreateIndex uint64            `json:"createIndex"`
	ModifyIndex uint64            `json:"modifyIndex"`
	Finalizers  []string          `json:"finalizers,omitempty"`
	Deleting    bool              `json:"deleting"`
	KindVersion uint64            `json:"kindVersion"`
}

// DecodeSpec decodes the spec of the object into v.
func (o *Object) DecodeSpec(v any) error {
	if o == nil || len(o.Spec) == 0 {
		return json.Unmarshal([]byte("{}"), v)
	}
	return json.Unmarshal(o.Spec, v)
}

// DecodeStatus decodes the status of the object into v.
func (o *Object) DecodeStatus(v any) error {
	if o == nil || len(o.Status) == 0 {
		return json.Unmarshal([]byte("{}"), v)
	}
	return json.Unmarshal(o.Status, v)
}

// Response is the decision of the webhook.
type Response struct {
	UID      string          `json:"uid"`
	Allowed  bool            `json:"allowed"`
	Message  string          `json:"message,omitempty"`
	Warnings []string        `json:"warnings,omitempty"`
	Patch    json.RawMessage `json:"patch,omitempty"`
}

// ValidateFunc reviews a write. A non-nil error denies it with the error's
// message; the warnings are returned to the writer.
type ValidateFunc func(ctx context.Context, req *Request) (warnings []string, err error)

// DefaultFunc changes the spec of a create or update in place. A non-nil
// error denies the write.
type DefaultFunc func(ctx context.Context, req *Request, spec map[string]any) (warnings []string, err error)

// Validating returns the handler of a validating webhook.
func Validating(fn ValidateFunc) http.Handler {
	return handler(func(ctx context.Context, req *Request) (*Response, error) {
		warnings, err := fn(ctx, req)
		if err != nil {
			return &Response{Allowed: false, Message: err.Error(), Warnings: warnings}, nil
		}
		return &Response{Allowed: true, Warnings: warnings}, nil
	})
}

// Defaulting returns the handler of a defaulting webhook. The change of the
// spec is sent back as a JSON patch replacing /spec.
func Defaulting(fn DefaultFunc) http.Handler {
	return handler(func(ctx context.Context, req *Request) (*Response, error) {
		if req.Object == nil {
			return nil, errors.New("review without object")
		}
		var spec map[string]any
		if len(req.Object.Spec) > 0 {
			if err := json.Unmarshal(req.Object.Spec, &spec); err != nil {
				return nil, fmt.Errorf("invalid spec: %w", err)
			}
		}
		if spec == nil {
			spec = map[string]any{}
		}
		var before map[string]any
		_ = json.Unmarshal(mustJSON(spec), &before)

		warnings, err := fn(ctx, req, spec)
		if err != nil {
			return &Response{Allowed: false, Message: err.Error(), Warnings: warnings}, nil
		}
		resp := &Response{Allowed: true, Warnings: warnings}
		var after map[string]any
		if err := json.Unmarshal(mustJSON(spec), &after); err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(before, after) {
			patch, err := json.Marshal([]map[string]any{{"op": "replace", "path": "/spec", "value": after}})
			if err != nil {
				return nil, err
			}
			resp.Patch = patch
		}
		return resp, nil
	})
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("null")
	}
	return b
}

// handler decodes a review, runs fn and encodes its response. An error of
// fn is a failure of the webhook (500), subject to its failure policy.
func handler(fn func(ctx context.Context, req *Request) (*Response, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestSize+1))
		if err != nil || len(body) > maxRequestSize {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		var review Review
		if err := json.Unmarshal(body, &review); err != nil || review.Request == nil {
			http.Error(w, "invalid admission review", http.StatusBadRequest)
			return
		}
		resp, err := fn(r.Context(), review.Request)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		resp.UID = review.Request.UID
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&Review{APIVersion: APIVersion, Kind: Kind, Response: resp})
	})
}

// ListenAndServeTLS serves the handler on addr with the certificate and key
// files until ctx is done.
func ListenAndServeTLS(ctx context.Context, addr, certFile, keyFile string, h http.Handler) error {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return fmt.Errorf("failed to load the webhook certificate: %w", err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return Serve(ctx, tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}), h)
}

// Serve serves the handler on the listener until ctx is done.
func Serve(ctx context.Context, ln net.Listener, h http.Handler) error {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
