// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"fmt"
	"regexp"
	"slices"

	"github.com/hashicorp/go-multierror"
	"github.com/ryanuber/go-glob"
)

const (
	// AdmissionPolicyActionDeny rejects a job that violates one of the rules
	// of the policy. It is the default action.
	AdmissionPolicyActionDeny = "deny"

	// AdmissionPolicyActionWarn only returns a warning for a job that violates
	// one of the rules of the policy.
	AdmissionPolicyActionWarn = "warn"

	// AdmissionOperationRegister and AdmissionOperationPlan are the job
	// operations the dynamic admission chain runs for.
	AdmissionOperationRegister = "register"
	AdmissionOperationPlan     = "plan"

	// maxAdmissionPolicyDescriptionLength is the maximum length allowed for
	// an admission policy description.
	maxAdmissionPolicyDescriptionLength = 256

	// maxAdmissionPolicyRules is the maximum number of rules of a policy.
	maxAdmissionPolicyRules = 64

	// maxAdmissionRuleExpressionLength is the maximum length of a single CEL
	// expression.
	maxAdmissionRuleExpressionLength = 8192
)

const (
	// TopicAdmissionPolicy is the event stream topic of admission policy
	// changes and of admission denials.
	TopicAdmissionPolicy Topic = "AdmissionPolicy"

	TypeAdmissionPolicyUpserted = "AdmissionPolicyUpserted"
	TypeAdmissionPolicyDeleted  = "AdmissionPolicyDeleted"
	TypeJobAdmissionDenied      = "JobAdmissionDenied"
)

var (
	// validAdmissionPolicyName is the rule used to validate a policy name.
	validAdmissionPolicyName = regexp.MustCompile("^[a-zA-Z0-9-_]{1,128}$")
)

// AdmissionPolicy is a cluster object holding CEL rules that every submitted
// job (register and plan) in a matching namespace is validated against.
type AdmissionPolicy struct {
	// Name is the unique name of the policy.
	Name string

	// Description is the human-friendly description of the policy.
	Description string

	// Action is what happens when a rule is violated: "deny" or "warn".
	Action string

	// Match restricts the jobs the policy applies to. A nil Match applies the
	// policy to every job.
	Match *AdmissionPolicyMatch

	// Rules are the CEL rules of the policy. A rule is violated when its
	// expression does not evaluate to true.
	Rules []*AdmissionPolicyRule

	// Raft indexes.
	CreateIndex uint64
	ModifyIndex uint64
}

// AdmissionPolicyMatch selects the jobs a policy applies to.
type AdmissionPolicyMatch struct {
	// Namespaces is the list of namespaces (glob patterns are allowed) the
	// policy applies to. Empty means every namespace.
	Namespaces []string
}

// AdmissionPolicyRule is a single CEL rule.
type AdmissionPolicyRule struct {
	// Expression is the CEL expression. It must evaluate to a bool and is
	// satisfied when it evaluates to true.
	Expression string

	// Message is returned to the submitter when the rule is violated.
	Message string
}

// GetID implements the IDGetter interface required for pagination.
func (p *AdmissionPolicy) GetID() string {
	return p.Name
}

// Canonicalize sets the defaults of the policy.
func (p *AdmissionPolicy) Canonicalize() {
	if p.Action == "" {
		p.Action = AdmissionPolicyActionDeny
	}
}

// Validate returns an error if the policy is invalid. The CEL expressions
// are compiled separately by the admission package.
func (p *AdmissionPolicy) Validate() error {
	var mErr *multierror.Error

	if !validAdmissionPolicyName.MatchString(p.Name) {
		mErr = multierror.Append(mErr, fmt.Errorf("invalid name %q, must match regex %s", p.Name, validAdmissionPolicyName))
	}
	if len(p.Description) > maxAdmissionPolicyDescriptionLength {
		mErr = multierror.Append(mErr, fmt.Errorf("description longer than %d", maxAdmissionPolicyDescriptionLength))
	}
	switch p.Action {
	case AdmissionPolicyActionDeny, AdmissionPolicyActionWarn:
	default:
		mErr = multierror.Append(mErr, fmt.Errorf("invalid action %q, must be %q or %q",
			p.Action, AdmissionPolicyActionDeny, AdmissionPolicyActionWarn))
	}
	if len(p.Rules) == 0 {
		mErr = multierror.Append(mErr, fmt.Errorf("at least one rule is required"))
	}
	if len(p.Rules) > maxAdmissionPolicyRules {
		mErr = multierror.Append(mErr, fmt.Errorf("at most %d rules are allowed", maxAdmissionPolicyRules))
	}
	for i, rule := range p.Rules {
		switch {
		case rule == nil || rule.Expression == "":
			mErr = multierror.Append(mErr, fmt.Errorf("rule %d: missing expression", i))
		case len(rule.Expression) > maxAdmissionRuleExpressionLength:
			mErr = multierror.Append(mErr, fmt.Errorf("rule %d: expression longer than %d", i, maxAdmissionRuleExpressionLength))
		}
	}
	if p.Match != nil {
		for _, ns := range p.Match.Namespaces {
			if ns == "" {
				mErr = multierror.Append(mErr, fmt.Errorf("match: empty namespace"))
			}
		}
	}

	return mErr.ErrorOrNil()
}

// Copy returns a deep copy of the policy.
func (p *AdmissionPolicy) Copy() *AdmissionPolicy {
	if p == nil {
		return nil
	}

	np := new(AdmissionPolicy)
	*np = *p
	if p.Match != nil {
		np.Match = &AdmissionPolicyMatch{Namespaces: slices.Clone(p.Match.Namespaces)}
	}
	if p.Rules != nil {
		np.Rules = make([]*AdmissionPolicyRule, len(p.Rules))
		for i, r := range p.Rules {
			if r != nil {
				rc := *r
				np.Rules[i] = &rc
			}
		}
	}
	return np
}

// MatchesNamespace returns true if the policy applies to jobs in the given
// namespace.
func (p *AdmissionPolicy) MatchesNamespace(namespace string) bool {
	if p.Match == nil {
		return true
	}
	return AdmissionNamespaceMatches(p.Match.Namespaces, namespace, true)
}

// AdmissionNamespaceMatches reports whether namespace matches one of the
// patterns (exact names or glob patterns). An empty pattern list returns
// emptyResult.
func AdmissionNamespaceMatches(patterns []string, namespace string, emptyResult bool) bool {
	if len(patterns) == 0 {
		return emptyResult
	}
	for _, pattern := range patterns {
		if pattern == namespace || glob.Glob(pattern, namespace) {
			return true
		}
	}
	return false
}

// AdmissionPolicyListRequest is used to list admission policies.
type AdmissionPolicyListRequest struct {
	QueryOptions
}

// AdmissionPolicyListResponse is the response to an admission policy list
// request.
type AdmissionPolicyListResponse struct {
	AdmissionPolicies []*AdmissionPolicy
	QueryMeta
}

// AdmissionPolicySpecificRequest is used to query a specific admission policy.
type AdmissionPolicySpecificRequest struct {
	Name string
	QueryOptions
}

// SingleAdmissionPolicyResponse is the response to a specific admission
// policy request.
type SingleAdmissionPolicyResponse struct {
	AdmissionPolicy *AdmissionPolicy
	QueryMeta
}

// AdmissionPolicyUpsertRequest is used to create or update admission
// policies.
type AdmissionPolicyUpsertRequest struct {
	AdmissionPolicies []*AdmissionPolicy
	WriteRequest
}

// AdmissionPolicyDeleteRequest is used to delete admission policies.
type AdmissionPolicyDeleteRequest struct {
	Names []string
	WriteRequest
}

// AdmissionPolicyEvent is the event stream payload of admission policy
// changes.
type AdmissionPolicyEvent struct {
	AdmissionPolicy *AdmissionPolicy
}

// AdmissionUser identifies the submitter of a job for admission controllers.
type AdmissionUser struct {
	AccessorID string
	Name       string
	Type       string
	Roles      []string
	Policies   []string
}

// NewAdmissionUser builds the admission view of the submitter from the
// authenticated identity or ACL token of a request. Either may be nil.
func NewAdmissionUser(token *ACLToken, claims *IdentityClaims) *AdmissionUser {
	switch {
	case token != nil:
		u := &AdmissionUser{
			AccessorID: token.AccessorID,
			Name:       token.Name,
			Type:       token.Type,
			Policies:   slices.Clone(token.Policies),
		}
		for _, role := range token.Roles {
			if role != nil {
				u.Roles = append(u.Roles, role.Name)
			}
		}
		return u
	case claims != nil:
		return &AdmissionUser{
			Name: fmt.Sprintf("workload:%s/%s/%s", claims.Namespace, claims.JobID, claims.TaskName),
			Type: "workload-identity",
		}
	default:
		return &AdmissionUser{Type: "anonymous"}
	}
}

// JobAdmissionDeniedEvent is the event stream payload emitted when the
// dynamic admission chain rejects a job.
type JobAdmissionDeniedEvent struct {
	Namespace string
	JobID     string
	Operation string
	User      *AdmissionUser

	// Controller is the kind of controller that rejected the job: "policy"
	// or "webhook".
	Controller string

	// Name is the name of the admission policy or webhook.
	Name string

	// Message is the rejection message.
	Message string
}
