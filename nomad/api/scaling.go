// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package api

import "time"

const (
	// ScalingPolicyTypeHorizontal indicates a policy that does horizontal scaling.
	ScalingPolicyTypeHorizontal = "horizontal"
)

// Scaling is used to query scaling-related API endpoints
type Scaling struct {
	client *Client
}

// Scaling returns a handle on the scaling endpoints.
func (c *Client) Scaling() *Scaling {
	return &Scaling{client: c}
}

func (s *Scaling) ListPolicies(q *QueryOptions) ([]*ScalingPolicyListStub, *QueryMeta, error) {
	var resp []*ScalingPolicyListStub
	qm, err := s.client.query("/v1/scaling/policies", &resp, q)
	if err != nil {
		return nil, nil, err
	}
	return resp, qm, nil
}

func (s *Scaling) GetPolicy(id string, q *QueryOptions) (*ScalingPolicy, *QueryMeta, error) {
	var policy ScalingPolicy
	qm, err := s.client.query("/v1/scaling/policy/"+id, &policy, q)
	if err != nil {
		return nil, nil, err
	}
	return &policy, qm, nil
}

func (p *ScalingPolicy) Canonicalize(taskGroupCount int) {
	if p.Enabled == nil {
		p.Enabled = pointerOf(true)
	}
	if p.Min == nil {
		var m int64 = int64(taskGroupCount)
		p.Min = &m
	}
	if p.Type == "" {
		p.Type = ScalingPolicyTypeHorizontal
	}
}

// ScalingRequest is the payload for a generic scaling action
type ScalingRequest struct {
	Count   *int64
	Target  map[string]string
	Message string
	Error   bool
	Meta    map[string]interface{}
	WriteRequest
	// PolicyOverride is ignored by the server: overriding admission policies
	// is not supported.
	PolicyOverride bool
}

// ScalingPolicy is the user-specified API object for an autoscaling policy
type ScalingPolicy struct {
	/* fields set by user in HCL config */

	Min     *int64                 `hcl:"min,optional"`
	Max     *int64                 `hcl:"max,optional"`
	Policy  map[string]interface{} `hcl:"policy,block"`
	Enabled *bool                  `hcl:"enabled,optional"`
	Type    string                 `hcl:"type,optional"`

	// Engine is the autoscaler responsible for the policy: "builtin" or
	// "external". Empty means the default engine of the servers.
	Engine string `hcl:"engine,optional"`

	// The typed policy of the built-in autoscaler. When none of these
	// fields is set, the servers fill them from the policy block. In
	// jobspecs they are written inside the policy block.
	EvaluationInterval    time.Duration   `json:",omitempty"`
	Cooldown              time.Duration   `json:",omitempty"`
	CooldownOnScaleUp     time.Duration   `json:",omitempty"`
	OnCheckError          string          `json:",omitempty"`
	ScaleDuringDeployment string          `json:",omitempty"`
	Checks                []*ScalingCheck `json:",omitempty"`

	/* fields set by server */

	ID          string
	Namespace   string
	Target      map[string]string
	CreateIndex uint64
	ModifyIndex uint64
}

// ScalingPolicyListStub is used to return a subset of scaling policy information
// for the scaling policy list
type ScalingPolicyListStub struct {
	ID          string
	Enabled     bool
	Type        string
	Engine      string
	Target      map[string]string
	CreateIndex uint64
	ModifyIndex uint64
}

// JobScaleStatusResponse is used to return information about job scaling status
type JobScaleStatusResponse struct {
	JobID          string
	Namespace      string
	JobCreateIndex uint64
	JobModifyIndex uint64
	JobStopped     bool
	TaskGroups     map[string]TaskGroupScaleStatus
}

type TaskGroupScaleStatus struct {
	Desired   int
	Placed    int
	Running   int
	Healthy   int
	Unhealthy int
	Events    []ScalingEvent
}

type ScalingEvent struct {
	Count         *int64
	PreviousCount int64
	Error         bool
	Message       string
	Meta          map[string]interface{}
	EvalID        *string
	Time          uint64
	CreateIndex   uint64
}
