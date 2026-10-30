// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/hashicorp/nomad/nomad/admission"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
)

// ReleaseAdmissionDeniedPrefix starts the error of releases rejected by
// admission control.
const ReleaseAdmissionDeniedPrefix = "release denied by admission control"

// admitRelease runs the admission policies that select releases in the
// namespace: the built-in signature check of their trusted keys, and their
// CEL rules with the variable "release". It returns the warnings of "warn"
// policies and fails for violated "deny" policies. The key ID of a verified
// signature is recorded in the pack as TrustedSigner.
func (m *releaseManager) admitRelease(snap *state.StateSnapshot, op *releaseOp, in *applyInput) ([]string, error) {
	if m.srv.admission == nil || m.srv.admission.policies == nil {
		return nil, nil
	}
	engine := m.srv.admission.policies
	policies, err := matchingAdmissionPolicies(snap, engine, func(p *structs.AdmissionPolicy) bool {
		return p.AppliesToRelease(op.namespace)
	})
	if err != nil || len(policies) == 0 {
		return nil, err
	}

	pack := in.pack
	sigs := make([]*admission.PackSignature, 0, len(pack.Signatures))
	for _, s := range pack.Signatures {
		if s != nil {
			sigs = append(sigs, &admission.PackSignature{Payload: []byte(s.Payload), Signature: s.Signature})
		}
	}

	var warnings, denials []string
	verdict := func(p *structs.AdmissionPolicy, msg string) {
		if p.Action == structs.AdmissionPolicyActionWarn {
			warnings = append(warnings, msg)
		} else {
			denials = append(denials, msg)
		}
	}

	var withRules []*structs.AdmissionPolicy
	for _, p := range policies {
		if len(p.Rules) > 0 {
			withRules = append(withRules, p)
		}
		if len(p.TrustedKeys) == 0 {
			continue
		}
		var keys []*admission.TrustedKey
		for _, pemKey := range p.TrustedKeys {
			if k, err := admission.ParseTrustedKey(pemKey); err == nil {
				keys = append(keys, k)
			}
		}
		signer := admission.VerifyPackSignatures(pack.Digest, sigs, keys)
		switch {
		case signer != "":
			pack.TrustedSigner = signer
		case pack.Digest == "":
			verdict(p, fmt.Sprintf("admission policy %q: the pack has no content digest; only signed packs from OCI registries or bundles are allowed in namespace %q",
				p.Name, op.namespace))
		case len(sigs) == 0:
			verdict(p, fmt.Sprintf("admission policy %q: the pack %s is not signed", p.Name, pack.Digest))
		default:
			verdict(p, fmt.Sprintf("admission policy %q: the pack %s is not signed by a trusted key", p.Name, pack.Digest))
		}
	}

	if len(withRules) > 0 {
		user := admissionUser(&structs.GenericRequest{})
		request := map[string]any{
			"operation": structs.AdmissionOperationApply,
			"namespace": op.namespace,
			"dryRun":    in.dryRun,
			"user":      map[string]any{"name": op.user, "type": user.Type},
		}
		release := map[string]any{
			"name":      op.name,
			"namespace": op.namespace,
			"action":    in.action,
			"pack": map[string]any{
				"name":          pack.Name,
				"version":       pack.Version,
				"registry":      pack.Registry,
				"path":          pack.Path,
				"digest":         pack.Digest,
				"signer":         pack.Signer,
				"signed":         len(sigs) > 0,
				"trustedSigner":  pack.TrustedSigner,
				"serverRendered": in.serverRendered,
			},
			"resources": releaseResourceSummary(in.resources),
		}
		for _, v := range engine.EvaluateRelease(context.Background(), withRules, release, request) {
			verdict(&structs.AdmissionPolicy{Action: v.Action}, v.Message)
		}
	}

	if len(denials) > 0 {
		return nil, structs.NewErrRPCCodedf(http.StatusForbidden, "%s: %s", ReleaseAdmissionDeniedPrefix, strings.Join(denials, "; "))
	}
	return warnings, nil
}

// releaseResourceSummary describes the resources of a release for CEL
// rules: a list of {type, namespace, name}.
func releaseResourceSummary(resources []*structs.ReleaseResource) []any {
	out := make([]any, 0, len(resources))
	for _, res := range resources {
		if res == nil {
			continue
		}
		ref := res.Ref()
		out = append(out, map[string]any{"type": ref.Type, "namespace": ref.Namespace, "name": ref.Name})
	}
	return out
}
