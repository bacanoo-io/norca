// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"errors"
	"net/url"
)

const (
	// TopicAdmissionPolicy is the event stream topic of admission policy
	// changes and of JobAdmissionDenied events.
	TopicAdmissionPolicy Topic = "AdmissionPolicy"

	// AdmissionPolicyActionDeny rejects jobs that violate a rule.
	AdmissionPolicyActionDeny = "deny"

	// AdmissionPolicyActionWarn only warns about jobs that violate a rule.
	AdmissionPolicyActionWarn = "warn"

	// AdmissionResourceJob and AdmissionResourceVariable are the object
	// kinds an admission policy can apply to (AdmissionPolicyMatch.Resources).
	AdmissionResourceJob      = "job"
	AdmissionResourceVariable = "variable"

	// AdmissionSchemaFromCRDSchema validates crd/<group>/<kind>/<name>
	// Variables against the JSON Schema of crd-schema/<group>/<kind>.
	AdmissionSchemaFromCRDSchema = "crd-schema"
)

// AdmissionPolicies is used to access the admission policy endpoints.
type AdmissionPolicies struct {
	client *Client
}

// AdmissionPolicies returns a handle on the admission policy endpoints.
func (c *Client) AdmissionPolicies() *AdmissionPolicies {
	return &AdmissionPolicies{client: c}
}

// List is used to list all admission policies.
func (a *AdmissionPolicies) List(q *QueryOptions) ([]*AdmissionPolicy, *QueryMeta, error) {
	var resp []*AdmissionPolicy
	qm, err := a.client.query("/v1/admission-policies", &resp, q)
	if err != nil {
		return nil, nil, err
	}
	return resp, qm, nil
}

// PrefixList is used to list admission policies whose name matches a prefix.
func (a *AdmissionPolicies) PrefixList(prefix string, q *QueryOptions) ([]*AdmissionPolicy, *QueryMeta, error) {
	if q == nil {
		q = &QueryOptions{}
	}
	q.Prefix = prefix
	return a.List(q)
}

// Info is used to fetch a specific admission policy.
func (a *AdmissionPolicies) Info(name string, q *QueryOptions) (*AdmissionPolicy, *QueryMeta, error) {
	if name == "" {
		return nil, nil, errors.New("missing admission policy name")
	}
	var resp AdmissionPolicy
	qm, err := a.client.query("/v1/admission-policy/"+url.PathEscape(name), &resp, q)
	if err != nil {
		return nil, nil, err
	}
	return &resp, qm, nil
}

// Register is used to create or update an admission policy. The server
// compiles the CEL rules and rejects the policy if one does not compile.
func (a *AdmissionPolicies) Register(policy *AdmissionPolicy, w *WriteOptions) (*WriteMeta, error) {
	if policy == nil {
		return nil, errors.New("missing admission policy")
	}
	if policy.Name == "" {
		return nil, errors.New("missing admission policy name")
	}
	wm, err := a.client.put("/v1/admission-policy/"+url.PathEscape(policy.Name), policy, nil, w)
	if err != nil {
		return nil, err
	}
	return wm, nil
}

// Delete is used to delete an admission policy.
func (a *AdmissionPolicies) Delete(name string, w *WriteOptions) (*WriteMeta, error) {
	if name == "" {
		return nil, errors.New("missing admission policy name")
	}
	wm, err := a.client.delete("/v1/admission-policy/"+url.PathEscape(name), nil, nil, w)
	if err != nil {
		return nil, err
	}
	return wm, nil
}

// AdmissionPolicy is a set of CEL rules that jobs submitted to matching
// namespaces must satisfy.
type AdmissionPolicy struct {
	Name        string                 `hcl:"name"`
	Description string                 `hcl:"description,optional"`
	Action      string                 `hcl:"action,optional"`
	Match       *AdmissionPolicyMatch  `hcl:"match,block"`
	SchemaFrom  string                 `hcl:"schema_from,optional"`
	Rules       []*AdmissionPolicyRule `hcl:"rule,block"`

	// TrustedKeys are PEM encoded public keys (cosign format); a policy
	// with the "release" resource then admits only releases of packs
	// signed by one of them.
	TrustedKeys []string `hcl:"trusted_keys,optional"`

	CreateIndex uint64
	ModifyIndex uint64
}

// AdmissionPolicyMatch selects the jobs and Variables a policy applies to.
type AdmissionPolicyMatch struct {
	// Namespaces (glob patterns allowed). Empty means every namespace.
	Namespaces []string `hcl:"namespaces,optional"`

	// Resources is "job", "variable" and/or "release". Empty means jobs only.
	Resources []string `hcl:"resources,optional"`

	// VariablePaths (glob patterns allowed) restricts the Variables writes
	// the policy applies to. Empty means every path.
	VariablePaths []string `hcl:"variable_paths,optional"`
}

// AdmissionPolicyRule is a CEL rule. It is satisfied when Expression
// evaluates to true.
type AdmissionPolicyRule struct {
	Expression string `hcl:"expression"`
	Message    string `hcl:"message,optional"`
}

// JobAdmissionDeniedEvent is the payload of a JobAdmissionDenied event.
type JobAdmissionDeniedEvent struct {
	Namespace  string
	JobID      string
	Operation  string
	User       *AdmissionUser
	Controller string
	Name       string
	Message    string
}

// VariableAdmissionDeniedEvent is the payload of a VariableAdmissionDenied
// event. It never contains the items of the Variable.
type VariableAdmissionDeniedEvent struct {
	Namespace  string
	Path       string
	Operation  string
	User       *AdmissionUser
	Controller string
	Name       string
	Message    string
}

// AdmissionUser identifies the submitter of a job.
type AdmissionUser struct {
	AccessorID string
	Name       string
	Type       string
	Roles      []string
	Policies   []string
}
