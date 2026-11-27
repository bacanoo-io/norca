// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"

	"github.com/hashicorp/hcl/v2"

	"github.com/bacanoo-io/norca/rigger/host"
	"github.com/bacanoo-io/norca/rigger/internal/eval"
	"github.com/bacanoo-io/norca/rigger/oplog"
	"github.com/bacanoo-io/norca/rigger/sign"
)

// ApplyOptions are the options of an apply.
type ApplyOptions struct {
	// Prune uninstalls the releases the stack owns and no longer has
	// (design 9.3); without it they are only shown.
	Prune bool
	// Adopt takes over the releases the plan shows as taken over
	// (ReleasePlan.TakesOver): those another package manager applied or
	// another stack owns (host.ApplyRequest.Adopt). A release the plan does
	// not show so is applied without it.
	Adopt bool
}

// ReleaseOutcome is what an apply did to a release.
type ReleaseOutcome struct {
	Release  host.ReleaseRef `json:"release"`
	Action   ReleaseAction   `json:"action"`
	Revision uint64          `json:"revision,omitempty"`
	// Skipped is set for a remove without prune and for the releases after
	// a failure.
	Skipped bool `json:"skipped,omitempty"`
	// Failed is set for the release whose apply failed: the apply stopped
	// there, and the error of the apply says why.
	Failed bool `json:"failed,omitempty"`
}

// ErrGeneratedMeanwhile is the error of an apply whose generated values
// were written by another run after the plan.
var ErrGeneratedMeanwhile = errors.New("the generated values of the stack were written by another run since the plan; plan again")

// ErrAdoptNeeded is the error of an apply that would take over a release
// and was not asked to.
var ErrAdoptNeeded = errors.New("it is taken over only when asked to (adopt; the flag --adopt)")

// CheckAdopt returns the error of an apply of the plan without Adopt
// (ApplyOptions): ErrAdoptNeeded with the release when it takes one over.
func (p *StackPlan) CheckAdopt(adopt bool) error {
	for i := range p.Releases {
		if err := p.Releases[i].checkAdopt(adopt); err != nil {
			return err
		}
	}

	return nil
}

func (rp *ReleasePlan) checkAdopt(adopt bool) error {
	if adopt || rp.TakesOver == "" {
		return nil
	}

	return fmt.Errorf("release %s/%s belongs to %s: %w", rp.Release.Namespace, rp.Release.Name, rp.TakesOver, ErrAdoptNeeded)
}

// ApplyStack applies a plan (design 9.2): the generated values and the
// secrets first, created once, then the releases in the order of their
// dependencies, each waiting until it is ready, and with prune the removals.
// The first failure stops the rest; a second apply goes on where it
// stopped, because its plan skips what is done.
func (e *Engine) ApplyStack(ctx context.Context, plan *StackPlan, opts ApplyOptions) (_ []ReleaseOutcome, retDiags hcl.Diagnostics) {
	defer e.recoverDiags(ctx, "stack.apply", &retDiags)

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

	// Before anything is written.
	if err := plan.CheckAdopt(opts.Adopt); err != nil {
		return nil, err
	}

	if err := e.applyGenerated(ctx, plan); err != nil {
		return nil, err
	}

	if err := e.applySecrets(ctx, plan); err != nil {
		return nil, err
	}

	outcomes := make([]ReleaseOutcome, 0, len(plan.Releases))

	for i, rp := range plan.Releases {
		// A cancel stops the apply between two releases: the release being
		// applied finishes, the rest are skipped.
		if err := ctx.Err(); err != nil {
			for _, rest := range plan.Releases[i:] {
				outcomes = append(outcomes, ReleaseOutcome{Release: rest.Release, Action: rest.Action, Skipped: true})
			}

			return outcomes, fmt.Errorf("stopped before release %s/%s: %w", rp.Release.Namespace, rp.Release.Name, err)
		}

		outcome, err := e.applyRelease(ctx, rp, opts)
		outcome.Failed = err != nil
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

		// Adopt is for the releases the plan showed as taken over. One that
		// became another's since the plan is refused by the server, as
		// without adopt: it is not taken over unseen.
		req := rp.request
		req.Adopt = opts.Adopt && rp.TakesOver != ""

		rev, err := e.releases.Apply(ctx, req)
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

	items, read := map[string]host.Sensitive{}, map[string]host.Sensitive{}
	for k, v := range existing {
		items[k], read[k] = host.NewSensitive(v), host.NewSensitive(v)
	}

	for _, name := range plan.Generate {
		if _, ok := existing[name]; ok {
			return ErrGeneratedMeanwhile
		}

		items[name] = host.NewSensitive(plan.generated[name])
	}

	secret := host.Secret{Path: generatedPath(plan.Stack), Items: items}
	if len(existing) > 0 {
		secret.Overwrite, secret.Replaces = true, read
	}

	written, err := e.secrets.Ensure(ctx, secret)
	if err != nil {
		return err
	}

	// The values of this plan are stored, or none of it is applied.
	if !written {
		return ErrGeneratedMeanwhile
	}

	return nil
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
func (e *Engine) StackStatus(ctx context.Context, s *Stack) (_ []ReleaseStatus, retErr error) {
	defer e.recoverErr(ctx, "stack.status", &retErr)

	ctx, span := oplog.Start(ctx, e.logger, slog.LevelInfo, "stack.status", "stack", s.Name())

	out, err := e.stackStatus(ctx, host.StackOwner(s.Namespace(), s.Name()))
	span.End(ctx, err)

	return out, err
}

func (e *Engine) stackStatus(ctx context.Context, owner string) ([]ReleaseStatus, error) {
	if e.releases == nil {
		return nil, ErrNoCluster
	}

	owned, err := e.releases.Owned(ctx, owner)
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
func (e *Engine) DestroyStack(ctx context.Context, s *Stack) (_ []host.ReleaseRef, retErr error) {
	defer e.recoverErr(ctx, "stack.destroy", &retErr)

	ctx, span := oplog.Start(ctx, e.logger, slog.LevelInfo, "stack.destroy", "stack", s.Name())

	out, err := e.destroyStack(ctx, s)
	span.End(ctx, err)

	return out, err
}

func (e *Engine) destroyStack(ctx context.Context, s *Stack) ([]host.ReleaseRef, error) {
	if e.releases == nil {
		return nil, ErrNoCluster
	}

	owned, err := e.releases.Owned(ctx, host.StackOwner(s.Namespace(), s.Name()))
	if err != nil {
		return nil, err
	}

	// The order of an apply, reversed: a release is removed before those it
	// reads. Releases no longer in the stack go first.
	order, diags := eval.StackReleaseOrder(s.s)
	if diags.HasErrors() {
		return nil, diags
	}

	rank := map[string]int{}
	for i, name := range order {
		rank[name] = i + 1
	}

	gone := len(order) + 1

	slices.SortStableFunc(owned, func(a, b host.Revision) int {
		return cmp.Or(rank[b.Release.Name], gone) - cmp.Or(rank[a.Release.Name], gone)
	})

	var removed []host.ReleaseRef

	for _, rev := range owned {
		// A cancel stops between two releases, as for an apply.
		if err := ctx.Err(); err != nil {
			return removed, fmt.Errorf("stopped before release %s/%s: %w", rev.Release.Namespace, rev.Release.Name, err)
		}

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

// PlanInstall plans one release of a module on its own (rigger install): a
// stack of one, owned by no stack.
func (e *Engine) PlanInstall(ctx context.Context, m *Module, in Install, set host.Settings) (_ *ReleasePlan, retDiags hcl.Diagnostics) {
	defer e.recoverDiags(ctx, "release.plan", &retDiags)

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

	evalCtx, cancel := e.evaluationContext(ctx)
	defer cancel()

	res, moreDiags := eval.Evaluate(evalCtx, m.mod, vars, facts, e.evalConfig())
	diags = append(diags, moreDiags...)

	if moreDiags.HasErrors() {
		return nil, diags
	}

	ref := host.ReleaseRef{Namespace: in.Namespace, Name: in.Release}

	alone := []requirer{{ref: ref, module: md, source: in.Source, vars: vars}}
	if moreDiags := e.checkRequires(ctx, alone, []host.ReleaseRef{ref}); moreDiags.HasErrors() {
		return nil, append(diags, moreDiags...)
	}

	o, err := installOrigin(m, in)
	if err != nil {
		return nil, append(diags, errorDiags("Signatures not read", err)...)
	}

	rp, err := e.planRelease(ctx, ref, "", in.Region, o, eval.MarkSensitive(m.mod, valuesObject(in.Values)), res, set)
	if err != nil {
		return nil, append(diags, errorDiags("Release not compared", err)...)
	}

	rp.nextSteps = nextSteps(res.Outputs)

	return &rp, diags
}

// installOrigin is what the release of an install records of its module: the
// source, tag, signers and signatures of a fetched module; for a module
// loaded from files, the source of the install (else its directory) and
// the signatures in its directory, for the admission of the server
// (design 8.2).
func installOrigin(m *Module, in Install) (moduleOrigin, error) {
	if m.origin != nil {
		return *m.origin, nil
	}

	o := moduleOrigin{module: m.mod, source: m.mod.Dir}
	if in.Source != "" {
		o.source = in.Source
	}

	sigs, err := sign.ReadSignatures(m.fsys, ".")
	if err != nil {
		return moduleOrigin{}, err
	}

	for _, s := range sigs {
		o.signatures = append(o.signatures, host.Signature{Payload: s.Payload, Signature: s.Signature})
	}

	return o, nil
}

// ApplyInstall applies the plan of one release; of the options only Adopt
// applies.
func (e *Engine) ApplyInstall(ctx context.Context, rp *ReleasePlan, opts ApplyOptions) (_ ReleaseOutcome, retErr error) {
	defer e.recoverErr(ctx, "release.apply", &retErr)

	if e.releases == nil {
		return ReleaseOutcome{}, ErrNoCluster
	}

	if err := rp.checkAdopt(opts.Adopt); err != nil {
		return ReleaseOutcome{}, err
	}

	return e.applyRelease(ctx, *rp, ApplyOptions{Adopt: opts.Adopt})
}
