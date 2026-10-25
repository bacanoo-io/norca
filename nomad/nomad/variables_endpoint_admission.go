// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/admission"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
)

// variableWebhooksFor returns the validating webhooks that apply to writes of
// the Variable at path in the namespace, in configured order. Mutating
// webhooks never apply to Variables.
func (d *dynamicAdmission) variableWebhooksFor(namespace, path string) []*admission.Webhook {
	if d == nil {
		return nil
	}
	var out []*admission.Webhook
	for _, w := range d.webhooks {
		if w.AppliesToVariable(namespace, path) {
			out = append(out, w)
		}
	}
	return out
}

// variableViolation is a reason to reject or warn about a Variables write.
type variableViolation struct {
	controller string
	name       string
	action     string

	// message is returned to the writer, eventMessage goes into the
	// VariableAdmissionDenied event, which must not reveal the items.
	message      string
	eventMessage string
}

// variableAdmission holds the state of the admission of one Variables write.
type variableAdmission struct {
	sv        *Variables
	snap      *state.StateSnapshot
	op        string
	namespace string
	path      string
	user      *structs.AdmissionUser
	newVar    *admission.VariableInput

	// oldVar is the Variable that the write replaces or deletes, loaded and
	// decrypted on first use.
	oldLoaded bool
	oldVar    *admission.VariableInput
}

// old returns the existing Variable at the path of the write, or nil.
func (va *variableAdmission) old() (*admission.VariableInput, error) {
	if va.oldLoaded {
		return va.oldVar, nil
	}
	v, err := va.decryptVariable(va.path)
	if err != nil {
		return nil, err
	}
	va.oldVar = v
	va.oldLoaded = true
	return v, nil
}

// decryptVariable returns the decrypted Variable at path in the namespace of
// the write, or nil if it does not exist.
func (va *variableAdmission) decryptVariable(path string) (*admission.VariableInput, error) {
	ev, err := va.snap.GetVariable(nil, va.namespace, path)
	if err != nil {
		return nil, err
	}
	if ev == nil {
		return nil, nil
	}
	dv, err := va.sv.decrypt(ev)
	if err != nil {
		return nil, fmt.Errorf("admission: failed to decrypt variable %q: %w", path, err)
	}
	return &admission.VariableInput{
		Namespace:   dv.Namespace,
		Path:        dv.Path,
		Items:       dv.Items,
		CreateIndex: dv.CreateIndex,
		ModifyIndex: dv.ModifyIndex,
	}, nil
}

// enforceAdmission runs the dynamic admission controllers that opted into
// Variables writes: the CEL admission policies (with their optional
// crd-schema validation) and then the validating admission webhooks. It runs
// on the leader after the writer has been authorized and before the Raft
// write. It returns the warnings and the rejection error.
func (sv *Variables) enforceAdmission(args *structs.VariablesApplyRequest) ([]string, error) {
	d := sv.srv.admission
	if d == nil || args.Var == nil {
		return nil, nil
	}
	namespace, path := args.Var.Namespace, args.Var.Path

	snap, err := sv.srv.State().Snapshot()
	if err != nil {
		return nil, err
	}
	policies, err := matchingAdmissionPolicies(snap, d.policies, func(p *structs.AdmissionPolicy) bool {
		return p.AppliesToVariable(namespace, path)
	})
	if err != nil {
		return nil, err
	}
	webhooks := d.variableWebhooksFor(namespace, path)
	if len(policies) == 0 && len(webhooks) == 0 {
		return nil, nil
	}

	va := &variableAdmission{
		sv:        sv,
		snap:      snap,
		op:        structs.AdmissionOperationSet,
		namespace: namespace,
		path:      path,
		user:      admissionUser(args),
		newVar:    &admission.VariableInput{Namespace: namespace, Path: path},
	}
	switch args.Op {
	case structs.VarOpDelete, structs.VarOpDeleteCAS:
		va.op = structs.AdmissionOperationDelete
	default:
		va.newVar.Items = args.Var.Items
	}
	cas := args.Op == structs.VarOpCAS || args.Op == structs.VarOpDeleteCAS

	var warnings []string

	// CEL admission policies and crd-schema validation.
	if len(policies) > 0 {
		violations, err := va.evaluatePolicies(policies, cas)
		if err != nil {
			return nil, err
		}
		var denials []*variableViolation
		for _, v := range violations {
			if v.action == structs.AdmissionPolicyActionWarn {
				warnings = append(warnings, v.message)
				continue
			}
			denials = append(denials, v)
		}
		if len(denials) > 0 {
			return warnings, va.deny(denials)
		}
	}

	// Validating admission webhooks.
	for _, w := range webhooks {
		resp, err := va.callWebhook(w)
		if err != nil {
			warning, failure := splitWarning(err)
			if failure != nil {
				return warnings, failure
			}
			warnings = append(warnings, warning.Error())
			continue
		}
		if !resp.Allowed {
			msg := resp.Message
			if msg == "" {
				msg = "no reason given"
			}
			return warnings, va.deny([]*variableViolation{{
				controller:   admissionControllerWebhook,
				name:         w.Name(),
				message:      fmt.Sprintf("admission webhook %q denied the write: %s", w.Name(), msg),
				eventMessage: msg,
			}})
		}
		for _, msg := range resp.Warnings {
			warnings = append(warnings, fmt.Sprintf("admission webhook %q: %s", w.Name(), msg))
		}
		if patch, _ := resp.PatchOperations(); patch != nil {
			warnings = append(warnings, fmt.Sprintf(
				"admission webhook %q returned a patch, which is ignored for variables", w.Name()))
		}
	}

	return warnings, nil
}

// evaluatePolicies evaluates the CEL rules of the policies and, for policies
// with schema_from, validates the write against its crd-schema.
func (va *variableAdmission) evaluatePolicies(policies []*structs.AdmissionPolicy, cas bool) ([]*variableViolation, error) {
	d := va.sv.srv.admission
	oldVar, err := va.old()
	if err != nil {
		return nil, err
	}

	request := map[string]any{
		"resource":  structs.AdmissionResourceVariable,
		"operation": va.op,
		"cas":       cas,
		"namespace": va.namespace,
		"path":      va.path,
		"user":      admissionUserValue(va.user),
	}

	var out []*variableViolation
	for _, v := range d.policies.EvaluateVariable(context.Background(), policies,
		va.newVar.Value(va.op), oldVar.Value(""), request) {
		out = append(out, &variableViolation{
			controller:   admissionControllerPolicy,
			name:         v.Policy,
			action:       v.Action,
			message:      v.Message,
			eventMessage: v.Message,
		})
	}

	// The schema is checked once per write, and every policy that opted in
	// reports the result with its own action.
	var schemaErr error
	var schemaChecked bool
	for _, policy := range policies {
		if policy.SchemaFrom != structs.AdmissionSchemaFromCRDSchema {
			continue
		}
		if !schemaChecked {
			schemaErr, err = va.checkSchema()
			if err != nil {
				return nil, err
			}
			schemaChecked = true
		}
		if schemaErr == nil {
			continue
		}
		out = append(out, &variableViolation{
			controller:   admissionControllerPolicy,
			name:         policy.Name,
			action:       policy.Action,
			message:      fmt.Sprintf("admission policy %q: %v", policy.Name, schemaErr),
			eventMessage: "schema validation failed",
		})
	}
	return out, nil
}

// checkSchema validates a write of a crd/<group>/<kind>/<name> Variable
// against the crd-schema/<group>/<kind> Variable if that exists, and checks
// that a write of a crd-schema Variable holds a valid schema. Deletes are not
// checked. It returns the validation failure, and an error if the check could
// not be done.
func (va *variableAdmission) checkSchema() (error, error) {
	if va.op != structs.AdmissionOperationSet {
		return nil, nil
	}

	if admission.IsCRDSchemaPath(va.path) {
		if _, err := admission.CompileVariableSchema(va.newVar.Items); err != nil {
			return fmt.Errorf("invalid schema: %v", err), nil
		}
		return nil, nil
	}

	schemaPath, ok := admission.CRDSchemaPath(va.path)
	if !ok {
		return nil, nil
	}
	schemaVar, err := va.decryptVariable(schemaPath)
	if err != nil {
		return nil, err
	}
	if schemaVar == nil {
		return nil, nil
	}
	schema, err := va.sv.srv.admission.schemas.Schema(va.namespace, schemaPath, schemaVar.ModifyIndex, schemaVar.Items)
	if err != nil {
		return fmt.Errorf("schema %q is invalid: %v", schemaPath, err), nil
	}
	if err := schema.Validate(va.newVar.Items); err != nil {
		return fmt.Errorf("variable does not match schema %q: %v", schemaPath, err), nil
	}
	return nil, nil
}

// callWebhook sends the write to a validating webhook. The decrypted items
// are only sent if the webhook opted in. If the call fails, the error is an
// *admissionWarning when the failure policy of the webhook ignores failures.
func (va *variableAdmission) callWebhook(w *admission.Webhook) (*admission.ReviewResponse, error) {
	include := w.IncludeVariableItems()

	// The previous Variable is only decrypted if its items are sent.
	var oldVar *admission.VariableInput
	if include {
		v, err := va.old()
		if err != nil {
			return nil, err
		}
		oldVar = v
	} else {
		ev, err := va.snap.GetVariable(nil, va.namespace, va.path)
		if err != nil {
			return nil, err
		}
		if ev != nil {
			oldVar = &admission.VariableInput{
				Namespace:   ev.Namespace,
				Path:        ev.Path,
				CreateIndex: ev.CreateIndex,
				ModifyIndex: ev.ModifyIndex,
			}
		}
	}

	req := &admission.ReviewRequest{
		UID:         uuid.Generate(),
		Resource:    structs.AdmissionResourceVariable,
		Operation:   va.op,
		Namespace:   va.namespace,
		User:        admission.NewReviewUser(va.user),
		Variable:    va.newVar.Review(include),
		OldVariable: oldVar.Review(include),
	}

	resp, err := w.Review(va.sv.srv.shutdownCtx, req)
	if err != nil {
		return nil, va.sv.srv.webhookFailure(w, err)
	}
	return resp, nil
}

// deny publishes a VariableAdmissionDenied event for the first denial and
// returns the rejection error listing every denial.
func (va *variableAdmission) deny(denials []*variableViolation) error {
	first := denials[0]
	va.sv.srv.publishVariableAdmissionDenied(&structs.VariableAdmissionDeniedEvent{
		Namespace:  va.namespace,
		Path:       va.path,
		Operation:  va.op,
		User:       va.user,
		Controller: first.controller,
		Name:       first.name,
		Message:    first.eventMessage,
	})

	msgs := make([]string, len(denials))
	for i, d := range denials {
		msgs[i] = d.message
	}
	return fmt.Errorf("%s: %s", structs.VariableAdmissionDeniedPrefix, strings.Join(msgs, "; "))
}
