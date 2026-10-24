// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/admission"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/stream"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/nomad/structs/config"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
)

// testWebhook is an httptest admission webhook that records the reviews it
// receives and answers with respond.
type testWebhook struct {
	*httptest.Server
	calls   atomic.Int32
	reviews chan *admission.ReviewRequest
}

func newTestWebhook(t *testing.T, respond func(req *admission.ReviewRequest) *admission.ReviewResponse) *testWebhook {
	w := &testWebhook{reviews: make(chan *admission.ReviewRequest, 16)}
	w.Server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.calls.Add(1)
		var review admission.Review
		if err := json.NewDecoder(r.Body).Decode(&review); err != nil {
			rw.WriteHeader(http.StatusBadRequest)
			return
		}
		select {
		case w.reviews <- review.Request:
		default:
		}
		resp := respond(review.Request)
		resp.UID = review.Request.UID
		_ = json.NewEncoder(rw).Encode(&admission.Review{
			APIVersion: admission.ReviewAPIVersion,
			Kind:       admission.ReviewKind,
			Response:   resp,
		})
	}))
	t.Cleanup(w.Server.Close)
	return w
}

func admissionServer(t *testing.T, webhooks ...*config.AdmissionWebhookConfig) (*Server, func()) {
	s, cleanup := TestServer(t, func(c *Config) {
		c.NumSchedulers = 0
		c.AdmissionConfig = &config.AdmissionConfig{Webhooks: webhooks}
	})
	testutil.WaitForLeader(t, s.RPC)
	return s, cleanup
}

func admissionRegisterJob(t *testing.T, s *Server, job *structs.Job, token string) (*structs.JobRegisterResponse, error) {
	codec := rpcClient(t, s)
	req := &structs.JobRegisterRequest{
		Job:          job,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace, AuthToken: token},
	}
	var resp structs.JobRegisterResponse
	err := msgpackrpc.CallWithCodec(codec, "Job.Register", req, &resp)
	return &resp, err
}

func admissionPlanJob(t *testing.T, s *Server, job *structs.Job) (*structs.JobPlanResponse, error) {
	codec := rpcClient(t, s)
	req := &structs.JobPlanRequest{
		Job:          job,
		Diff:         true,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace},
	}
	var resp structs.JobPlanResponse
	err := msgpackrpc.CallWithCodec(codec, "Job.Plan", req, &resp)
	return &resp, err
}

func upsertAdmissionPolicies(t *testing.T, s *Server, policies ...*structs.AdmissionPolicy) {
	codec := rpcClient(t, s)
	req := &structs.AdmissionPolicyUpsertRequest{
		AdmissionPolicies: policies,
		WriteRequest:      structs.WriteRequest{Region: "global"},
	}
	var resp structs.GenericResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "AdmissionPolicy.UpsertAdmissionPolicies", req, &resp))
}

func TestJobEndpoint_Admission_Policies(t *testing.T) {
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

	// Without policies the job is admitted.
	job := mock.Job()
	_, err = admissionRegisterJob(t, s, job, "")
	must.NoError(t, err)

	upsertAdmissionPolicies(t, s,
		testAdmissionPolicy("owner", nil, "deny", `job.Meta.owner != 'mallory'`),
		testAdmissionPolicy("team-meta", nil, "warn", `has(job.Meta.team)`),
		testAdmissionPolicy("prod-only", []string{"prod"}, "deny", `false`),
	)

	// A warn policy only adds a warning; the prod policy doesn't match.
	job = mock.Job()
	resp, err := admissionRegisterJob(t, s, job, "")
	must.NoError(t, err)
	must.StrContains(t, resp.Warnings, "team-meta violated")

	// A deny policy rejects register and plan.
	job = mock.Job()
	job.Meta["owner"] = "mallory"
	_, err = admissionRegisterJob(t, s, job, "")
	must.ErrorContains(t, err, "owner violated")
	_, err = admissionPlanJob(t, s, job)
	must.ErrorContains(t, err, "owner violated")

	stored, err := s.State().JobByID(nil, job.Namespace, job.ID)
	must.NoError(t, err)
	must.Nil(t, stored)

	// The rejection is published on the event stream.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var denied *structs.JobAdmissionDeniedEvent
	for denied == nil {
		events, err := sub.Next(ctx)
		must.NoError(t, err)
		for _, e := range events.Events {
			if e.Type == structs.TypeJobAdmissionDenied {
				denied = e.Payload.(*structs.JobAdmissionDeniedEvent)
				must.Eq(t, job.ID, e.Key)
			}
		}
	}
	must.Eq(t, "owner", denied.Name)
	must.Eq(t, "policy", denied.Controller)
	must.Eq(t, structs.AdmissionOperationRegister, denied.Operation)
}

func TestJobEndpoint_Admission_MutatingWebhook(t *testing.T) {
	ci.Parallel(t)

	hook := newTestWebhook(t, func(req *admission.ReviewRequest) *admission.ReviewResponse {
		return &admission.ReviewResponse{
			Allowed:  true,
			Warnings: []string{"meta added"},
			Patch:    json.RawMessage(`[{"op":"add","path":"/Meta/team","value":"platform"}]`),
		}
	})

	s, cleanup := admissionServer(t, &config.AdmissionWebhookConfig{
		Name: "add-team", URL: hook.URL, Mode: config.AdmissionWebhookModeMutate,
	})
	defer cleanup()

	// The patch is visible in the plan diff.
	job := mock.Job()
	job.VaultToken = "secret"
	planResp, err := admissionPlanJob(t, s, job)
	must.NoError(t, err)
	must.StrContains(t, planResp.Warnings, "meta added")
	must.NotNil(t, planResp.Diff)
	found := false
	for _, f := range planResp.Diff.Fields {
		if f.Name == "Meta[team]" && f.New == "platform" {
			found = true
		}
	}
	must.True(t, found, must.Sprintf("diff fields: %#v", planResp.Diff.Fields))

	review := <-hook.reviews
	must.Eq(t, structs.AdmissionOperationPlan, review.Operation)
	must.True(t, review.DryRun)
	must.StrNotContains(t, string(review.Job), "secret")

	// And on register the stored job has it.
	job = mock.Job()
	_, err = admissionRegisterJob(t, s, job, "")
	must.NoError(t, err)
	stored, err := s.State().JobByID(nil, job.Namespace, job.ID)
	must.NoError(t, err)
	must.Eq(t, "platform", stored.Meta["team"])
	must.Eq(t, "armon", stored.Meta["owner"])
}

func TestJobEndpoint_Admission_MutatingWebhook_InvalidPatch(t *testing.T) {
	ci.Parallel(t)

	hook := newTestWebhook(t, func(req *admission.ReviewRequest) *admission.ReviewResponse {
		// Moving the job to another namespace is not allowed.
		return &admission.ReviewResponse{
			Allowed: true,
			Patch:   json.RawMessage(`[{"op":"replace","path":"/Namespace","value":"other"}]`),
		}
	})

	s, cleanup := admissionServer(t, &config.AdmissionWebhookConfig{
		Name: "mover", URL: hook.URL, Mode: config.AdmissionWebhookModeMutate,
	})
	defer cleanup()

	_, err := admissionRegisterJob(t, s, mock.Job(), "")
	must.ErrorContains(t, err, "must not change the job ID, namespace or region")
}

func TestJobEndpoint_Admission_ValidatingWebhook(t *testing.T) {
	ci.Parallel(t)

	hook := newTestWebhook(t, func(req *admission.ReviewRequest) *admission.ReviewResponse {
		var job structs.Job
		_ = json.Unmarshal(req.Job, &job)
		if job.Meta["owner"] == "mallory" {
			return &admission.ReviewResponse{Allowed: false, Message: "mallory may not deploy"}
		}
		return &admission.ReviewResponse{Allowed: true, Warnings: []string{"looks fine"}}
	})
	excluded := newTestWebhook(t, func(req *admission.ReviewRequest) *admission.ReviewResponse {
		return &admission.ReviewResponse{Allowed: false, Message: "should never be called"}
	})

	s, cleanup := admissionServer(t,
		&config.AdmissionWebhookConfig{Name: "owner-check", URL: hook.URL},
		&config.AdmissionWebhookConfig{Name: "excluded", URL: excluded.URL, ExcludeNamespaces: []string{"default"}},
	)
	defer cleanup()

	job := mock.Job()
	resp, err := admissionRegisterJob(t, s, job, "")
	must.NoError(t, err)
	must.StrContains(t, resp.Warnings, "looks fine")

	review := <-hook.reviews
	must.Eq(t, structs.AdmissionOperationRegister, review.Operation)
	must.Eq(t, "default", review.Namespace)
	must.Eq(t, "anonymous", review.User.Type)

	job = mock.Job()
	job.Meta["owner"] = "mallory"
	_, err = admissionRegisterJob(t, s, job, "")
	must.ErrorContains(t, err, "mallory may not deploy")
	_, err = admissionPlanJob(t, s, job)
	must.ErrorContains(t, err, "mallory may not deploy")

	must.Eq(t, 0, excluded.calls.Load())
}

func TestJobEndpoint_Admission_FailurePolicy(t *testing.T) {
	ci.Parallel(t)

	// An unreachable webhook.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	t.Run("fail", func(t *testing.T) {
		s, cleanup := admissionServer(t, &config.AdmissionWebhookConfig{
			Name: "dead", URL: deadURL, Timeout: time.Second,
		})
		defer cleanup()

		_, err := admissionRegisterJob(t, s, mock.Job(), "")
		must.ErrorContains(t, err, `admission webhook "dead" failed`)
	})

	t.Run("ignore", func(t *testing.T) {
		s, cleanup := admissionServer(t,
			&config.AdmissionWebhookConfig{
				Name: "dead", URL: deadURL, Timeout: time.Second,
				FailurePolicy: config.AdmissionFailurePolicyIgnore,
			},
			&config.AdmissionWebhookConfig{
				Name: "dead-mutating", URL: deadURL, Timeout: time.Second,
				Mode: config.AdmissionWebhookModeMutate, FailurePolicy: config.AdmissionFailurePolicyIgnore,
			},
		)
		defer cleanup()

		resp, err := admissionRegisterJob(t, s, mock.Job(), "")
		must.NoError(t, err)
		must.StrContains(t, resp.Warnings, `admission webhook "dead" failed and was ignored`)
		must.StrContains(t, resp.Warnings, `admission webhook "dead-mutating" failed and was ignored`)
	})
}

func TestJobEndpoint_Admission_WebhookACL(t *testing.T) {
	ci.Parallel(t)

	hook := newTestWebhook(t, func(req *admission.ReviewRequest) *admission.ReviewResponse {
		return &admission.ReviewResponse{Allowed: true}
	})

	s, root, cleanup := TestACLServer(t, func(c *Config) {
		c.NumSchedulers = 0
		c.AdmissionConfig = &config.AdmissionConfig{Webhooks: []*config.AdmissionWebhookConfig{
			{Name: "mutate", URL: hook.URL, Mode: config.AdmissionWebhookModeMutate},
		}}
	})
	defer cleanup()
	testutil.WaitForLeader(t, s.RPC)

	// A token that may not submit jobs never reaches the webhook.
	readOnly := mock.CreatePolicyAndToken(t, s.State(), 1001, "read-only",
		mock.NamespacePolicy(structs.DefaultNamespace, "", []string{acl.NamespaceCapabilityReadJob}))
	_, err := admissionRegisterJob(t, s, mock.Job(), readOnly.SecretID)
	must.EqError(t, err, structs.ErrPermissionDenied.Error())
	must.Eq(t, 0, hook.calls.Load())

	// The submitter is sent to the webhook.
	_, err = admissionRegisterJob(t, s, mock.Job(), root.SecretID)
	must.NoError(t, err)
	review := <-hook.reviews
	must.Eq(t, root.AccessorID, review.User.AccessorID)
	must.Eq(t, structs.ACLManagementToken, review.User.Type)
}
