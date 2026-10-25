// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package admission

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/nomad/structs/config"
	"github.com/shoenig/test/must"
)

func TestApplyJSONPatch(t *testing.T) {
	ci.Parallel(t)

	doc := `{"a":{"b":1,"c":[1,2,3]},"d":"x","big":12345678901234567}`
	cases := []struct {
		name  string
		patch string
		want  string
		err   bool
	}{
		{"add member", `[{"op":"add","path":"/a/e","value":{"x":true}}]`,
			`{"a":{"b":1,"c":[1,2,3],"e":{"x":true}},"big":12345678901234567,"d":"x"}`, false},
		{"add array end", `[{"op":"add","path":"/a/c/-","value":4}]`,
			`{"a":{"b":1,"c":[1,2,3,4]},"big":12345678901234567,"d":"x"}`, false},
		{"insert array", `[{"op":"add","path":"/a/c/0","value":0}]`,
			`{"a":{"b":1,"c":[0,1,2,3]},"big":12345678901234567,"d":"x"}`, false},
		{"remove", `[{"op":"remove","path":"/a/c/1"},{"op":"remove","path":"/d"}]`,
			`{"a":{"b":1,"c":[1,3]},"big":12345678901234567}`, false},
		{"replace", `[{"op":"replace","path":"/d","value":"y"}]`,
			`{"a":{"b":1,"c":[1,2,3]},"big":12345678901234567,"d":"y"}`, false},
		{"move", `[{"op":"move","from":"/d","path":"/a/d"}]`,
			`{"a":{"b":1,"c":[1,2,3],"d":"x"},"big":12345678901234567}`, false},
		{"copy", `[{"op":"copy","from":"/a/c","path":"/e"},{"op":"add","path":"/e/-","value":9}]`,
			`{"a":{"b":1,"c":[1,2,3]},"big":12345678901234567,"d":"x","e":[1,2,3,9]}`, false},
		{"test ok", `[{"op":"test","path":"/a/b","value":1.0}]`,
			`{"a":{"b":1,"c":[1,2,3]},"big":12345678901234567,"d":"x"}`, false},
		{"escaped pointer", `[{"op":"add","path":"/a~1b~0c","value":1}]`,
			`{"a":{"b":1,"c":[1,2,3]},"a/b~c":1,"big":12345678901234567,"d":"x"}`, false},
		{"test fails", `[{"op":"test","path":"/d","value":"z"}]`, "", true},
		{"replace missing", `[{"op":"replace","path":"/nope","value":1}]`, "", true},
		{"remove missing", `[{"op":"remove","path":"/a/c/7"}]`, "", true},
		{"bad op", `[{"op":"merge","path":"/d","value":1}]`, "", true},
		{"move into child", `[{"op":"move","from":"/a","path":"/a/x"}]`, "", true},
		{"not array", `{"op":"add"}`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := ApplyJSONPatch([]byte(doc), []byte(tc.patch))
			if tc.err {
				must.Error(t, err)
				return
			}
			must.NoError(t, err)
			must.EqJSON(t, tc.want, string(out))
		})
	}
}

func TestEncodeDecodeJob(t *testing.T) {
	ci.Parallel(t)

	job := mock.Job()
	job.VaultToken = "secret-vault"
	job.ConsulToken = "secret-consul"

	raw, err := EncodeJob(job)
	must.NoError(t, err)
	must.StrNotContains(t, string(raw), "secret-vault")
	must.StrNotContains(t, string(raw), "secret-consul")
	must.Eq(t, "secret-vault", job.VaultToken, must.Sprint("original job must not be modified"))

	out, err := DecodeJob(raw)
	must.NoError(t, err)
	must.Eq(t, job.ID, out.ID)
	must.Eq(t, job.TaskGroups[0].Tasks[0].Resources.CPU, out.TaskGroups[0].Tasks[0].Resources.CPU)
	must.Eq(t, job.Meta, out.Meta)

	v, err := JobValue(raw)
	must.NoError(t, err)
	m := v.(map[string]any)
	must.Eq(t, any(int64(job.Priority)), m["Priority"])
}

func testPolicy(name, action string, exprs ...string) *structs.AdmissionPolicy {
	p := &structs.AdmissionPolicy{Name: name, Action: action, ModifyIndex: 10}
	for _, e := range exprs {
		p.Rules = append(p.Rules, &structs.AdmissionPolicyRule{Expression: e, Message: "violated: " + e})
	}
	return p
}

func TestPolicyEngine_Compile(t *testing.T) {
	ci.Parallel(t)

	e, err := NewPolicyEngine()
	must.NoError(t, err)

	must.NoError(t, e.Compile(testPolicy("ok", "deny",
		`job.TaskGroups.all(g, g.Count <= 10)`,
		`request.operation in ['register', 'plan']`,
		`!job.TaskGroups.exists(g, g.Tasks.exists(t, t.Driver == 'docker' && has(t.Config.privileged) && t.Config.privileged))`,
	)))

	// syntax error
	must.ErrorContains(t, e.Compile(testPolicy("bad", "deny", `job.ID ==`)), "rule 0")
	// undeclared variable
	must.ErrorContains(t, e.Compile(testPolicy("bad", "deny", `true`, `jobs.ID == 'x'`)), "rule 1")
	// non-bool result
	must.ErrorContains(t, e.Compile(testPolicy("bad", "deny", `'a string'`)), "bool")
}

func TestPolicyEngine_Evaluate(t *testing.T) {
	ci.Parallel(t)

	e, err := NewPolicyEngine()
	must.NoError(t, err)

	job := mock.Job()
	job.TaskGroups[0].Tasks[0].Driver = "docker"
	job.TaskGroups[0].Tasks[0].Config = map[string]any{"image": "redis", "privileged": true}
	raw, err := EncodeJob(job)
	must.NoError(t, err)
	jobVal, err := JobValue(raw)
	must.NoError(t, err)
	req := map[string]any{"operation": "register", "namespace": "default"}

	privileged := `!job.TaskGroups.exists(g, g.Tasks.exists(t, t.Driver == 'docker' && has(t.Config.privileged) && t.Config.privileged))`

	// violated deny rule
	v := e.Evaluate(context.Background(), []*structs.AdmissionPolicy{testPolicy("p1", "deny", privileged)}, jobVal, req)
	must.Len(t, 1, v)
	must.Eq(t, "p1", v[0].Policy)
	must.Eq(t, "deny", v[0].Action)
	must.StrContains(t, v[0].Message, "violated")

	// satisfied rules, integer comparison and request variable
	v = e.Evaluate(context.Background(), []*structs.AdmissionPolicy{
		testPolicy("p2", "deny", `job.TaskGroups[0].Count == 10`, `job.Priority > 10.5`, `request.operation == 'register'`),
	}, jobVal, req)
	must.Len(t, 0, v)

	// runtime error fails closed
	v = e.Evaluate(context.Background(), []*structs.AdmissionPolicy{testPolicy("p3", "warn", `job.NoSuchField == 1`)}, jobVal, req)
	must.Len(t, 1, v)
	must.StrContains(t, v[0].Message, "failed to evaluate")
	must.Eq(t, "warn", v[0].Action)

	// the compiled program is cached per modify index
	p := testPolicy("p4", "deny", `true`)
	must.Len(t, 0, e.Evaluate(context.Background(), []*structs.AdmissionPolicy{p}, jobVal, req))
	p2 := testPolicy("p4", "deny", `false`)
	p2.ModifyIndex = 11
	must.Len(t, 1, e.Evaluate(context.Background(), []*structs.AdmissionPolicy{p2}, jobVal, req))

	e.Prune(map[string]struct{}{"p4": {}})
	e.lock.Lock()
	must.MapLen(t, 1, e.cache)
	e.lock.Unlock()
}

func webhookConfig(name, url string) *config.AdmissionWebhookConfig {
	cfg := &config.AdmissionConfig{Webhooks: []*config.AdmissionWebhookConfig{{Name: name, URL: url}}}
	cfg.Canonicalize()
	return cfg.Webhooks[0]
}

func TestWebhook_Review(t *testing.T) {
	ci.Parallel(t)

	var got Review
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		must.Eq(t, http.MethodPost, r.Method)
		must.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		_ = json.NewEncoder(w).Encode(Review{
			APIVersion: ReviewAPIVersion,
			Kind:       ReviewKind,
			Response: &ReviewResponse{
				UID:      got.Request.UID,
				Allowed:  true,
				Warnings: []string{"careful"},
				Patch:    json.RawMessage(`[{"op":"add","path":"/Meta/team","value":"a"}]`),
			},
		})
	}))
	defer srv.Close()

	w, err := NewWebhook(webhookConfig("hook", srv.URL))
	must.NoError(t, err)
	defer w.Close()

	job := mock.Job()
	raw, err := EncodeJob(job)
	must.NoError(t, err)

	resp, err := w.Review(context.Background(), &ReviewRequest{
		UID: "123", Operation: structs.AdmissionOperationPlan, DryRun: true,
		Namespace: job.Namespace, User: &ReviewUser{Name: "alice"}, Job: raw,
	})
	must.NoError(t, err)
	must.True(t, resp.Allowed)
	must.Eq(t, []string{"careful"}, resp.Warnings)
	patch, err := resp.PatchOperations()
	must.NoError(t, err)
	must.NotNil(t, patch)

	must.Eq(t, ReviewKind, got.Kind)
	must.Eq(t, "123", got.Request.UID)
	must.Eq(t, "plan", got.Request.Operation)
	must.True(t, got.Request.DryRun)
	must.Eq(t, "alice", got.Request.User.Name)
	must.StrContains(t, string(got.Request.Job), job.ID)
}

func TestWebhook_ReviewErrors(t *testing.T) {
	ci.Parallel(t)

	handler := func(fn func(w http.ResponseWriter, req *Review)) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var review Review
			_ = json.NewDecoder(r.Body).Decode(&review)
			fn(w, &review)
		}))
	}

	cases := map[string]func(w http.ResponseWriter, req *Review){
		"status": func(w http.ResponseWriter, _ *Review) {
			w.WriteHeader(http.StatusInternalServerError)
		},
		"bad json": func(w http.ResponseWriter, _ *Review) {
			_, _ = w.Write([]byte("nope"))
		},
		"missing response": func(w http.ResponseWriter, _ *Review) {
			_, _ = w.Write([]byte(`{"kind":"AdmissionReview"}`))
		},
		"uid mismatch": func(w http.ResponseWriter, _ *Review) {
			_ = json.NewEncoder(w).Encode(Review{Response: &ReviewResponse{UID: "other", Allowed: true}})
		},
		"redirect": func(w http.ResponseWriter, _ *Review) {
			w.Header().Set("Location", "http://example.invalid/")
			w.WriteHeader(http.StatusTemporaryRedirect)
		},
		"timeout": func(w http.ResponseWriter, req *Review) {
			time.Sleep(500 * time.Millisecond)
			_ = json.NewEncoder(w).Encode(Review{Response: &ReviewResponse{UID: req.Request.UID, Allowed: true}})
		},
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			srv := handler(fn)
			defer srv.Close()

			cfg := webhookConfig("hook", srv.URL)
			cfg.Timeout = 100 * time.Millisecond
			w, err := NewWebhook(cfg)
			must.NoError(t, err)

			_, err = w.Review(context.Background(), &ReviewRequest{UID: "1", Job: json.RawMessage(`{}`)})
			must.Error(t, err)
		})
	}
}

func TestWebhook_TLS(t *testing.T) {
	ci.Parallel(t)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var review Review
		_ = json.NewDecoder(r.Body).Decode(&review)
		_ = json.NewEncoder(w).Encode(Review{Response: &ReviewResponse{UID: review.Request.UID, Allowed: true}})
	}))
	defer srv.Close()

	req := &ReviewRequest{UID: "1", Job: json.RawMessage(`{}`)}

	// Without the CA the server certificate is not trusted.
	w, err := NewWebhook(webhookConfig("hook", srv.URL))
	must.NoError(t, err)
	_, err = w.Review(context.Background(), req)
	must.Error(t, err)

	// With the CA file it is.
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	must.NoError(t, os.WriteFile(caFile, certPEM, 0o600))

	cfg := webhookConfig("hook", srv.URL)
	cfg.CAFile = caFile
	w, err = NewWebhook(cfg)
	must.NoError(t, err)
	resp, err := w.Review(context.Background(), req)
	must.NoError(t, err)
	must.True(t, resp.Allowed)

	// An invalid CA file is a configuration error.
	cfg.CAFile = filepath.Join(t.TempDir(), "missing.pem")
	_, err = NewWebhook(cfg)
	must.Error(t, err)
}

func TestWebhook_AppliesTo(t *testing.T) {
	ci.Parallel(t)

	cfg := webhookConfig("hook", "http://127.0.0.1:1")
	w, err := NewWebhook(cfg)
	must.NoError(t, err)
	must.True(t, w.AppliesTo("default"))

	cfg.Namespaces = []string{"prod", "team-*"}
	cfg.ExcludeNamespaces = []string{"team-admission"}
	must.True(t, w.AppliesTo("prod"))
	must.True(t, w.AppliesTo("team-a"))
	must.False(t, w.AppliesTo("team-admission"))
	must.False(t, w.AppliesTo("default"))
}

func TestReviewResponse_PatchOperations(t *testing.T) {
	ci.Parallel(t)

	p, err := (&ReviewResponse{}).PatchOperations()
	must.NoError(t, err)
	must.Nil(t, p)

	p, err = (&ReviewResponse{Patch: json.RawMessage(`[]`)}).PatchOperations()
	must.NoError(t, err)
	must.Nil(t, p)

	// base64 encoded, Kubernetes style
	encoded := base64.StdEncoding.EncodeToString([]byte(`[{"op":"remove","path":"/a"}]`))
	p, err = (&ReviewResponse{Patch: json.RawMessage(`"` + encoded + `"`)}).PatchOperations()
	must.NoError(t, err)
	must.Eq(t, `[{"op":"remove","path":"/a"}]`, string(p))

	_, err = (&ReviewResponse{Patch: json.RawMessage(`{"op":"add"}`)}).PatchOperations()
	must.Error(t, err)
}
