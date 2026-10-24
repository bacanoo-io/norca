// Copyright IBM Corp. 2020, 2026
// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Rewritten from Nomad Autoscaler v0.6.0 policy/manager.go and
// policy/nomad/source.go. Instead of blocking queries on the HTTP API, the
// policy manager watches the scaling_policy table of the leader's state store
// and only picks up the policies of the builtin engine.

package autoscaler

import (
	"context"
	"errors"
	"fmt"
	"time"

	metrics "github.com/armon/go-metrics"
	memdb "github.com/hashicorp/go-memdb"
	multierror "github.com/hashicorp/go-multierror"
	"github.com/hashicorp/nomad/nomad/autoscaler/sdk"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
)

// policyRetryInterval is the wait after a failed state store query.
const policyRetryInterval = 5 * time.Second

// policyList is the set of builtin policies at a state store index.
type policyList struct {
	index    uint64
	policies []*structs.ScalingPolicy
}

// watchPolicies sends the builtin policies to ch every time the
// scaling_policy table changes. It blocks until ctx is canceled.
func (r *runner) watchPolicies(ctx context.Context, ch chan<- *policyList) {
	var lastIndex uint64

	for {
		// Get the state store on every iteration: it is replaced when a
		// snapshot is restored, and the old one is abandoned.
		store := r.deps.State()
		ws := memdb.NewWatchSet()
		ws.Add(store.AbandonCh())

		list, err := r.builtinPolicies(ws, store)
		if err != nil {
			r.logger.Error("failed to list scaling policies", "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(policyRetryInterval):
				continue
			}
		}

		if list.index != lastIndex {
			lastIndex = list.index
			select {
			case <-ctx.Done():
				return
			case ch <- list:
			}
		}

		if err := ws.WatchCtx(ctx); err != nil {
			return
		}
	}
}

// builtinPolicies returns the enabled task group policies of the builtin
// engine.
func (r *runner) builtinPolicies(ws memdb.WatchSet, store *state.StateStore) (*policyList, error) {
	iter, err := store.ScalingPolicies(ws)
	if err != nil {
		return nil, err
	}

	list := &policyList{}
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		p := raw.(*structs.ScalingPolicy)
		if r.isBuiltinPolicy(p) {
			list.policies = append(list.policies, p)
		}
	}

	list.index, err = store.Index("scaling_policy")
	if err != nil {
		return nil, err
	}
	return list, nil
}

// isBuiltinPolicy reports whether the built-in autoscaler is responsible for
// the policy.
func (r *runner) isBuiltinPolicy(p *structs.ScalingPolicy) bool {
	if p == nil || !p.Enabled {
		return false
	}
	if p.Type != "" && p.Type != structs.ScalingPolicyTypeHorizontal {
		return false
	}
	if PolicyEngine(p, r.cfg.DefaultEngine) != structs.ScalingPolicyEngineBuiltin {
		return false
	}
	_, ok := targetKeyFromPolicy(p)
	return ok
}

// policyStillActive re-reads the policy from the state store right before a
// scaling action.
func (r *runner) policyStillActive(c *compiledPolicy) bool {
	p, err := r.deps.State().ScalingPolicyByID(nil, c.policy.ID)
	if err != nil {
		return false
	}
	return r.isBuiltinPolicy(p)
}

// reconcile updates the tracked policies from the latest list.
func (r *runner) reconcile(list *policyList, now time.Time) {
	seen := make(map[string]struct{}, len(list.policies))

	for _, p := range list.policies {
		seen[p.ID] = struct{}{}

		h := r.handles[p.ID]
		if h != nil && h.modifyIndex == p.ModifyIndex {
			continue
		}

		c, err := r.compile(p)
		if err != nil {
			metrics.IncrCounter([]string{"nomad", "autoscaler", "policy", "invalid"}, 1)
			if h != nil && h.compiled != nil {
				r.logger.Warn("latest version for policy is invalid, old one will keep running",
					"policy_id", p.ID, "validation_error", err)
			} else {
				r.logger.Warn("policy is invalid and will not be evaluated",
					"policy_id", p.ID, "validation_error", err)
			}
			if h == nil {
				h = &policyHandle{}
				r.handles[p.ID] = h
			}
			h.modifyIndex = p.ModifyIndex
			continue
		}

		interval := c.policy.EvaluationInterval
		switch {
		case h == nil:
			r.logger.Debug("tracking new policy", "policy_id", p.ID, "target", c.key.String())
			h = &policyHandle{nextEval: now.Add(splay(interval))}
			r.handles[p.ID] = h
		case h.compiled == nil:
			h.nextEval = now.Add(splay(interval))
		default:
			// Do not wait longer than the new interval.
			if latest := now.Add(interval); h.nextEval.After(latest) {
				h.nextEval = latest
			}
		}
		h.compiled = c
		h.modifyIndex = p.ModifyIndex
	}

	for id, h := range r.handles {
		if _, ok := seen[id]; ok {
			continue
		}
		r.logger.Debug("policy removed", "policy_id", id)
		if h.compiled != nil {
			r.exec.forget(h.compiled.key)
		}
		delete(r.handles, id)
		if h.queued {
			r.removeFromQueue(id)
		}
	}

	metrics.SetGauge([]string{"nomad", "autoscaler", "policies"}, float32(len(r.handles)))
}

func (r *runner) removeFromQueue(id string) {
	for i, qid := range r.queue {
		if qid == id {
			r.queue = append(r.queue[:i], r.queue[i+1:]...)
			return
		}
	}
}

// compile parses, validates and canonicalizes a policy, and loads the
// plugins of its checks.
func (r *runner) compile(p *structs.ScalingPolicy) (*compiledPolicy, error) {
	key, ok := targetKeyFromPolicy(p)
	if !ok {
		return nil, errors.New("policy target is not a task group")
	}

	// Policies are validated at submission; this only protects against
	// policies stored before (such as by older servers) or invalidated by a
	// configuration change. The interval and cooldown lower bounds are not
	// enforced here: the interval is raised to the minimum instead.
	if err := validatePolicy(p, r.cfg, false); err != nil {
		return nil, err
	}
	tp, _ := TypedPolicy(p)

	sp := parsePolicy(p)
	applyTypedPolicy(&sp, tp, r.cfg)
	r.proc.canonicalizePolicy(&sp)

	var mErr *multierror.Error
	if err := sp.Validate(); err != nil {
		mErr = multierror.Append(mErr, err)
	}
	if err := r.proc.validatePolicy(&sp); err != nil {
		mErr = multierror.Append(mErr, err)
	}
	if err := sdk.FormattedMultiError(mErr); err != nil {
		return nil, err
	}

	runners, err := r.loadCheckRunners(&sp)
	if err != nil {
		return nil, err
	}

	schedule, err := compileSchedule(sp.Schedule)
	if err != nil {
		return nil, fmt.Errorf("invalid schedule: %w", err)
	}

	return &compiledPolicy{
		policy:       &sp,
		modifyIndex:  p.ModifyIndex,
		key:          key,
		checkRunners: runners,
		schedule:     schedule,

		scaleDuringDeployment: tp.ScaleDuringDeployment,
	}, nil
}
