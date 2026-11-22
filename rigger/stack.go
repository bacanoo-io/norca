// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/big"
	"slices"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	"github.com/bacanoo-io/norca/rigger/host"
	"github.com/bacanoo-io/norca/rigger/internal/eval"
	"github.com/bacanoo-io/norca/rigger/internal/lang"
	"github.com/bacanoo-io/norca/rigger/module"
	"github.com/bacanoo-io/norca/rigger/oplog"
	"github.com/bacanoo-io/norca/rigger/resource"
	"github.com/bacanoo-io/norca/rigger/source"
)

// Stack is a loaded stack (design 6.4).
type Stack struct {
	s *lang.Stack
	// origin is set for a stack fetched from a source.
	origin *StackOrigin
}

// Name is the name of the stack, the owner of its releases.
func (s *Stack) Name() string { return s.s.Rigger.Name }

// Region is the region of the stack.
func (s *Stack) Region() string { return eval.StackRegion(s.s) }

// ValueFiles are the paths of the value files under values/ of the stack.
func (s *Stack) ValueFiles() []string { return slices.Clone(s.s.ValueFiles) }

// ReadValueFile reads a value file of the stack, such as values/dev.vars.hcl.
func (s *Stack) ReadValueFile(p string) ([]byte, error) { return fs.ReadFile(s.s.FS, p) }

// Metadata is the stack block of a published stack (design 7.5); nil for a
// stack without metadata.hcl.
func (s *Stack) Metadata() *module.Metadata {
	if s.s.Metadata == nil {
		return nil
	}

	md := *s.s.Metadata

	return &md
}

// Digest is the tree digest of the stack directory (design 7.5): its own
// modules, values and lock file included.
func (s *Stack) Digest() (string, error) { return lang.StackDigest(s.s) }

// LockedModule is a module of a source other than the stack's own, as the
// lock file pins it.
type LockedModule struct {
	Source  string      `json:"source"`
	Kind    source.Kind `json:"kind"`
	Version string      `json:"version"`
	Digest  string      `json:"digest"`
}

// LockedModules are the modules of the stack from sources, as its lock
// file pins them, each once, in the order of the releases.
func (s *Stack) LockedModules() []LockedModule {
	var out []LockedModule

	for _, r := range s.s.Releases {
		if r.Address == nil || r.Locked == nil || slices.ContainsFunc(out, func(m LockedModule) bool { return m.Source == r.Source }) {
			continue
		}

		out = append(out, LockedModule{Source: r.Source, Kind: r.Address.Kind, Version: r.Locked.Version, Digest: r.Locked.Digest})
	}

	return out
}

// LoadStack loads the stack in the directory dir of root, and the modules
// of its local sources, inside root (design 6.4).
func (e *Engine) LoadStack(ctx context.Context, root fs.FS, dir string) (_ *Stack, retDiags hcl.Diagnostics) {
	defer e.recoverDiags(ctx, "stack.load", &retDiags)

	ctx, span := oplog.Start(ctx, e.logger, slog.LevelDebug, "stack.load", "dir", dir)

	s, diags := lang.LoadStack(ctx, root, dir, e.langLimits())
	if !diags.HasErrors() {
		diags = append(diags, e.resolveLocked(ctx, s)...)
	}

	span.End(ctx, diagsErr(diags))

	return &Stack{s: s}, diags
}

// ReadStack loads the stack in the directory dir of root without fetching
// the modules of its sources: for its metadata and its digest.
func (e *Engine) ReadStack(ctx context.Context, root fs.FS, dir string) (_ *Stack, retDiags hcl.Diagnostics) {
	defer e.recoverDiags(ctx, "stack.read", &retDiags)

	s, diags := lang.LoadStack(ctx, root, dir, e.langLimits())

	return &Stack{s: s}, diags
}

// langLimits are the limits of the engine for loading.
func (e *Engine) langLimits() lang.Limits {
	return lang.Limits{
		ModuleBytes: e.limits.ModuleBytes, FileBytes: e.limits.FileBytes, Depth: e.limits.Depth, Modules: e.limits.Modules,
	}
}

// ReleaseAction is what a plan does to a release (design 9.2).
type ReleaseAction string

// The actions of a plan.
const (
	ActionUnchanged ReleaseAction = "unchanged"
	ActionInstall   ReleaseAction = "install"
	ActionUpgrade   ReleaseAction = "upgrade"
	// ActionRemove is a release the stack owns and no longer has; it is
	// uninstalled only with prune (design 9.3).
	ActionRemove ReleaseAction = "remove"
	// ActionDisabled is a release written with enabled = false and not
	// installed.
	ActionDisabled ReleaseAction = "disabled"
)

// ReleasePlan is the plan of one release.
type ReleasePlan struct {
	Release host.ReleaseRef `json:"release"`
	Action  ReleaseAction   `json:"action"`
	Module  host.ModuleRef  `json:"module"`
	// Digest is the digest of the release (design 9.2), recorded in the
	// revision; Revision the number of the current one, 0 for none.
	Digest   string `json:"digest,omitempty"`
	Revision uint64 `json:"revision,omitempty"`
	// Changes are the server's changes per resource of an upgrade.
	Changes   []host.Change `json:"changes,omitempty"`
	DependsOn []string      `json:"depends_on,omitempty"`

	request host.ApplyRequest
}

// SecretPlan is the plan of a secret of the stack.
type SecretPlan struct {
	Path string `json:"path"`
	// Action is create, keep or overwrite.
	Action string `json:"action"`
}

// StackPlan is the plan of a stack: what an apply would do.
type StackPlan struct {
	Stack    string        `json:"stack"`
	Region   string        `json:"region"`
	Releases []ReleasePlan `json:"releases"`
	Secrets  []SecretPlan  `json:"secrets,omitempty"`
	// Generate are the names of the values an apply generates.
	Generate []string `json:"generate,omitempty"`
	Outputs  []Output `json:"outputs"`

	namespace string
	generated map[string]string
	secrets   []eval.StackSecret
}

// HasChanges reports whether an apply would change something, removals
// included.
func (p *StackPlan) HasChanges() bool {
	if len(p.Generate) > 0 {
		return true
	}

	for _, s := range p.Secrets {
		if s.Action != "keep" {
			return true
		}
	}

	for _, r := range p.Releases {
		if r.Action != ActionUnchanged && r.Action != ActionDisabled {
			return true
		}
	}

	return false
}

// generatedPath is the secret that holds the generated values of a stack.
func generatedPath(stack string) string {
	return "rigger/stacks/" + stack + "/generated"
}

// ErrNoCluster is the error of an operation on a cluster by an engine
// without the ports it needs.
var ErrNoCluster = errors.New("the engine has no release API or secret store (rigger.WithReleases, rigger.WithSecrets)")

// PlanStack evaluates a stack with its values and compares every release
// with the cluster (design 9.2): unchanged by the digest the current
// revision records, else install or upgrade with the server's plan, and
// remove for a release the stack owns and no longer has. It changes
// nothing.
func (e *Engine) PlanStack(ctx context.Context, s *Stack, values Values) (_ *StackPlan, retDiags hcl.Diagnostics) {
	defer e.recoverDiags(ctx, "stack.plan", &retDiags)

	ctx, span := oplog.Start(ctx, e.logger, slog.LevelInfo, "stack.plan", "stack", s.Name(), "region", s.Region())

	plan, diags := e.planStack(ctx, s, values)
	span.End(ctx, diagsErr(diags))

	return plan, diags
}

func (e *Engine) planStack(ctx context.Context, s *Stack, values Values) (*StackPlan, hcl.Diagnostics) {
	if e.releases == nil || e.secrets == nil {
		return nil, errorDiags("No cluster", ErrNoCluster)
	}

	if e.limits.Duration > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, e.limits.Duration)
		defer cancel()
	}

	vars, diags := eval.Variables(s.s.Vars, inputs(values))
	if diags.HasErrors() {
		return nil, diags
	}

	plan := &StackPlan{Stack: s.Name(), Region: s.Region(), namespace: eval.StackNamespace(s.s), generated: map[string]string{}}

	existing, err := e.readGenerated(ctx, plan)
	if err != nil {
		return nil, errorDiags("Generated values not read", err)
	}

	res, moreDiags := eval.EvaluateStack(ctx, s.s, vars, func(name string) (string, bool) {
		if v, ok := existing[name]; ok {
			return v, true
		}

		if _, ok := plan.generated[name]; !ok {
			plan.generated[name] = generate()
		}

		return plan.generated[name], false
	}, e.evalConfig())
	diags = append(diags, moreDiags...)

	if moreDiags.HasErrors() {
		return nil, diags
	}

	plan.Generate = res.Missing
	plan.secrets = res.Secrets

	for _, o := range res.Outputs {
		plan.Outputs = append(plan.Outputs, Output(o))
	}

	if err := e.planReleases(ctx, plan, res); err != nil {
		return nil, append(diags, errorDiags("Releases not compared", err)...)
	}

	if err := e.planSecrets(ctx, plan); err != nil {
		return nil, append(diags, errorDiags("Secrets not read", err)...)
	}

	return plan, diags
}

// readGenerated reads the generated values of the stack.
func (e *Engine) readGenerated(ctx context.Context, plan *StackPlan) (map[string]string, error) {
	items, err := e.secrets.Read(ctx, generatedPath(plan.Stack))
	if errors.Is(err, host.ErrNotFound) {
		return map[string]string{}, nil
	}

	if err != nil {
		return nil, err
	}

	out := make(map[string]string, len(items))
	for k, v := range items {
		out[k] = v.Reveal()
	}

	return out, nil
}

// planReleases compares the releases of the stack with the cluster.
func (e *Engine) planReleases(ctx context.Context, plan *StackPlan, res *eval.StackResult) error {
	inStack := map[host.ReleaseRef]bool{}

	for _, rr := range res.Releases {
		ref := host.ReleaseRef{Namespace: rr.Namespace, Name: rr.Name}

		var (
			rp  ReleasePlan
			err error
		)

		if rr.Enabled {
			inStack[ref] = true
			rp, err = e.planRelease(ctx, ref, plan.Stack, plan.Region, origin(rr.Release), rr.Values, rr.Result, settings(rr.Release))
		} else {
			rp = ReleasePlan{Release: ref, Action: ActionDisabled}
		}

		if err != nil {
			return fmt.Errorf("release %s: %w", rr.Name, err)
		}

		rp.DependsOn = rr.DependsOn
		plan.Releases = append(plan.Releases, rp)
	}

	owned, err := e.releases.Owned(ctx, plan.Stack)
	if err != nil {
		return err
	}

	for _, rev := range owned {
		if !inStack[rev.Release] {
			plan.Releases = append(plan.Releases, ReleasePlan{
				Release: rev.Release, Action: ActionRemove, Module: rev.Module, Revision: rev.Number,
			})
		}
	}

	return nil
}

// settings are the settings of a release of a stack: a stack waits until a
// release is ready before it applies the next (design 6.5).
func settings(r *lang.StackRelease) host.Settings {
	return host.Settings{Atomic: r.Atomic, Wait: r.Wait == nil || *r.Wait, Timeout: r.Timeout, MaxHistory: r.MaxHistory}
}

// moduleOrigin is a module, where it came from and who signed it.
type moduleOrigin struct {
	module      *lang.Module
	source, ref string
	signers     []string
	signatures  []host.Signature
}

// origin is the module of a release of a stack and where it came from: the
// address and the tag of a source, the path of a local module.
func origin(r *lang.StackRelease) moduleOrigin {
	o := moduleOrigin{module: r.Module, source: r.Source, signers: r.Signers}
	if r.Locked != nil {
		o.ref = r.Locked.Ref
	}

	for _, s := range r.Signatures {
		o.signatures = append(o.signatures, host.Signature{Payload: s.Payload, Signature: s.Signature})
	}

	return o
}

// planRelease builds the request of a release and compares it with the
// current revision.
func (e *Engine) planRelease(ctx context.Context, ref host.ReleaseRef, owner, region string, m moduleOrigin,
	values cty.Value, res *eval.Result, set host.Settings,
) (ReleasePlan, error) {
	req, err := applyRequest(ref, owner, region, m, values, res, set)
	if err != nil {
		return ReleasePlan{}, err
	}

	rp := ReleasePlan{Release: ref, Module: req.Module, Digest: req.Digest, request: req}

	current, err := e.releases.Current(ctx, ref)

	switch {
	case errors.Is(err, host.ErrNotFound):
		rp.Action = ActionInstall

		return rp, nil
	case err != nil:
		return ReleasePlan{}, err
	}

	rp.Revision = current.Number

	if current.Digest == req.Digest && current.Module.Digest == req.Module.Digest {
		rp.Action = ActionUnchanged

		return rp, nil
	}

	rp.Action = ActionUpgrade

	server, err := e.releases.Plan(ctx, req)
	if err != nil {
		return ReleasePlan{}, err
	}

	rp.Changes = server.Changes

	return rp, nil
}

// applyRequest builds the request of a release: the module and its digest,
// the digest of the release, the values a revision records, the resources
// and their secret items.
func applyRequest(ref host.ReleaseRef, owner, region string, m moduleOrigin, values cty.Value, res *eval.Result,
	set host.Settings,
) (host.ApplyRequest, error) {
	moduleDigest, err := lang.Digest(m.module)
	if err != nil {
		return host.ApplyRequest{}, err
	}

	md := m.module.Metadata
	req := host.ApplyRequest{
		Release: ref, Owner: owner, Settings: set,
		Module: host.ModuleRef{
			Name: md.Name, Version: md.Version, Source: m.source, Ref: m.ref, Digest: moduleDigest, Requires: &md.Requires,
			Signers: m.signers, Signatures: m.signatures,
		},
		Secrets: map[string]map[string]host.Sensitive{},
	}

	if req.Values, err = eval.RecordedValues(values); err != nil {
		return host.ApplyRequest{}, err
	}

	for _, r := range res.Resources {
		req.Resources = append(req.Resources, resource.Resource{Kind: r.Kind, Name: r.Name, Spec: r.Spec, Hook: r.Hook})
		if len(r.Secrets) > 0 {
			req.Secrets[r.Name] = r.Secrets
		}
	}

	if req.Digest, err = releaseDigest(req, region, values); err != nil {
		return host.ApplyRequest{}, err
	}

	return req, nil
}

// releaseDigest is the digest of a release (design 9.2): the module digest,
// the facts, the values with sensitive ones as hashes, and the settings.
func releaseDigest(req host.ApplyRequest, region string, values cty.Value) (string, error) {
	vals, err := eval.DigestValues(values)
	if err != nil {
		return "", err
	}

	data, err := json.Marshal(struct {
		Module    string          `json:"module"`
		Release   host.ReleaseRef `json:"release"`
		Region    string          `json:"region"`
		Values    json.RawMessage `json:"values"`
		Settings  host.Settings   `json:"settings"`
		Resources int             `json:"resources"`
	}{req.Module.Digest, req.Release, region, vals, req.Settings, len(req.Resources)})
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(data)

	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// planSecrets reads the secrets of the stack: create the missing ones,
// keep or overwrite the others.
func (e *Engine) planSecrets(ctx context.Context, plan *StackPlan) error {
	for _, s := range plan.secrets {
		_, err := e.secrets.Read(ctx, s.Path)

		switch {
		case errors.Is(err, host.ErrNotFound):
			plan.Secrets = append(plan.Secrets, SecretPlan{Path: s.Path, Action: "create"})
		case err != nil:
			return fmt.Errorf("secret %s: %w", s.Path, err)
		case s.Overwrite:
			plan.Secrets = append(plan.Secrets, SecretPlan{Path: s.Path, Action: "overwrite"})
		default:
			plan.Secrets = append(plan.Secrets, SecretPlan{Path: s.Path, Action: "keep"})
		}
	}

	return nil
}

// generateAlphabet are the characters of a generated value.
const generateAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// generate returns a random value of 32 letters and digits (190 bits).
func generate() string {
	b := make([]byte, 32)
	n := big.NewInt(int64(len(generateAlphabet)))

	for i := range b {
		k, err := rand.Int(rand.Reader, n)
		if err != nil {
			panic(err) // crypto/rand does not fail on supported systems
		}

		b[i] = generateAlphabet[k.Int64()]
	}

	return string(b)
}

// errorDiags makes a diagnostic of an error of the cluster.
func errorDiags(summary string, err error) hcl.Diagnostics {
	return hcl.Diagnostics{{Severity: hcl.DiagError, Summary: summary, Detail: err.Error() + "."}}
}

// inputs maps values onto the inputs of an evaluation.
func inputs(values Values) map[string]eval.Input {
	out := make(map[string]eval.Input, len(values))
	for name, v := range values {
		out[name] = eval.Input{Value: v.Value, Range: v.Range}
	}

	return out
}

// evalConfig is the configuration of an evaluation by the engine.
func (e *Engine) evalConfig() eval.Config {
	return eval.Config{FileBytes: e.limits.FileBytes, Resources: e.limits.Resources, Jobs: e.jobs, Decoder: e.decoder}
}

// Namespace is the namespace of a release of the stack that names none,
// and of the secrets of the stack.
func (s *Stack) Namespace() string { return eval.StackNamespace(s.s) }

// ValidateStack evaluates a stack with values without a cluster (rigger stack
// validate): its variables, rules, locals, the values and modules of its
// releases, its checks, secrets and outputs. Generated values stand as
// fresh random values.
func (e *Engine) ValidateStack(ctx context.Context, s *Stack, values Values) (retDiags hcl.Diagnostics) {
	defer e.recoverDiags(ctx, "stack.validate", &retDiags)

	ctx, span := oplog.Start(ctx, e.logger, slog.LevelInfo, "stack.validate", "stack", s.Name())

	vars, diags := eval.Variables(s.s.Vars, inputs(values))
	if !diags.HasErrors() {
		_, moreDiags := eval.EvaluateStack(ctx, s.s, vars, func(string) (string, bool) { return generate(), false }, e.evalConfig())
		diags = append(diags, moreDiags...)
	}

	span.End(ctx, diagsErr(diags))

	return diags
}
