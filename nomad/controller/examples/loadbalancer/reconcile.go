// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/controller"
)

// reconciler maintains LoadBalancer objects.
type reconciler struct {
	ctrl      *controller.Controller
	cluster   Cluster
	providers map[string]Provider
	logger    *slog.Logger
	now       func() time.Time

	// requeueNotReady and requeueReady bound how long a health change of a
	// target that produces no event (for example a deployment marking an
	// allocation unhealthy) takes to reach the load balancer.
	requeueNotReady time.Duration
	requeueReady    time.Duration
}

func (r *reconciler) Reconcile(ctx context.Context, key controller.ObjectKey) (time.Duration, error) {
	obj, ok := r.ctrl.Get(key)
	if !ok {
		// The object was deleted: remove what every provider created for
		// it. (With native objects a finalizer will guarantee this even if
		// the controller is down when the object is deleted; until then
		// the owner references in job meta let it clean up afterwards.)
		var errs []error
		for _, p := range r.providers {
			errs = append(errs, p.Delete(ctx, key))
		}
		return 0, errors.Join(errs...)
	}

	var status Status
	_ = obj.DecodeStatus(&status)
	status.Observe(obj)

	var spec Spec
	if err := obj.DecodeSpec(&spec); err != nil {
		return 0, r.fail(ctx, obj, &status, "InvalidSpec", err.Error())
	}
	spec.Defaults()
	if err := spec.Validate(); err != nil {
		return 0, r.fail(ctx, obj, &status, "InvalidSpec", err.Error())
	}
	provider, ok := r.providers[spec.Class]
	if !ok {
		return 0, r.fail(ctx, obj, &status, "UnknownClass",
			fmt.Sprintf("no provider for class %q in this region", spec.Class))
	}

	// A changed class leaves resources of the previous provider behind.
	for class, p := range r.providers {
		if class != spec.Class {
			if err := p.Delete(ctx, key); err != nil {
				return 0, err
			}
		}
	}

	targets, err := r.cluster.ServiceTargets(ctx, key.Namespace, spec.Service)
	if err != nil {
		return 0, fmt.Errorf("reading the instances of service %q: %w", spec.Service, err)
	}
	res, err := provider.Ensure(ctx, obj, spec, targets)
	if err != nil {
		return 0, err
	}

	status.Class = spec.Class
	status.Resource = res.Resource
	status.Addresses = res.Addresses
	status.Targets = make([]string, len(targets))
	for i, t := range targets {
		status.Targets[i] = t.String()
	}
	now := r.now()
	ready := res.Ready && len(targets) > 0
	switch {
	case ready:
		status.SetCondition(cond(controller.ConditionReady, true, "Ready",
			fmt.Sprintf("forwarding to %d targets", len(targets))), now)
	case len(targets) == 0:
		status.SetCondition(cond(controller.ConditionReady, false, "NoHealthyTargets",
			fmt.Sprintf("service %q has no healthy instances", spec.Service)), now)
	default:
		status.SetCondition(cond(controller.ConditionReady, false, "ProviderNotReady", res.Message), now)
	}
	if res.Ready {
		status.SetCondition(cond(controller.ConditionProgressing, false, "Provisioned", ""), now)
	} else {
		status.SetCondition(cond(controller.ConditionProgressing, true, "Provisioning", res.Message), now)
	}
	if len(targets) == 0 {
		status.SetCondition(cond(controller.ConditionDegraded, true, "NoHealthyTargets", ""), now)
	} else {
		status.SetCondition(cond(controller.ConditionDegraded, false, "TargetsAvailable", ""), now)
	}

	if _, err := r.ctrl.Objects().UpdateStatus(ctx, obj, status); err != nil {
		if errors.Is(err, controller.ErrNotFound) {
			return 0, nil
		}
		return 0, err
	}
	if ready {
		return r.requeueReady, nil
	}
	return r.requeueNotReady, nil
}

func cond(conditionType string, ok bool, reason, message string) controller.Condition {
	status := controller.ConditionFalse
	if ok {
		status = controller.ConditionTrue
	}
	return controller.Condition{Type: conditionType, Status: status, Reason: reason, Message: message}
}

// fail records a problem of the spec in the status. Such problems are not
// retried: the next spec change reconciles again.
func (r *reconciler) fail(ctx context.Context, obj *controller.Object, status *Status, reason, message string) error {
	now := r.now()
	status.SetCondition(controller.Condition{Type: controller.ConditionReady, Status: controller.ConditionFalse, Reason: reason, Message: message}, now)
	status.SetCondition(controller.Condition{Type: controller.ConditionDegraded, Status: controller.ConditionTrue, Reason: reason, Message: message}, now)
	_, err := r.ctrl.Objects().UpdateStatus(ctx, obj, status)
	if errors.Is(err, controller.ErrNotFound) {
		return nil
	}
	return err
}

// serviceSource reconciles the load balancers of a service when its
// registrations change.
func serviceSource() controller.Source {
	return &controller.EventSource{
		SourceName: "services",
		Topics:     map[api.Topic][]string{api.TopicService: {"*"}},
		Map: func(ev *api.Event, sc *controller.SourceContext) []controller.ObjectKey {
			reg, err := ev.Service()
			if err != nil || reg == nil {
				return nil
			}
			var keys []controller.ObjectKey
			for _, obj := range sc.Objects() {
				var spec Spec
				if obj.Key.Namespace == reg.Namespace && obj.DecodeSpec(&spec) == nil && spec.Service == reg.ServiceName {
					keys = append(keys, obj.Key)
				}
			}
			return slices.Clip(keys)
		},
	}
}
