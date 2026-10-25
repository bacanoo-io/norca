// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"context"
	"testing"
	"time"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/admission"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/stream"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/nomad/structs/config"
	"github.com/shoenig/test/must"
)

// testVariablePolicy returns an admission policy for Variables writes under
// the path patterns.
func testVariablePolicy(name, action, expr string, paths ...string) *structs.AdmissionPolicy {
	p := testAdmissionPolicy(name, nil, action, expr)
	p.Match = &structs.AdmissionPolicyMatch{
		Resources:     []string{structs.AdmissionResourceVariable},
		VariablePaths: paths,
	}
	return p
}

func admissionApplyVariable(t *testing.T, s *Server, op structs.VarOp, path string, items map[string]string,
	modifyIndex uint64) (*structs.VariablesApplyResponse, error) {

	codec := rpcClient(t, s)
	req := &structs.VariablesApplyRequest{
		Op: op,
		Var: &structs.VariableDecrypted{
			VariableMetadata: structs.VariableMetadata{
				Namespace:   structs.DefaultNamespace,
				Path:        path,
				ModifyIndex: modifyIndex,
			},
			Items: items,
		},
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace},
	}
	var resp structs.VariablesApplyResponse
	err := msgpackrpc.CallWithCodec(codec, structs.VariablesApplyRPCMethod, req, &resp)
	return &resp, err
}

func TestVariablesEndpoint_Admission_Policies(t *testing.T) {
	ci.Parallel(t)

	s, cleanup := admissionServer(t)
	defer cleanup()

	broker, err := s.State().EventBroker()
	must.NoError(t, err)
	sub, err := broker.Subscribe(&stream.SubscribeRequest{
		Topics:    map[structs.Topic][]string{structs.TopicAdmissionPolicy: {"*"}},
		Namespace: "*",
	})
	must.NoError(t, err)
	defer sub.Unsubscribe()

	upsertAdmissionPolicies(t, s,
		// A job policy (no resources) never applies to Variables.
		testAdmissionPolicy("jobs-only", nil, "deny", `false`),
		testVariablePolicy("owner-required", "deny", `request.operation == 'delete' || has(variable.items.owner)`, "crd/*"),
		testVariablePolicy("owner-immutable", "deny",
			`oldVariable == null || variable.operation == 'delete' || oldVariable.items.owner == variable.items.owner`, "crd/*"),
		testVariablePolicy("description", "warn", `request.operation == 'delete' || has(variable.items.description)`, "crd/*"),
		testVariablePolicy("protected", "deny",
			`request.operation != 'delete' || oldVariable == null || !has(oldVariable.items.protected) || oldVariable.items.protected != 'true'`, "crd/*"),
	)

	// Paths that no Variables policy matches are not affected.
	_, err = admissionApplyVariable(t, s, structs.VarOpSet, "other/path", map[string]string{"a": "b"}, 0)
	must.NoError(t, err)

	// Deny: the policy message is returned and nothing is written.
	path := "crd/db-example-com/PostgresCluster/billing"
	_, err = admissionApplyVariable(t, s, structs.VarOpSet, path, map[string]string{"spec": "{}"}, 0)
	must.ErrorContains(t, err, structs.VariableAdmissionDeniedPrefix)
	must.ErrorContains(t, err, "owner-required violated")
	stored, err := s.State().GetVariable(nil, structs.DefaultNamespace, path)
	must.NoError(t, err)
	must.Nil(t, stored)

	// Warn: the write succeeds with a warning.
	resp, err := admissionApplyVariable(t, s, structs.VarOpCAS, path,
		map[string]string{"owner": "team-a", "protected": "true"}, 0)
	must.NoError(t, err)
	must.Eq(t, structs.VarOpResultOk, resp.Result)
	must.Len(t, 1, resp.Warnings)
	must.StrContains(t, resp.Warnings[0], "description violated")
	modifyIndex := resp.Output.ModifyIndex

	// oldVariable is the stored Variable on update.
	_, err = admissionApplyVariable(t, s, structs.VarOpSet, path,
		map[string]string{"owner": "team-b", "description": "d"}, 0)
	must.ErrorContains(t, err, "owner-immutable violated")
	resp, err = admissionApplyVariable(t, s, structs.VarOpCAS, path,
		map[string]string{"owner": "team-a", "description": "d", "protected": "true"}, modifyIndex)
	must.NoError(t, err)
	must.Len(t, 0, resp.Warnings)
	modifyIndex = resp.Output.ModifyIndex

	// Delete and delete-cas see the deleted Variable as oldVariable.
	_, err = admissionApplyVariable(t, s, structs.VarOpDelete, path, nil, 0)
	must.ErrorContains(t, err, "protected violated")
	_, err = admissionApplyVariable(t, s, structs.VarOpDeleteCAS, path, nil, modifyIndex)
	must.ErrorContains(t, err, "protected violated")
	resp, err = admissionApplyVariable(t, s, structs.VarOpSet, path,
		map[string]string{"owner": "team-a", "description": "d"}, 0)
	must.NoError(t, err)
	_, err = admissionApplyVariable(t, s, structs.VarOpDeleteCAS, path, nil, resp.Output.ModifyIndex)
	must.NoError(t, err)

	// The rejection is published without the items.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var denied *structs.VariableAdmissionDeniedEvent
	for denied == nil {
		events, err := sub.Next(ctx)
		must.NoError(t, err)
		for _, e := range events.Events {
			if e.Type == structs.TypeVariableAdmissionDenied {
				denied = e.Payload.(*structs.VariableAdmissionDeniedEvent)
				must.Eq(t, path, e.Key)
			}
		}
	}
	must.Eq(t, "owner-required", denied.Name)
	must.Eq(t, "policy", denied.Controller)
	must.Eq(t, structs.AdmissionOperationSet, denied.Operation)

	// Job policies are not affected by Variables policies, and Variables
	// policies do not apply to jobs: only jobs-only rejects the job.
	_, err = admissionRegisterJob(t, s, mock.Job(), "")
	must.ErrorContains(t, err, "jobs-only violated")
	must.StrNotContains(t, err.Error(), "owner-required")
}

func TestVariablesEndpoint_Admission_Schema(t *testing.T) {
	ci.Parallel(t)

	s, cleanup := admissionServer(t)
	defer cleanup()

	schemaPolicy := &structs.AdmissionPolicy{
		Name:       "crd-schema",
		SchemaFrom: structs.AdmissionSchemaFromCRDSchema,
		Match: &structs.AdmissionPolicyMatch{
			Resources:     []string{structs.AdmissionResourceVariable},
			VariablePaths: []string{"crd/*", "crd-schema/*"},
		},
	}
	upsertAdmissionPolicies(t, s, schemaPolicy)

	path := "crd/db-example-com/PostgresCluster/billing"

	// Without a schema, objects are not validated.
	_, err := admissionApplyVariable(t, s, structs.VarOpSet, path, map[string]string{"anything": "goes"}, 0)
	must.NoError(t, err)

	// An invalid schema cannot be stored.
	_, err = admissionApplyVariable(t, s, structs.VarOpSet, "crd-schema/db-example-com/PostgresCluster",
		map[string]string{"schema": `{"type": 12}`}, 0)
	must.ErrorContains(t, err, "invalid schema")

	_, err = admissionApplyVariable(t, s, structs.VarOpSet, "crd-schema/db-example-com/PostgresCluster",
		map[string]string{"schema": `{
			"type": "object",
			"required": ["spec"],
			"properties": {"spec": {"type": "object", "properties": {"replicas": {"type": "integer", "maximum": 5}}}}
		}`, "encoding": "json"}, 0)
	must.NoError(t, err)

	_, err = admissionApplyVariable(t, s, structs.VarOpSet, path, map[string]string{"spec": `{"replicas": 9}`}, 0)
	must.ErrorContains(t, err, structs.VariableAdmissionDeniedPrefix)
	must.ErrorContains(t, err, "does not match schema")
	must.ErrorContains(t, err, "/spec/replicas")

	_, err = admissionApplyVariable(t, s, structs.VarOpSet, path, map[string]string{"spec": `{"replicas": 3}`}, 0)
	must.NoError(t, err)

	// Objects of other kinds have no schema; deletes are not validated.
	_, err = admissionApplyVariable(t, s, structs.VarOpSet, "crd/db-example-com/Other/x", map[string]string{"a": "b"}, 0)
	must.NoError(t, err)
	_, err = admissionApplyVariable(t, s, structs.VarOpDelete, path, nil, 0)
	must.NoError(t, err)

	// With the warn action a mismatch is only a warning.
	schemaPolicy = schemaPolicy.Copy()
	schemaPolicy.Action = structs.AdmissionPolicyActionWarn
	upsertAdmissionPolicies(t, s, schemaPolicy)
	resp, err := admissionApplyVariable(t, s, structs.VarOpSet, path, map[string]string{"spec": `{"replicas": 9}`}, 0)
	must.NoError(t, err)
	must.Len(t, 1, resp.Warnings)
	must.StrContains(t, resp.Warnings[0], "does not match schema")
}

func TestVariablesEndpoint_Admission_Webhooks(t *testing.T) {
	ci.Parallel(t)

	withItems := newTestWebhook(t, func(req *admission.ReviewRequest) *admission.ReviewResponse {
		if req.Variable != nil && req.Variable.Items["forbidden"] != "" {
			return &admission.ReviewResponse{Allowed: false, Message: "forbidden item"}
		}
		return &admission.ReviewResponse{Allowed: true, Warnings: []string{"seen"}}
	})
	metadataOnly := newTestWebhook(t, func(req *admission.ReviewRequest) *admission.ReviewResponse {
		return &admission.ReviewResponse{Allowed: true}
	})
	jobsOnly := newTestWebhook(t, func(req *admission.ReviewRequest) *admission.ReviewResponse {
		return &admission.ReviewResponse{Allowed: false, Message: "jobs only"}
	})
	mutating := newTestWebhook(t, func(req *admission.ReviewRequest) *admission.ReviewResponse {
		return &admission.ReviewResponse{Allowed: true}
	})

	s, cleanup := admissionServer(t,
		&config.AdmissionWebhookConfig{Name: "with-items", URL: withItems.URL,
			Resources: []string{"variable"}, VariablePaths: []string{"crd/*"}, IncludeVariableItems: true},
		&config.AdmissionWebhookConfig{Name: "metadata-only", URL: metadataOnly.URL,
			Resources: []string{"variable"}},
		&config.AdmissionWebhookConfig{Name: "jobs-only", URL: jobsOnly.URL, Namespaces: []string{"none"}},
		&config.AdmissionWebhookConfig{Name: "mutating", URL: mutating.URL, Mode: config.AdmissionWebhookModeMutate,
			Namespaces: []string{"none"}},
	)
	defer cleanup()

	path := "crd/g/k/n"
	resp, err := admissionApplyVariable(t, s, structs.VarOpSet, path, map[string]string{"secret": "s3cr3t"}, 0)
	must.NoError(t, err)
	must.Eq(t, []string{`admission webhook "with-items": seen`}, resp.Warnings)

	review := <-withItems.reviews
	must.Eq(t, structs.AdmissionResourceVariable, review.Resource)
	must.Eq(t, structs.AdmissionOperationSet, review.Operation)
	must.Eq(t, path, review.Variable.Path)
	must.Eq(t, "s3cr3t", review.Variable.Items["secret"])
	must.Nil(t, review.OldVariable)
	must.Nil(t, review.Job)

	// Webhooks that did not opt in never receive the items.
	review = <-metadataOnly.reviews
	must.Eq(t, path, review.Variable.Path)
	must.MapEmpty(t, review.Variable.Items)

	// The previous Variable is sent on update and delete.
	_, err = admissionApplyVariable(t, s, structs.VarOpSet, path, map[string]string{"secret": "new"}, 0)
	must.NoError(t, err)
	review = <-withItems.reviews
	must.NotNil(t, review.OldVariable)
	must.Eq(t, "s3cr3t", review.OldVariable.Items["secret"])
	review = <-metadataOnly.reviews
	must.NotNil(t, review.OldVariable)
	must.Positive(t, review.OldVariable.ModifyIndex)
	must.MapEmpty(t, review.OldVariable.Items)

	_, err = admissionApplyVariable(t, s, structs.VarOpDelete, path, nil, 0)
	must.NoError(t, err)
	review = <-withItems.reviews
	must.Eq(t, structs.AdmissionOperationDelete, review.Operation)
	must.Eq(t, "new", review.OldVariable.Items["secret"])
	<-metadataOnly.reviews

	// A denying validating webhook rejects the write.
	_, err = admissionApplyVariable(t, s, structs.VarOpSet, path, map[string]string{"forbidden": "x"}, 0)
	must.ErrorContains(t, err, structs.VariableAdmissionDeniedPrefix)
	must.ErrorContains(t, err, "forbidden item")

	// Paths outside variable_paths are not sent to the webhook.
	before := withItems.calls.Load()
	_, err = admissionApplyVariable(t, s, structs.VarOpSet, "other", map[string]string{"forbidden": "x"}, 0)
	must.NoError(t, err)
	must.Eq(t, before, withItems.calls.Load())

	// Job webhooks and mutating webhooks never see Variables.
	must.Eq(t, 0, jobsOnly.calls.Load())
	must.Eq(t, 0, mutating.calls.Load())
}

func TestVariablesEndpoint_Admission_WebhookFailure(t *testing.T) {
	ci.Parallel(t)

	s, cleanup := admissionServer(t,
		&config.AdmissionWebhookConfig{Name: "down", URL: "http://127.0.0.1:1/validate",
			Resources: []string{"variable"}, VariablePaths: []string{"strict/*"}},
		&config.AdmissionWebhookConfig{Name: "down-ignored", URL: "http://127.0.0.1:1/validate",
			Resources: []string{"variable"}, VariablePaths: []string{"lenient/*"},
			FailurePolicy: config.AdmissionFailurePolicyIgnore},
	)
	defer cleanup()

	_, err := admissionApplyVariable(t, s, structs.VarOpSet, "strict/a", map[string]string{"a": "b"}, 0)
	must.ErrorContains(t, err, `admission webhook "down" failed`)

	resp, err := admissionApplyVariable(t, s, structs.VarOpSet, "lenient/a", map[string]string{"a": "b"}, 0)
	must.NoError(t, err)
	must.Len(t, 1, resp.Warnings)
	must.StrContains(t, resp.Warnings[0], "failed and was ignored")
}
