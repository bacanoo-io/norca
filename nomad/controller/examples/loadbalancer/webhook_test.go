// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/nomad/controller/webhook"
	"github.com/shoenig/test/must"
)

func review(t *testing.T, url, op, spec, oldSpec string) *webhook.Response {
	t.Helper()
	req := &webhook.Request{UID: "u1", Operation: op, Object: &webhook.Object{Spec: json.RawMessage(spec)}}
	if oldSpec != "" {
		req.OldObject = &webhook.Object{Spec: json.RawMessage(oldSpec)}
	}
	body, err := json.Marshal(&webhook.Review{APIVersion: webhook.APIVersion, Kind: webhook.Kind, Request: req})
	must.NoError(t, err)
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	must.NoError(t, err)
	defer resp.Body.Close()
	must.Eq(t, http.StatusOK, resp.StatusCode)
	var out webhook.Review
	must.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	must.Eq(t, "u1", out.Response.UID)
	return out.Response
}

func TestWebhook_LoadBalancer(t *testing.T) {
	srv := httptest.NewServer(newWebhookMux(webhookConfig{classes: []string{"nomad", "edge"}, defaultClass: "edge"}))
	defer srv.Close()

	// Defaulting fills in the class of the region as a patch of the spec.
	r := review(t, srv.URL+"/default", "create", `{"service":"web","port":80}`, "")
	must.True(t, r.Allowed)
	var patch []map[string]any
	must.NoError(t, json.Unmarshal(r.Patch, &patch))
	must.Eq(t, "replace", patch[0]["op"])
	must.Eq(t, "/spec", patch[0]["path"])
	must.Eq[any](t, "edge", patch[0]["value"].(map[string]any)["class"])

	r = review(t, srv.URL+"/default", "create", `{"service":"web","port":80,"class":"nomad"}`, "")
	must.True(t, r.Allowed)
	must.Len(t, 0, r.Patch)

	// Validation.
	must.True(t, review(t, srv.URL+"/validate", "create", `{"service":"web","port":80,"class":"edge"}`, "").Allowed)
	r = review(t, srv.URL+"/validate", "create", `{"service":"web","port":80,"class":"aws-nlb"}`, "")
	must.False(t, r.Allowed)
	must.StrContains(t, r.Message, `class "aws-nlb" is not available`)
	r = review(t, srv.URL+"/validate", "create", `{"service":"","port":80}`, "")
	must.False(t, r.Allowed)
	must.StrContains(t, r.Message, "service is required")
	r = review(t, srv.URL+"/validate", "update", `{"service":"web","port":80,"class":"edge"}`, `{"service":"web","port":80,"class":"nomad"}`)
	must.False(t, r.Allowed)
	must.StrContains(t, r.Message, "can't be changed")
	must.True(t, review(t, srv.URL+"/validate", "delete", `{}`, `{}`).Allowed)
}
