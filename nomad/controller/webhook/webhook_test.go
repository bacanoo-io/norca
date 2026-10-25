// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package webhook_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/controller/webhook"
	"github.com/shoenig/test/must"
)

func post(t *testing.T, url string, body string) (*http.Response, *webhook.Review) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	must.NoError(t, err)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp, nil
	}
	var out webhook.Review
	must.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return resp, &out
}

func reviewBody(t *testing.T, req *webhook.Request) string {
	b, err := json.Marshal(&webhook.Review{APIVersion: webhook.APIVersion, Kind: webhook.Kind, Request: req})
	must.NoError(t, err)
	return string(b)
}

func TestValidating(t *testing.T) {
	type spec struct {
		Size int `json:"size"`
	}
	h := webhook.Validating(func(_ context.Context, req *webhook.Request) ([]string, error) {
		var s spec
		if err := req.Object.DecodeSpec(&s); err != nil {
			return nil, err
		}
		if s.Size > 3 {
			return []string{"big"}, errors.New("size must be at most 3")
		}
		return []string{"ok " + req.Operation}, nil
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	_, out := post(t, srv.URL, reviewBody(t, &webhook.Request{UID: "a", Operation: "create",
		Object: &webhook.Object{Spec: json.RawMessage(`{"size": 2}`)}}))
	must.Eq(t, webhook.APIVersion, out.APIVersion)
	must.Eq(t, "a", out.Response.UID)
	must.True(t, out.Response.Allowed)
	must.Eq(t, []string{"ok create"}, out.Response.Warnings)

	_, out = post(t, srv.URL, reviewBody(t, &webhook.Request{UID: "b", Operation: "update",
		Object: &webhook.Object{Spec: json.RawMessage(`{"size": 5}`)}}))
	must.False(t, out.Response.Allowed)
	must.Eq(t, "size must be at most 3", out.Response.Message)

	// Bad requests.
	resp, _ := post(t, srv.URL, `{`)
	must.Eq(t, http.StatusBadRequest, resp.StatusCode)
	resp, _ = post(t, srv.URL, `{"api_version": "x"}`)
	must.Eq(t, http.StatusBadRequest, resp.StatusCode)
	getResp, err := http.Get(srv.URL)
	must.NoError(t, err)
	getResp.Body.Close()
	must.Eq(t, http.StatusMethodNotAllowed, getResp.StatusCode)
}

func TestDefaulting(t *testing.T) {
	h := webhook.Defaulting(func(_ context.Context, req *webhook.Request, spec map[string]any) ([]string, error) {
		if spec["deny"] == true {
			return nil, errors.New("denied")
		}
		if _, ok := spec["size"]; !ok {
			spec["size"] = 1
		}
		return nil, nil
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	_, out := post(t, srv.URL, reviewBody(t, &webhook.Request{UID: "a", Operation: "create",
		Object: &webhook.Object{Spec: json.RawMessage(`{"name": "x"}`)}}))
	must.True(t, out.Response.Allowed)
	var patch []map[string]any
	must.NoError(t, json.Unmarshal(out.Response.Patch, &patch))
	must.Eq(t, []map[string]any{{"op": "replace", "path": "/spec", "value": map[string]any{"name": "x", "size": float64(1)}}}, patch)

	// No change, no patch.
	_, out = post(t, srv.URL, reviewBody(t, &webhook.Request{UID: "b", Operation: "update",
		Object: &webhook.Object{Spec: json.RawMessage(`{"size": 2}`)}}))
	must.True(t, out.Response.Allowed)
	must.Len(t, 0, out.Response.Patch)

	_, out = post(t, srv.URL, reviewBody(t, &webhook.Request{UID: "c", Operation: "update",
		Object: &webhook.Object{Spec: json.RawMessage(`{"deny": true}`)}}))
	must.False(t, out.Response.Allowed)

	// A review without an object is a failure of the webhook.
	resp, _ := post(t, srv.URL, reviewBody(t, &webhook.Request{UID: "d", Operation: "create"}))
	must.Eq(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestServe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- webhook.Serve(ctx, ln, webhook.Validating(func(context.Context, *webhook.Request) ([]string, error) { return nil, nil }))
	}()
	resp, err := http.Post("http://"+ln.Addr().String(), "application/json",
		bytes.NewReader([]byte(reviewBody(t, &webhook.Request{UID: "x", Object: &webhook.Object{}}))))
	must.NoError(t, err)
	resp.Body.Close()
	must.Eq(t, http.StatusOK, resp.StatusCode)
	cancel()
	select {
	case err := <-done:
		must.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not stop")
	}
	must.Error(t, webhook.ListenAndServeTLS(context.Background(), "127.0.0.1:0", "missing.pem", "missing-key.pem", nil))
}
