// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	"github.com/bacanoo-io/norca/rigger/host"
	"github.com/bacanoo-io/norca/rigger/internal/eval"
	"github.com/bacanoo-io/norca/rigger/internal/lang"
	"github.com/bacanoo-io/norca/rigger/module"
	"github.com/bacanoo-io/norca/rigger/version"
)

// What a plan checks of what is required, both needing a cluster:
// required_version of the rigger blocks of a stack and its parts, a
// constraint on the version of Norca; and requires { module } of the
// modules it installs (design 6.5), met by a release of the stack or by one
// deployed in the namespace of the release that requires it.

// checkRequiredVersion checks the required_version of a stack and of its
// parts against the version of the cluster.
func (e *Engine) checkRequiredVersion(ctx context.Context, s *lang.Stack) hcl.Diagnostics {
	var (
		diags   hcl.Diagnostics
		cluster *version.Version
	)

	for _, st := range withParts(s) {
		text, rng := st.Rigger.RequiredVersion, st.Rigger.Range
		if text == "" {
			continue
		}

		want, err := version.ParseConstraint(text)
		if err != nil {
			diags = append(diags, diagAt(rng, "Invalid required_version",
				fmt.Sprintf("required_version %q is no version constraint: %s.", text, err)))

			continue
		}

		if cluster == nil {
			v, err := e.clusterVersion(ctx)
			if err != nil {
				return append(diags, diagAt(rng, "Version of Norca not known",
					fmt.Sprintf("required_version %q can't be checked: %s.", text, err)))
			}

			cluster = &v
		}

		if !want.Check(*cluster) {
			diags = append(diags, diagAt(rng, "Version of Norca",
				fmt.Sprintf("The stack %s has required_version %q, the cluster runs Norca %s.", st.Rigger.Name, text, cluster)))
		}
	}

	return diags
}

// withParts are a stack and the stacks of its parts.
func withParts(s *lang.Stack) []*lang.Stack {
	out := []*lang.Stack{s}

	for _, p := range s.Parts {
		if p.Stack != nil {
			out = append(out, withParts(p.Stack)...)
		}
	}

	return out
}

func (e *Engine) clusterVersion(ctx context.Context) (version.Version, error) {
	if e.cluster == nil {
		return version.Version{}, ErrNoCluster
	}

	info, err := e.cluster.Info(ctx)
	if err != nil {
		return version.Version{}, err
	}

	v, err := version.Parse(info.Version)
	if err != nil {
		return version.Version{}, fmt.Errorf("the cluster says %q for its version: %w", info.Version, err)
	}

	return v, nil
}

// requirer is a release that is planned, as its requirements see it.
type requirer struct {
	ref    host.ReleaseRef
	module module.Metadata
	source string
	vars   cty.Value
	rng    hcl.Range
}

// stackRequirers are the releases of a stack and its parts that are
// enabled.
func stackRequirers(res *eval.StackResult) []requirer {
	var out []requirer

	for _, rr := range res.Releases {
		if !rr.Enabled || rr.Release.Module == nil {
			continue
		}

		out = append(out, requirer{
			ref:    host.ReleaseRef{Namespace: rr.Namespace, Name: rr.Name},
			module: rr.Release.Module.Metadata, source: rr.Release.Source,
			vars: rr.Vars, rng: rr.Release.Block.DefRange,
		})
	}

	return out
}

// plannedReleases are the releases a plan changes, keeps or removes.
func plannedReleases(plan *StackPlan) []host.ReleaseRef {
	out := make([]host.ReleaseRef, 0, len(plan.Releases))
	for _, rp := range plan.Releases {
		out = append(out, rp.Release)
	}

	return out
}

// checkRequires checks the required modules of what is planned. replaced
// are the releases the plan changes or removes: as they are now, they meet
// nothing.
func (e *Engine) checkRequires(ctx context.Context, planned []requirer, replaced []host.ReleaseRef) hcl.Diagnostics {
	var diags hcl.Diagnostics

	installed := map[string][]host.Revision{}

	for _, r := range planned {
		for _, req := range r.module.Requires.Modules {
			needed, want, err := requirement(req, r.vars)
			if err != nil {
				diags = append(diags, r.diag("Invalid requirement", fmt.Sprintf("The module %s requires %s: %s.", r.module.Name, req.Name, err)))

				continue
			}

			if !needed || slices.ContainsFunc(planned, func(other requirer) bool {
				return other.ref != r.ref && want.metBy(other.module.Name, other.source, other.module.Version)
			}) {
				continue
			}

			revs, ok := installed[r.ref.Namespace]
			if !ok {
				if revs, err = e.releases.List(ctx, r.ref.Namespace); err != nil {
					return append(diags, r.diag("Requirements not checked",
						fmt.Sprintf("The releases of the namespace %s were not listed: %s.", r.ref.Namespace, err)))
				}

				installed[r.ref.Namespace] = revs
			}

			if !want.metByInstalled(revs, replaced) {
				diags = append(diags, r.diag("Required module missing", fmt.Sprintf(
					"The module %s requires the module %s: the stack has none that fits, and none is deployed in the namespace %s. Add or install it.",
					r.module.Name, want, r.ref.Namespace)))
			}
		}
	}

	return diags
}

func (r requirer) diag(summary, detail string) *hcl.Diagnostic {
	d := &hcl.Diagnostic{Severity: hcl.DiagError, Summary: summary, Detail: detail}
	if r.rng.Filename != "" {
		d.Subject = r.rng.Ptr()
	}

	return d
}

// requiredModule is a requirement: a module by its name, which is the last
// element of the address when the requirement has one, or by that address.
type requiredModule struct {
	name, source string
	constraint   *version.Constraint
	text         string
}

// requirement reads a requirement, and whether its when makes it apply.
func requirement(req module.RequiredModule, vars cty.Value) (bool, requiredModule, error) {
	want := requiredModule{name: req.Name, source: req.Source, text: req.Version}
	if req.Source != "" {
		want.name = path.Base(strings.TrimRight(req.Source, "/"))
	}

	if req.Version != "" {
		c, err := version.ParseConstraint(req.Version)
		if err != nil {
			return false, want, err
		}

		want.constraint = &c
	}

	if req.When == "" {
		return true, want, nil
	}

	on, err := isSettingOn(vars, req.When)

	return on, want, err
}

func (m requiredModule) String() string {
	out := m.name
	if m.source != "" {
		out = m.source
	}

	if m.text != "" {
		out += " " + m.text
	}

	return out
}

// metBy reports whether a module meets the requirement.
func (m requiredModule) metBy(name, source, v string) bool {
	if name != m.name && (m.source == "" || source != m.source) {
		return false
	}

	if m.constraint == nil {
		return true
	}

	parsed, err := version.Parse(v)

	return err == nil && m.constraint.Check(parsed)
}

// metByInstalled reports whether a deployed release that stays meets the
// requirement.
func (m requiredModule) metByInstalled(revs []host.Revision, replaced []host.ReleaseRef) bool {
	return slices.ContainsFunc(revs, func(rev host.Revision) bool {
		return rev.Status == host.StatusDeployed && !slices.Contains(replaced, rev.Release) &&
			m.metBy(rev.Module.Name, rev.Module.Source, rev.Module.Version)
	})
}

// isSettingOn reads the when of a requirement: the path of a variable
// (dumps.enabled, or var.dumps.enabled). Off are false, null and the empty
// string; a value that is only known at apply is on.
func isSettingOn(vars cty.Value, setting string) (bool, error) {
	v := vars

	for name := range strings.SplitSeq(strings.TrimPrefix(setting, "var."), ".") {
		v, _ = v.Unmark()

		switch {
		case !v.IsKnown():
			return true, nil
		case v.IsNull():
			return false, nil
		case v.Type().IsObjectType() && v.Type().HasAttribute(name):
			v = v.GetAttr(name)
		case v.Type().IsMapType():
			key := cty.StringVal(name)

			has, _ := v.HasIndex(key).Unmark()
			if !has.True() {
				return false, nil
			}

			v = v.Index(key)
		default:
			return false, fmt.Errorf("when = %q names no variable of the module", setting)
		}
	}

	v, _ = v.Unmark()

	switch {
	case !v.IsKnown():
		return true, nil
	case v.IsNull():
		return false, nil
	case v.Type() == cty.Bool:
		return v.True(), nil
	case v.Type() == cty.String:
		return v.AsString() != "", nil
	default:
		return true, nil
	}
}
