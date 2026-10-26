// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"context"
	"fmt"

	metrics "github.com/armon/go-metrics"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/admission"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/nomad/structs/config"
)

const (
	admissionControllerPolicy  = "policy"
	admissionControllerWebhook = "webhook"
)

// admissionRequest carries the request context that the dynamic admission
// controllers need: the operation and the identity of the submitter.
type admissionRequest struct {
	operation string
	args      structs.RequestWithIdentity
}

// jobRequestMutator is implemented by mutators that need the request context.
// The admission chain calls MutateWithRequest instead of Mutate for them.
type jobRequestMutator interface {
	jobMutator
	MutateWithRequest(*admissionRequest, *structs.Job) (*structs.Job, []error, error)
}

// dynamicAdmission holds the configured admission webhooks and the CEL
// policy engine of a server.
type dynamicAdmission struct {
	logger   hclog.Logger
	webhooks []*admission.Webhook
	policies *admission.PolicyEngine

	// schemas caches the compiled crd-schema Variables used by policies
	// with schema_from.
	schemas *admission.SchemaCache
}

// newDynamicAdmission builds the dynamic admission controllers from the
// server configuration.
func newDynamicAdmission(cfg *config.AdmissionConfig, logger hclog.Logger) (*dynamicAdmission, error) {
	engine, err := admission.NewPolicyEngine()
	if err != nil {
		return nil, err
	}
	d := &dynamicAdmission{
		logger:   logger.Named("admission"),
		policies: engine,
		schemas:  admission.NewSchemaCache(),
	}

	if cfg != nil {
		cfg = cfg.Copy()
		cfg.Canonicalize()
		if err := cfg.Validate(); err != nil {
			return nil, err
		}
		for _, wcfg := range cfg.Webhooks {
			w, err := admission.NewWebhook(wcfg)
			if err != nil {
				return nil, err
			}
			d.webhooks = append(d.webhooks, w)
		}
	}
	return d, nil
}

// webhooksFor returns the webhooks of the given mode that apply to the
// namespace, in configured order.
func (d *dynamicAdmission) webhooksFor(namespace string, mutating bool) []*admission.Webhook {
	if d == nil {
		return nil
	}
	var out []*admission.Webhook
	for _, w := range d.webhooks {
		if w.Mutating() == mutating && w.AppliesTo(namespace) {
			out = append(out, w)
		}
	}
	return out
}

// admissionUser returns the admission view of the submitter of a request.
func admissionUser(args structs.RequestWithIdentity) *structs.AdmissionUser {
	if args == nil {
		return structs.NewAdmissionUser(nil, nil)
	}
	identity := args.GetIdentity()
	return structs.NewAdmissionUser(identity.GetACLToken(), identity.GetClaims())
}

// publishAdmissionDenied emits a JobAdmissionDenied event. The event is not
// written to Raft, so it is only published on the server that ran the
// admission chain (the leader).
func (s *Server) publishAdmissionDenied(ev *structs.JobAdmissionDeniedEvent) {
	metrics.IncrCounterWithLabels([]string{"nomad", "admission", "denied"}, 1, []metrics.Label{
		{Name: "controller", Value: ev.Controller},
		{Name: "name", Value: ev.Name},
	})

	store := s.State()
	if store == nil {
		return
	}
	broker, err := store.EventBroker()
	if err != nil {
		return
	}
	index, _ := store.LatestIndex()
	broker.Publish(&structs.Events{
		Index: index,
		Events: []structs.Event{{
			Topic:      structs.TopicAdmissionPolicy,
			Type:       structs.TypeJobAdmissionDenied,
			Key:        ev.JobID,
			Namespace:  ev.Namespace,
			FilterKeys: []string{ev.Name},
			Index:      index,
			Payload:    ev,
		}},
	})
}

// publishVariableAdmissionDenied emits a VariableAdmissionDenied event, like
// publishAdmissionDenied does for jobs. The event never carries the items of
// the Variable.
func (s *Server) publishVariableAdmissionDenied(ev *structs.VariableAdmissionDeniedEvent) {
	metrics.IncrCounterWithLabels([]string{"nomad", "admission", "denied"}, 1, []metrics.Label{
		{Name: "controller", Value: ev.Controller},
		{Name: "name", Value: ev.Name},
	})

	store := s.State()
	if store == nil {
		return
	}
	broker, err := store.EventBroker()
	if err != nil {
		return
	}
	index, _ := store.LatestIndex()
	broker.Publish(&structs.Events{
		Index: index,
		Events: []structs.Event{{
			Topic:      structs.TopicAdmissionPolicy,
			Type:       structs.TypeVariableAdmissionDenied,
			Key:        ev.Path,
			Namespace:  ev.Namespace,
			FilterKeys: []string{ev.Name},
			Index:      index,
			Payload:    ev,
		}},
	})
}

// callWebhook sends the job to a webhook and returns its response. If the call
// fails, the error is an *admissionWarning when the failure policy of the
// webhook ignores failures.
func (s *Server) callWebhook(w *admission.Webhook, op string, user *structs.AdmissionUser,
	job *structs.Job) (*admission.ReviewResponse, error) {

	raw, err := admission.EncodeJob(job)
	if err != nil {
		return nil, err
	}
	req := &admission.ReviewRequest{
		UID:       uuid.Generate(),
		Resource:  structs.AdmissionResourceJob,
		Operation: op,
		DryRun:    op == structs.AdmissionOperationPlan,
		Namespace: job.Namespace,
		User:      admission.NewReviewUser(user),
		Job:       raw,
	}

	resp, err := w.Review(s.shutdownCtx, req)
	if err != nil {
		return nil, s.webhookFailure(w, err)
	}
	return resp, nil
}

// webhookFailure applies the failure policy of the webhook to a failed call.
// It returns a warning for "ignore" and an error for "fail".
func (s *Server) webhookFailure(w *admission.Webhook, err error) error {
	if w.IgnoreFailure() {
		s.admission.logger.Warn("admission webhook failed, ignoring", "webhook", w.Name(), "error", err)
		return &admissionWarning{fmt.Errorf("admission webhook %q failed and was ignored: %v", w.Name(), err)}
	}
	s.admission.logger.Warn("admission webhook failed, rejecting job", "webhook", w.Name(), "error", err)
	return fmt.Errorf("admission webhook %q failed: %v", w.Name(), err)
}

// admissionWarning marks an error that is only a warning.
type admissionWarning struct{ error }

func splitWarning(err error) (warning, failure error) {
	if w, ok := err.(*admissionWarning); ok {
		return w.error, nil
	}
	return nil, err
}

// webhookDenied publishes the denial event and returns the rejection error.
func (s *Server) webhookDenied(w *admission.Webhook, op string, user *structs.AdmissionUser,
	job *structs.Job, resp *admission.ReviewResponse) error {

	msg := resp.Message
	if msg == "" {
		msg = "no reason given"
	}
	s.publishAdmissionDenied(&structs.JobAdmissionDeniedEvent{
		Namespace:  job.Namespace,
		JobID:      job.ID,
		Operation:  op,
		User:       user,
		Controller: admissionControllerWebhook,
		Name:       w.Name(),
		Message:    msg,
	})
	return fmt.Errorf("admission webhook %q denied the job: %s", w.Name(), msg)
}

// admissionWebhookMutator calls the mutating admission webhooks. It runs after
// the built-in mutators and before the built-in validators.
type admissionWebhookMutator struct {
	srv *Server
}

func (admissionWebhookMutator) Name() string {
	return "admission-webhooks"
}

// Mutate is a no-op: mutating webhooks only run for register and plan, which
// call MutateWithRequest.
func (admissionWebhookMutator) Mutate(job *structs.Job) (*structs.Job, []error, error) {
	return job, nil, nil
}

func (m admissionWebhookMutator) MutateWithRequest(areq *admissionRequest, job *structs.Job) (*structs.Job, []error, error) {
	if areq == nil || m.srv.admission == nil {
		return job, nil, nil
	}
	webhooks := m.srv.admission.webhooksFor(job.Namespace, true)
	if len(webhooks) == 0 {
		return job, nil, nil
	}

	// The chain runs before the endpoint authorizes the submission. Never
	// send a job to a webhook on behalf of a submitter that is not allowed
	// to submit it; the endpoint rejects the request right after.
	aclObj, err := m.srv.ResolveACL(areq.args)
	if err != nil {
		return job, nil, nil
	}
	if aclObj != nil && !aclObj.AllowNsOp(job.Namespace, acl.NamespaceCapabilitySubmitJob) {
		return job, nil, nil
	}

	user := admissionUser(areq.args)
	var warnings []error
	patched := false

	for _, w := range webhooks {
		resp, err := m.srv.callWebhook(w, areq.operation, user, job)
		if err != nil {
			warning, failure := splitWarning(err)
			if failure != nil {
				return nil, nil, failure
			}
			warnings = append(warnings, warning)
			continue
		}
		if !resp.Allowed {
			return nil, nil, m.srv.webhookDenied(w, areq.operation, user, job, resp)
		}
		for _, msg := range resp.Warnings {
			warnings = append(warnings, fmt.Errorf("admission webhook %q: %s", w.Name(), msg))
		}

		newJob, err := applyWebhookPatch(job, resp)
		if err != nil {
			warning, failure := splitWarning(m.srv.webhookFailure(w, err))
			if failure != nil {
				return nil, nil, failure
			}
			warnings = append(warnings, warning)
			continue
		}
		if newJob != job {
			job = newJob
			patched = true
		}
	}

	if patched {
		// Bring the patched job back to its canonical form before the
		// built-in validators see it.
		var err error
		job, _, err = (&jobCanonicalizer{srv: m.srv}).Mutate(job)
		if err != nil {
			return nil, nil, err
		}
	}
	return job, warnings, nil
}

// applyWebhookPatch applies the patch of a mutating webhook response to the
// job. It returns the original job if there is no patch.
func applyWebhookPatch(job *structs.Job, resp *admission.ReviewResponse) (*structs.Job, error) {
	patch, err := resp.PatchOperations()
	if err != nil || patch == nil {
		return job, err
	}

	raw, err := admission.EncodeJob(job)
	if err != nil {
		return nil, err
	}
	raw, err = admission.ApplyJSONPatch(raw, patch)
	if err != nil {
		return nil, err
	}
	newJob, err := admission.DecodeJob(raw)
	if err != nil {
		return nil, err
	}

	// A webhook may not move the job: authorization was done for the
	// original identity.
	if newJob.ID != job.ID || newJob.Namespace != job.Namespace || newJob.Region != job.Region {
		return nil, fmt.Errorf("patch must not change the job ID, namespace or region")
	}

	// Secrets are never sent to webhooks; keep the submitted ones.
	newJob.VaultToken = job.VaultToken
	newJob.ConsulToken = job.ConsulToken
	return newJob, nil
}

// enforceAdmission runs the CEL admission policies and then the validating
// admission webhooks against the final job. It runs in the slot of
// enforceSubmitJob, after the submitter has been authorized. It returns the
// warnings and the rejection error.
func (j *Job) enforceAdmission(op string, job *structs.Job, token *structs.ACLToken) (error, error) {
	d := j.srv.admission
	if d == nil {
		return nil, nil
	}

	snap, err := j.srv.State().Snapshot()
	if err != nil {
		return nil, err
	}
	policies, err := admissionPoliciesFor(snap, job.Namespace, d.policies)
	if err != nil {
		return nil, err
	}
	webhooks := d.webhooksFor(job.Namespace, false)
	if len(policies) == 0 && len(webhooks) == 0 {
		return nil, nil
	}

	user := structs.NewAdmissionUser(token, nil)
	var warnings *multierror.Error

	// CEL admission policies.
	if len(policies) > 0 {
		raw, err := admission.EncodeJob(job)
		if err != nil {
			return nil, err
		}
		jobVal, err := admission.JobValue(raw)
		if err != nil {
			return nil, err
		}
		request := map[string]any{
			"resource":  structs.AdmissionResourceJob,
			"operation": op,
			"namespace": job.Namespace,
			"user":      admissionUserValue(user),
			"secrets":   admissionSecretRefsValue(job, j.srv.config.VaultConfig.IsEnabled()),
		}

		var denials *multierror.Error
		var firstDenial *admission.Violation
		for _, v := range d.policies.Evaluate(context.Background(), policies, jobVal, request) {
			if v.Action == structs.AdmissionPolicyActionWarn {
				warnings = multierror.Append(warnings, v)
				continue
			}
			if firstDenial == nil {
				firstDenial = v
			}
			denials = multierror.Append(denials, v)
		}
		if firstDenial != nil {
			j.srv.publishAdmissionDenied(&structs.JobAdmissionDeniedEvent{
				Namespace:  job.Namespace,
				JobID:      job.ID,
				Operation:  op,
				User:       user,
				Controller: admissionControllerPolicy,
				Name:       firstDenial.Policy,
				Message:    firstDenial.Message,
			})
			return warnings.ErrorOrNil(), denials.ErrorOrNil()
		}
	}

	// Validating admission webhooks.
	for _, w := range webhooks {
		resp, err := j.srv.callWebhook(w, op, user, job)
		if err != nil {
			warning, failure := splitWarning(err)
			if failure != nil {
				return warnings.ErrorOrNil(), failure
			}
			warnings = multierror.Append(warnings, warning)
			continue
		}
		if !resp.Allowed {
			return warnings.ErrorOrNil(), j.srv.webhookDenied(w, op, user, job, resp)
		}
		for _, msg := range resp.Warnings {
			warnings = multierror.Append(warnings, fmt.Errorf("admission webhook %q: %s", w.Name(), msg))
		}
		if patch, _ := resp.PatchOperations(); patch != nil {
			warnings = multierror.Append(warnings, fmt.Errorf(
				"admission webhook %q returned a patch, which is ignored for validating webhooks", w.Name()))
		}
	}

	return warnings.ErrorOrNil(), nil
}

// admissionUserValue returns the submitter as the CEL rules see it in
// request.user.
func admissionUserValue(user *structs.AdmissionUser) map[string]any {
	return map[string]any{
		"accessor_id": user.AccessorID,
		"name":        user.Name,
		"type":        user.Type,
		"roles":       user.Roles,
		"policies":    user.Policies,
	}
}

// admissionPoliciesFor returns the admission policies that apply to jobs in
// the namespace and prunes the compiled program cache of deleted policies.
func admissionPoliciesFor(snap *state.StateSnapshot, namespace string, engine *admission.PolicyEngine) ([]*structs.AdmissionPolicy, error) {
	return matchingAdmissionPolicies(snap, engine, func(p *structs.AdmissionPolicy) bool {
		return p.AppliesToJob(namespace)
	})
}

// matchingAdmissionPolicies returns the admission policies selected by match
// and prunes the compiled program cache of deleted policies.
func matchingAdmissionPolicies(snap *state.StateSnapshot, engine *admission.PolicyEngine,
	match func(*structs.AdmissionPolicy) bool) ([]*structs.AdmissionPolicy, error) {

	iter, err := snap.AdmissionPolicies(nil, state.SortDefault)
	if err != nil {
		return nil, err
	}
	var out []*structs.AdmissionPolicy
	names := make(map[string]struct{})
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		policy := raw.(*structs.AdmissionPolicy)
		names[policy.Name] = struct{}{}
		if match(policy) {
			out = append(out, policy)
		}
	}
	engine.Prune(names)
	return out, nil
}
