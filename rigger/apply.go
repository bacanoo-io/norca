// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rig

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"

	"github.com/hashicorp/hcl/v2"

	"github.com/bacanoo-io/norca-rig/host"
	"github.com/bacanoo-io/norca-rig/internal/eval"
	"github.com/bacanoo-io/norca-rig/oplog"
)

// ApplyOptions are the options of an apply.
type ApplyOptions struct {
	// Prune uninstalls the releases the stack owns and no longer has
	// (design 9.3); without it they are only shown.
	Prune bool
}

// ReleaseOutcome is what an apply did to a release.
type ReleaseOutcome struct {
	Release  host.ReleaseRef `json:"release"`
	Action   ReleaseAction   `json:"action"`
	Revision uint64          `json:"revision,omitempty"`
	// Skipped is set for a remove without prune and for the releases after
	// a failure.
	Skipped bool `json:"skipped,omitempty"`
}

// ErrGeneratedMeanwhile is the error of an apply whose generated values
// were written by another run after the plan.
var ErrGeneratedMeanwhile = errors.New("the generated values of the stack were written by another run since the plan; plan again")

// ApplyStack applies a plan (design 9.2): the generated values and the
// secrets first, created once, then the releases in the order of their
// dependencies, each waiting until it is ready, and with prune the removals.
// The first failure stops the rest; a second apply goes on where it
// stopped, because its plan skips what is done.
func (e *Engine) ApplyStack(ctx context.Context, plan *StackPlan, opts ApplyOptions) ([]ReleaseOutcome, hcl.Diagnostics) {
	ctx, span := oplog.Start(ctx, e.logger, slog.LevelInfo, "stack.apply", "stack", plan.Stack, "region", plan.Region)

	out, err := e.applyStack(ctx, plan, opts)
	span.End(ctx, err)

	if err != nil {
		return out, errorDiags("Apply stopped", err)
	}

	return out, nil
}

func (e *Engine) applyStack(ctx context.Context, plan *StackPlan, opts ApplyOptions) ([]ReleaseOutcome, error) {
	if e.releases == nil || e.secrets == nil {
		return nil, ErrNoCluster
	}

	if err := e.applyGenerated(ctx, plan); err != nil {
		return nil, err
	}

	if err := e.applySecrets(ctx, plan); err != nil {
		return nil, err
	}

	outcomes := make([]ReleaseOutcome, 0, len(plan.Releases))

	for i, rp := range plan.Releases {
		outcome, err := e.applyRelease(ctx, rp, opts)
		outcomes = append(outcomes, outcome)

		if err != nil {
			for _, rest := range plan.Releases[i+1:] {
				outcomes = append(outcomes, ReleaseOutcome{Release: rest.Release, Action: rest.Action, Skipped: true})
			}

			return outcomes, fmt.Errorf("release %s/%s: %w", rp.Release.Namespace, rp.Release.Name, err)
		}
	}

	return outcomes, nil
}

// applyRelease applies the plan of one release.
func (e *Engine) applyRelease(ctx context.Context, rp ReleasePlan, opts ApplyOptions) (ReleaseOutcome, error) {
	outcome := ReleaseOutcome{Release: rp.Release, Action: rp.Action, Revision: rp.Revision}

	switch rp.Action {
	case ActionInstall, ActionUpgrade:
		ctx, span := oplog.Start(ctx, e.logger, slog.LevelInfo, "release.apply",
			"release", rp.Release.Name, "namespace", rp.Release.Namespace, "action", string(rp.Action))

		rev, err := e.releases.Apply(ctx, rp.request)
		span.End(ctx, err)

		if err != nil {
			return outcome, err
		}

		outcome.Revision = rev.Number
	case ActionRemove:
		if !opts.Prune {
			outcome.Skipped = true

			return outcome, nil
		}

		ctx, span := oplog.Start(ctx, e.logger, slog.LevelInfo, "release.prune", "release", rp.Release.Name, "namespace", rp.Release.Namespace)
		err := e.releases.Uninstall(ctx, rp.Release)
		span.End(ctx, err)

		if err != nil {
			return outcome, err
		}
	case ActionUnchanged, ActionDisabled:
	}

	return outcome, nil
}

// applyGenerated writes the values the plan generated into the secret of
// the generated values, keeping those already there.
func (e *Engine) applyGenerated(ctx context.Context, plan *StackPlan) error {
	if len(plan.Generate) == 0 {
		return nil
	}

	existing, err := e.readGenerated(ctx, plan)
	if err != nil {
		return err
	}

	items := map[string]host.Sensitive{}
	for k, v := range existing {
		items[k] = host.NewSensitive(v)
	}

	for _, name := range plan.Generate {
		if _, ok := existing[name]; ok {
			return ErrGeneratedMeanwhile
		}

		items[name] = host.NewSensitive(plan.generated[name])
	}

	_, err = e.secrets.Ensure(ctx, host.Secret{Path: generatedPath(plan.Stack), Items: items, Overwrite: len(existing) > 0})

	return err
}

// applySecrets creates the secrets of the stack, or overwrites those that
// say so.
func (e *Engine) applySecrets(ctx context.Context, plan *StackPlan) error {
	for _, s := range plan.secrets {
		items := make(map[string]host.Sensitive, len(s.Items))
		for _, k := range slices.Sorted(maps.Keys(s.Items)) {
			items[k] = host.NewSensitive(s.Items[k])
		}

		if _, err := e.secrets.Ensure(ctx, host.Secret{Path: s.Path, Items: items, Overwrite: s.Overwrite}); err != nil {
			return fmt.Errorf("secret %s: %w", s.Path, err)
		}
	}

	return nil
}

// ReleaseStatus is the state of a release a stack owns.
type ReleaseStatus struct {
	Revision host.Revision     `json:"revision"`
	Drift    []host.DriftEntry `json:"drift,omitempty"`
}

// StackStatus returns the releases a stack owns, their current revision
// and their drift (design 9.3).
func (e *Engine) StackStatus(ctx context.Context, s *Stack) ([]ReleaseStatus, error) {
	ctx, span := oplog.Start(ctx, e.logger, slog.LevelInfo, "stack.status", "stack", s.Name())

	out, err := e.stackStatus(ctx, s.Name())
	span.End(ctx, err)

	return out, err
}

func (e *Engine) stackStatus(ctx context.Context, stack string) ([]ReleaseStatus, error) {
	if e.releases == nil {
		return nil, ErrNoCluster
	}

	owned, err := e.releases.Owned(ctx, stack)
	if err != nil {
		return nil, err
	}

	out := make([]ReleaseStatus, 0, len(owned))

	for _, rev := range owned {
		current, err := e.releases.Current(ctx, rev.Release)
		if err != nil {
			return nil, err
		}

		drift, err := e.releases.Drift(ctx, rev.Release)
		if err != nil {
			return nil, err
		}

		out = append(out, ReleaseStatus{Revision: current, Drift: drift.Entries})
	}

	return out, nil
}

// DestroyStack uninstalls the releases a stack owns, the releases that
// others depend on last. It keeps the secrets and the generated values: a
// later apply finds them again.
func (e *Engine) DestroyStack(ctx context.Context, s *Stack) ([]host.ReleaseRef, error) {
	ctx, span := oplog.Start(ctx, e.logger, slog.LevelInfo, "stack.destroy", "stack", s.Name())

	out, err := e.destroyStack(ctx, s)
	span.End(ctx, err)

	return out, err
}

func (e *Engine) destroyStack(ctx context.Context, s *Stack) ([]host.ReleaseRef, error) {
	if e.releases == nil {
		return nil, ErrNoCluster
	}

	owned, err := e.releases.Owned(ctx, s.Name())
	if err != nil {
		return nil, err
	}

	// The order of the stack, reversed: a release is removed before those
	// it reads. Releases no longer in the stack go first.
	rank := map[string]int{}
	for i, r := range s.s.Releases {
		rank[r.Name] = i + 1
	}

	slices.SortStableFunc(owned, func(a, b host.Revision) int {
		return rank[b.Release.Name] - rank[a.Release.Name]
	})

	var removed []host.ReleaseRef

	for _, rev := range owned {
		ctx, span := oplog.Start(ctx, e.logger, slog.LevelInfo, "release.uninstall", "release", rev.Release.Name, "namespace", rev.Release.Namespace)
		err := e.releases.Uninstall(ctx, rev.Release)
		span.End(ctx, err)

		if err != nil {
			return removed, fmt.Errorf("release %s/%s: %w", rev.Release.Namespace, rev.Release.Name, err)
		}

		removed = append(removed, rev.Release)
	}

	return removed, nil
}

// PlanInstall plans one release of a module on its own (rig install): a
// stack of one, owned by no stack.
func (e *Engine) PlanInstall(ctx context.Context, m *Module, in Install, set host.Settings) (*ReleasePlan, hcl.Diagnostics) {
	ctx, span := oplog.Start(ctx, e.logger, slog.LevelInfo, "release.plan", "release", in.Release, "namespace", in.Namespace)

	rp, diags := e.planInstall(ctx, m, in, set)
	span.End(ctx, diagsErr(diags))

	return rp, diags
}

func (e *Engine) planInstall(ctx context.Context, m *Module, in Install, set host.Settings) (*ReleasePlan, hcl.Diagnostics) {
	if e.releases == nil {
		return nil, errorDiags("No cluster", ErrNoCluster)
	}

	if diags := checkInstall(in); diags.HasErrors() {
		return nil, diags
	}

	vars, diags := e.variables(ctx, m, in.Values)
	if diags.HasErrors() {
		return nil, diags
	}

	md := m.mod.Metadata
	facts := eval.Facts{Release: in.Release, Namespace: in.Namespace, Region: in.Region, ModuleName: md.Name, ModuleVersion: md.Version}

	res, moreDiags := eval.Evaluate(ctx, m.mod, vars, facts, e.evalConfig())
	diags = append(diags, moreDiags...)

	if moreDiags.HasErrors() {
		return nil, diags
	}

	ref := host.ReleaseRef{Namespace: in.Namespace, Name: in.Release}

	rp, err := e.planRelease(ctx, ref, "", in.Region, m.mod, eval.MarkSensitive(m.mod, valuesObject(in.Values)), res, set)
	if err != nil {
		return nil, append(diags, errorDiags("Release not compared", err)...)
	}

	return &rp, diags
}

// ApplyInstall applies the plan of one release.
func (e *Engine) ApplyInstall(ctx context.Context, rp *ReleasePlan) (ReleaseOutcome, error) {
	if e.releases == nil {
		return ReleaseOutcome{}, ErrNoCluster
	}

	return e.applyRelease(ctx, *rp, ApplyOptions{})
}
