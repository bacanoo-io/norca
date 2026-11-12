// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/admission"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/stream"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/norca/structs/config"
	"github.com/bacanoo-io/norca/norca/testutil"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
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
	// A denial is the answer 403 of the HTTP API, not a server error.
	must.ErrorContains(t, err, "RPC Error:: 403,")
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

// A patch that puts a null into a list of the job is refused: the nil task
// group it decodes to made the canonicalizer panic.
func TestJobEndpoint_Admission_MutatingWebhook_NullListElement(t *testing.T) {
	ci.Parallel(t)

	patches := map[string]string{
		"task group": `[{"op":"replace","path":"/TaskGroups/0","value":null}]`,
		"task":       `[{"op":"add","path":"/TaskGroups/0/Tasks/-","value":null}]`,
	}
	for name, patch := range patches {
		t.Run(name, func(t *testing.T) {
			hook := newTestWebhook(t, func(req *admission.ReviewRequest) *admission.ReviewResponse {
				return &admission.ReviewResponse{Allowed: true, Patch: json.RawMessage(patch)}
			})

			s, cleanup := admissionServer(t, &config.AdmissionWebhookConfig{
				Name: "nuller", URL: hook.URL, Mode: config.AdmissionWebhookModeMutate,
			})
			defer cleanup()

			_, err := admissionRegisterJob(t, s, mock.Job(), "")
			must.ErrorContains(t, err, "must not put a null into a list of the job")
		})
	}
}

// A null value is a value: a webhook may clear a member of the job with it.
func TestJobEndpoint_Admission_MutatingWebhook_NullValue(t *testing.T) {
	ci.Parallel(t)

	hook := newTestWebhook(t, func(req *admission.ReviewRequest) *admission.ReviewResponse {
		return &admission.ReviewResponse{
			Allowed: true,
			Patch:   json.RawMessage(`[{"op":"replace","path":"/Meta","value":null}]`),
		}
	})

	s, cleanup := admissionServer(t, &config.AdmissionWebhookConfig{
		Name: "clearer", URL: hook.URL, Mode: config.AdmissionWebhookModeMutate,
	})
	defer cleanup()

	job := mock.Job()
	job.Meta = map[string]string{"owner": "armon"}
	_, err := admissionRegisterJob(t, s, job, "")
	must.NoError(t, err)

	stored, err := s.State().JobByID(nil, job.Namespace, job.ID)
	must.NoError(t, err)
	must.MapEmpty(t, stored.Meta)
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
	must.ErrorContains(t, err, "RPC Error:: 403,")
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

// A scale writes the job without a registration: the admission policies and
// the validating webhooks decide on the new count, with the operation
// "scale".
func TestJobEndpoint_Admission_Scale(t *testing.T) {
	ci.Parallel(t)

	var operations atomic.Value
	hook := newTestWebhook(t, func(req *admission.ReviewRequest) *admission.ReviewResponse {
		operations.Store(req.Operation)
		var job structs.Job
		_ = json.Unmarshal(req.Job, &job)
		if job.TaskGroups[0].Count > 8 {
			return &admission.ReviewResponse{Allowed: false, Message: "more than 8 is too many"}
		}
		return &admission.ReviewResponse{Allowed: true}
	})

	s, cleanup := admissionServer(t, &config.AdmissionWebhookConfig{Name: "count-check", URL: hook.URL})
	defer cleanup()
	upsertAdmissionPolicies(t, s,
		testAdmissionPolicy("small-groups", nil, "deny", `job.TaskGroups.all(g, g.Count <= 5)`))

	job := mock.Job()
	job.TaskGroups[0].Count = 1
	_, err := admissionRegisterJob(t, s, job, "")
	must.NoError(t, err)

	scale := func(count int64) error {
		req := &structs.JobScaleRequest{
			JobID:        job.ID,
			Target:       map[string]string{structs.ScalingTargetGroup: job.TaskGroups[0].Name},
			Count:        &count,
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace},
		}
		var resp structs.JobRegisterResponse
		return msgpackrpc.CallWithCodec(rpcClient(t, s), "Job.Scale", req, &resp)
	}
	count := func() int {
		stored, err := s.State().JobByID(nil, job.Namespace, job.ID)
		must.NoError(t, err)
		return stored.TaskGroups[0].Count
	}

	// The policy denies the count, the job is unchanged.
	err = scale(6)
	must.ErrorContains(t, err, "small-groups violated")
	must.ErrorContains(t, err, "RPC Error:: 403,")
	must.Eq(t, 1, count())

	// A count the policy and the webhook allow is written.
	must.NoError(t, scale(3))
	must.Eq(t, 3, count())
	must.Eq(t, structs.AdmissionOperationScale, operations.Load().(string))

	// Without the policy the webhook decides.
	var resp structs.GenericResponse
	must.NoError(t, msgpackrpc.CallWithCodec(rpcClient(t, s), "AdmissionPolicy.DeleteAdmissionPolicies",
		&structs.AdmissionPolicyDeleteRequest{
			Names:        []string{"small-groups"},
			WriteRequest: structs.WriteRequest{Region: "global"},
		}, &resp))
	must.ErrorContains(t, scale(9), "more than 8 is too many")
	must.Eq(t, 3, count())
}

// A dispatch makes a job with the meta of the caller: the admission policies
// decide on the child, with the operation "dispatch".
func TestJobEndpoint_Admission_Dispatch(t *testing.T) {
	ci.Parallel(t)

	s, cleanup := admissionServer(t)
	defer cleanup()

	parent := mock.BatchJob()
	parent.ParameterizedJob = &structs.ParameterizedJobConfig{MetaOptional: []string{"team"}}
	_, err := admissionRegisterJob(t, s, parent, "")
	must.NoError(t, err)

	upsertAdmissionPolicies(t, s, testAdmissionPolicy("known-team", nil, "deny",
		`request.operation != 'dispatch' || (has(job.Meta.team) && job.Meta.team == 'platform')`))

	dispatch := func(team string) (*structs.JobDispatchResponse, error) {
		req := &structs.JobDispatchRequest{
			JobID:        parent.ID,
			Meta:         map[string]string{"team": team},
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: parent.Namespace},
		}
		var resp structs.JobDispatchResponse
		err := msgpackrpc.CallWithCodec(rpcClient(t, s), "Job.Dispatch", req, &resp)
		return &resp, err
	}

	_, err = dispatch("mallory")
	must.ErrorContains(t, err, "known-team violated")
	must.ErrorContains(t, err, "RPC Error:: 403,")

	resp, err := dispatch("platform")
	must.NoError(t, err)
	child, err := s.State().JobByID(nil, parent.Namespace, resp.DispatchedJobID)
	must.NoError(t, err)
	must.NotNil(t, child)
}

// A plan is refused when the namespace of the request is not that of the
// job: the ACL check is made for the first, the admission controllers and
// the plan work on the second.
func TestJobEndpoint_Admission_PlanNamespaceMismatch(t *testing.T) {
	ci.Parallel(t)

	s, cleanup := admissionServer(t)
	defer cleanup()

	job := mock.Job()
	job.Namespace = "other"
	req := &structs.JobPlanRequest{
		Job:          job,
		Diff:         true,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace},
	}
	var resp structs.JobPlanResponse
	err := msgpackrpc.CallWithCodec(rpcClient(t, s), "Job.Plan", req, &resp)
	must.ErrorContains(t, err, "mismatched request namespace")
}

// recordingConfigEntries counts the configuration entries written to Consul.
type recordingConfigEntries struct {
	ConsulConfigsAPI
	writes atomic.Int64
}

func (r *recordingConfigEntries) SetIngressCE(context.Context, string, string, *structs.ConsulIngressConfigEntry) error {
	r.writes.Add(1)
	return nil
}

func (r *recordingConfigEntries) SetTerminatingCE(context.Context, string, string, *structs.ConsulTerminatingConfigEntry) error {
	r.writes.Add(1)
	return nil
}

func (r *recordingConfigEntries) Stop() {}

// A job that an admission policy denies leaves nothing behind in Consul: its
// gateway configuration entry is written after the policies decided.
func TestJobEndpoint_Admission_DeniedBeforeConfigEntries(t *testing.T) {
	ci.Parallel(t)

	s, cleanup := admissionServer(t)
	defer cleanup()
	entries := &recordingConfigEntries{}
	s.consulConfigEntries = entries

	upsertAdmissionPolicies(t, s, testAdmissionPolicy("no-mallory", nil, "deny",
		`!has(job.Meta.owner) || job.Meta.owner != 'mallory'`))

	denied := mock.ConnectIngressGatewayJob("host", false)
	denied.Meta = map[string]string{"owner": "mallory"}
	_, err := admissionRegisterJob(t, s, denied, "")
	must.ErrorContains(t, err, "no-mallory violated")
	must.Eq(t, 0, entries.writes.Load())

	allowed := mock.ConnectIngressGatewayJob("host", false)
	_, err = admissionRegisterJob(t, s, allowed, "")
	must.NoError(t, err)
	must.Eq(t, 1, entries.writes.Load())
}

// What a mutating webhook adds goes through the built-in mutators: a service
// of the native service discovery it adds gets the constraint on the nodes
// that every such service implies.
func TestJobEndpoint_Admission_MutatingWebhook_BuiltinMutatorsRunAgain(t *testing.T) {
	ci.Parallel(t)

	hook := newTestWebhook(t, func(req *admission.ReviewRequest) *admission.ReviewResponse {
		return &admission.ReviewResponse{
			Allowed: true,
			Patch: json.RawMessage(
				`[{"op":"add","path":"/TaskGroups/0/Services","value":[{"Name":"injected","Provider":"nomad"}]}]`),
		}
	})

	s, cleanup := admissionServer(t, &config.AdmissionWebhookConfig{
		Name: "add-service", URL: hook.URL, Mode: config.AdmissionWebhookModeMutate,
	})
	defer cleanup()

	job := mock.Job()
	job.TaskGroups[0].Services = nil
	for _, task := range job.TaskGroups[0].Tasks {
		task.Services = nil
	}
	_, err := admissionRegisterJob(t, s, job, "")
	must.NoError(t, err)

	stored, err := s.State().JobByID(nil, job.Namespace, job.ID)
	must.NoError(t, err)
	must.Len(t, 1, stored.TaskGroups[0].Services)

	found := false
	for _, c := range stored.TaskGroups[0].Constraints {
		if c.LTarget == "${attr.nomad.service_discovery}" {
			found = true
		}
	}
	must.True(t, found, must.Sprintf("constraints: %v", stored.TaskGroups[0].Constraints))
}
