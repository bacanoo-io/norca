// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/controller"
)

// Finalizer is the finalizer of native LoadBalancer objects: deleting an
// object only marks it until the controller has removed its load balancer.
const Finalizer = "norca.io/loadbalancer-cleanup"

// reconciler maintains LoadBalancer objects.
type reconciler struct {
	ctrl      *controller.Controller
	cluster   Cluster
	providers map[string]Provider

	// classes holds the providers registered by LoadBalancerClass objects
	// (with native objects only); nil disables them.
	classes *classRegistry
	logger  *slog.Logger
	now     func() time.Time

	// requeueNotReady and requeueReady bound how long a health change of a
	// target that produces no event (for example a deployment marking an
	// allocation unhealthy) takes to reach the load balancer.
	requeueNotReady time.Duration
	requeueReady    time.Duration
}

func (r *reconciler) Reconcile(ctx context.Context, key controller.ObjectKey) (time.Duration, error) {
	obj, ok := r.ctrl.Get(key)
	if !ok {
		// The object is gone: remove what every provider created for it.
		// With native objects the finalizer normally did this before the
		// object disappeared; with Variables this is the only cleanup.
		r.ctrl.Recorder().Forget(key)
		return 0, r.cleanup(ctx, key, "", false)
	}

	native := r.ctrl.Objects().Storage() == controller.StorageNative
	if native && obj.Deleting() {
		// The finalizer keeps the object until the load balancer is gone,
		// even if the controller was down when it was deleted.
		if !obj.HasFinalizer(Finalizer) {
			return 0, nil
		}
		var last Status
		_ = obj.DecodeStatus(&last)
		if err := r.cleanup(ctx, key, last.Class, true); err != nil {
			r.ctrl.Recorder().Event(ctx, key, controller.EventTypeWarning, "CleanupFailed", err.Error())
			return 0, err
		}
		_, err := r.ctrl.Objects().RemoveFinalizer(ctx, obj, Finalizer)
		return 0, err
	}
	if native && !obj.HasFinalizer(Finalizer) {
		next, err := r.ctrl.Objects().AddFinalizer(ctx, obj, Finalizer)
		if err != nil {
			if errors.Is(err, controller.ErrConflict) || errors.Is(err, controller.ErrNotFound) {
				// Deleted meanwhile: the deletion is reconciled next.
				return time.Second, nil
			}
			return 0, err
		}
		obj = next
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
	provider, problem, err := r.resolve(ctx, native, spec)
	if err != nil {
		return 0, err
	}
	if provider == nil {
		// With registered classes a provider may appear, come back or
		// change its capabilities, so the problem is checked again.
		return r.failRetry(ctx, obj, &status, problem.reason, problem.message, problem.retry)
	}

	// A changed class leaves resources of the previous provider behind.
	for class, p := range r.providers {
		if class != spec.Class {
			if err := p.Delete(ctx, key); err != nil {
				return 0, err
			}
		}
	}
	if status.Class != "" && status.Class != spec.Class {
		if err := r.deleteRemote(ctx, key, status.Class, false); err != nil {
			return 0, err
		}
	}

	targets, err := r.cluster.ServiceTargets(ctx, key.Namespace, spec.Service)
	if err != nil {
		return 0, fmt.Errorf("reading the instances of service %q: %w", spec.Service, err)
	}
	res, err := provider.Ensure(ctx, obj, spec, targets)
	if err != nil {
		r.ctrl.Recorder().Event(ctx, key, controller.EventTypeWarning, "ProvisioningFailed", err.Error())
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
		r.ctrl.Recorder().Event(ctx, key, controller.EventTypeNormal, "Ready",
			fmt.Sprintf("forwarding %s to %d targets", strings.Join(res.Addresses, ","), len(targets)))
	} else if c := status.Condition(controller.ConditionReady); c != nil {
		r.ctrl.Recorder().Event(ctx, key, controller.EventTypeNormal, c.Reason, c.Message)
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

// cleanup removes what every built-in provider created for the object, and
// what the registered provider of class created. With strict, a registered
// provider that cannot be reached (or is gone) is an error, so that the
// finalizer keeps the object until the load balancer is removed.
func (r *reconciler) cleanup(ctx context.Context, key controller.ObjectKey, class string, strict bool) error {
	var errs []error
	for _, p := range r.providers {
		errs = append(errs, p.Delete(ctx, key))
	}
	errs = append(errs, r.deleteRemote(ctx, key, class, strict))
	return errors.Join(errs...)
}

// deleteRemote removes the load balancer a registered provider created. A
// class that is no longer registered is an error only when strict is set;
// otherwise what it created is left behind.
func (r *reconciler) deleteRemote(ctx context.Context, key controller.ObjectKey, class string, strict bool) error {
	if class == "" || r.classes == nil {
		return nil
	}
	if _, builtin := r.providers[class]; builtin {
		return nil
	}
	info, err := r.classes.lookup(ctx, class)
	if err != nil {
		return fmt.Errorf("reading the LoadBalancerClass objects: %w", err)
	}
	if info == nil {
		if strict {
			return fmt.Errorf("cannot remove the load balancer of class %q: %w", class, errClassNotRegistered)
		}
		r.logger.Warn("the class of a load balancer is not registered, what it created is left behind",
			"object", key.String(), "class", class)
		return nil
	}
	return r.classes.provider(info).Delete(ctx, key)
}

// classProblem describes why a class cannot implement a load balancer now.
type classProblem struct {
	reason  string
	message string
	retry   bool
}

// resolve returns the provider of the class of the spec: a built-in one, or
// one registered by a LoadBalancerClass object. It returns a nil provider and
// the problem when there is none that can implement the spec.
func (r *reconciler) resolve(ctx context.Context, native bool, spec Spec) (Provider, classProblem, error) {
	if p, ok := r.providers[spec.Class]; ok {
		return p, classProblem{}, nil
	}
	known := func() string {
		names := make([]string, 0, len(r.providers))
		for class := range r.providers {
			names = append(names, class)
		}
		if r.classes != nil && native {
			names = append(names, r.classes.names(ctx)...)
		}
		slices.Sort(names)
		return strings.Join(slices.Compact(names), ", ")
	}
	if r.classes == nil || !native {
		return nil, classProblem{reason: "UnknownClass",
			message: fmt.Sprintf("no provider for class %q in this region (known: %s)", spec.Class, known())}, nil
	}

	info, err := r.classes.lookup(ctx, spec.Class)
	if err != nil {
		return nil, classProblem{}, fmt.Errorf("reading the LoadBalancerClass objects: %w", err)
	}
	if info == nil {
		return nil, classProblem{reason: "UnknownClass", retry: true,
			message: fmt.Sprintf("no provider for class %q in this region (known: %s)", spec.Class, known())}, nil
	}
	if err := info.validate(r.classes.allowHTTP); err != nil {
		return nil, classProblem{reason: "InvalidClass", retry: true, message: err.Error()}, nil
	}
	if ok, why := info.available(r.now(), r.classes.ttl); !ok {
		return nil, classProblem{reason: "ProviderUnavailable", retry: true,
			message: fmt.Sprintf("provider of class %q is unavailable: %s", spec.Class, why)}, nil
	}
	if why := info.unsupported(spec); why != "" {
		return nil, classProblem{reason: "Unsupported", retry: true, message: why}, nil
	}
	return r.classes.provider(info), classProblem{}, nil
}

// failRetry is fail for a problem that may go away without a spec change: it
// asks for another reconcile when retry is set.
func (r *reconciler) failRetry(ctx context.Context, obj *controller.Object, status *Status, reason, message string, retry bool) (time.Duration, error) {
	if err := r.fail(ctx, obj, status, reason, message); err != nil {
		return 0, err
	}
	if retry {
		return r.requeueNotReady, nil
	}
	return 0, nil
}

// fail records a problem of the spec in the status and the event log. Such
// problems are not retried: the next spec change reconciles again.
func (r *reconciler) fail(ctx context.Context, obj *controller.Object, status *Status, reason, message string) error {
	r.ctrl.Recorder().Event(ctx, obj.Key, controller.EventTypeWarning, reason, message)
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
