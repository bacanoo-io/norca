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
	// commit is the commit a stack loaded from files was fetched at
	// (SetCommit).
	commit string
}

// SetCommit says at which git commit the files of the stack were fetched:
// a client that reads a stack from a repository knows it, and the engine
// can't. The releases of the stack's own modules record it
// (host.ModuleRef.Commit). A published stack has the commit of its origin.
func (s *Stack) SetCommit(commit string) { s.commit = commit }

// sourceCommit is the commit of the files of the stack: of its origin, or
// the one SetCommit named; empty when nobody knows it.
func (s *Stack) sourceCommit() string {
	if s.origin != nil && s.origin.Commit != "" {
		return s.origin.Commit
	}

	return s.commit
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

// Readme is the README.md of the stack directory.
func (s *Stack) Readme() ([]byte, error) {
	return fs.ReadFile(s.s.FS, "README.md") // FS is the stack directory
}

// Digest is the tree digest of the stack directory (design 7.5): its own
// modules, values and lock file included.
func (s *Stack) Digest() (string, error) { return lang.StackDigest(s.s) }

// Variables returns the variables of the stack in the order of its
// variables.hcl, as Module.Variables describes those of a module.
func (s *Stack) Variables() []module.Variable { return variablesOf(s.s.Vars) }

// Outputs returns the outputs of the stack in the order of its outputs.hcl;
// nothing is evaluated.
func (s *Stack) Outputs() []module.Output { return outputsOf(s.s.Outputs) }

// StackModule is a module block of a stack: the release it makes, its
// source and version constraint as written, and the version the lock file
// pins (empty for a module of the stack's own and for one the lock does not
// pin).
type StackModule struct {
	Release    string `json:"release"`
	Source     string `json:"source"`
	Constraint string `json:"constraint,omitempty"`
	Version    string `json:"version,omitempty"`
}

// Modules returns the module blocks of the stack in the order of its
// releases.
func (s *Stack) Modules() []StackModule {
	out := make([]StackModule, 0, len(s.s.Releases))

	for _, r := range s.s.Releases {
		m := StackModule{Release: r.Name, Source: r.Source, Constraint: r.Version}
		if r.Locked != nil {
			m.Version = r.Locked.Version
		}

		out = append(out, m)
	}

	return out
}

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
// of its local sources, inside root (design 6.4). With errors in the
// diagnostics the stack is what could be read of it, and nil when dir is no
// directory of root.
func (e *Engine) LoadStack(ctx context.Context, root fs.FS, dir string) (_ *Stack, retDiags hcl.Diagnostics) {
	defer e.recoverDiags(ctx, "stack.load", &retDiags)

	ctx, span := oplog.Start(ctx, e.logger, slog.LevelDebug, "stack.load", "dir", dir)

	s, diags := lang.LoadStack(ctx, root, dir, e.langLimits())
	if !diags.HasErrors() {
		diags = append(diags, e.resolveLocked(ctx, s)...)
	}

	span.End(ctx, diagsErr(diags))

	return loadedStack(s), diags
}

// ReadStack loads the stack in the directory dir of root without fetching
// the modules of its sources: for its metadata and its digest. With errors
// it returns what LoadStack does.
func (e *Engine) ReadStack(ctx context.Context, root fs.FS, dir string) (_ *Stack, retDiags hcl.Diagnostics) {
	defer e.recoverDiags(ctx, "stack.read", &retDiags)

	s, diags := lang.LoadStack(ctx, root, dir, e.langLimits())

	return loadedStack(s), diags
}

// loadedStack is the stack of what was loaded; nil when nothing was (dir is
// no directory of the root), because no method of a stack without it works.
func loadedStack(s *lang.Stack) *Stack {
	if s == nil {
		return nil
	}

	return &Stack{s: s}
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
	// ActionDisabled is a release written with enabled = false: the stack
	// does not install it. One that the stack installed before stays as it
	// is (its plan has the revision that stays), until it is enabled again
	// or its block is removed and the release pruned.
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
	// TakesOver says whose the release is when an apply of the plan takes
	// it over, which needs ApplyOptions.Adopt; empty for none.
	TakesOver string `json:"takes_over,omitempty"`
	// Changes are the server's changes per resource of an install or an
	// upgrade, from its dry run.
	Changes   []host.Change `json:"changes,omitempty"`
	DependsOn []string      `json:"depends_on,omitempty"`

	request host.ApplyRequest
	// nextSteps is the text of the output next_steps of an install.
	nextSteps string
}

// NextStepsOutput is the name of the output a module shows its user after
// an install: what to do next, as text (a string; a heredoc template reads
// best). A sensitive one is not shown.
const NextStepsOutput = "next_steps"

// NextSteps returns the text of the output next_steps of the module of a
// plan of an install (PlanInstall), to be shown after its apply; empty when
// the module has no such output, when it is not a string, or when it is
// sensitive. A plan of a stack has none.
func (rp *ReleasePlan) NextSteps() string { return rp.nextSteps }

// nextSteps is the text of the output next_steps among outputs.
func nextSteps(outputs []eval.Output) string {
	for _, o := range outputs {
		if o.Name != NextStepsOutput || o.Sensitive || o.Value == nil {
			continue
		}

		var text string
		if err := json.Unmarshal(o.Value, &text); err != nil {
			return ""
		}

		return text
	}

	return ""
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
	// commit is the commit of the files of the stack (Stack.sourceCommit).
	commit  string
	secrets []eval.StackSecret
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

	vars, diags := eval.Variables(s.s.Vars, inputs(values))
	if diags.HasErrors() {
		return nil, diags
	}

	if moreDiags := e.checkRequiredVersion(ctx, s.s); moreDiags.HasErrors() {
		return nil, append(diags, moreDiags...)
	}

	plan := &StackPlan{
		Stack: s.Name(), Region: s.Region(), namespace: eval.StackNamespace(s.s), generated: map[string]string{},
		commit: s.sourceCommit(),
	}

	existing, err := e.readGenerated(ctx, plan)
	if err != nil {
		return nil, errorDiags("Generated values not read", err)
	}

	evalCtx, cancel := e.evaluationContext(ctx)
	defer cancel()

	res, moreDiags := eval.EvaluateStack(evalCtx, s.s, vars, func(name string) (string, bool) {
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

	moreDiags, err = e.planReleases(ctx, plan, res)
	if err != nil {
		return nil, append(diags, errorDiags("Releases not compared", err)...)
	}

	diags = append(diags, moreDiags...)

	if moreDiags := e.checkRequires(ctx, stackRequirers(res), plannedReleases(plan)); moreDiags.HasErrors() {
		return nil, append(diags, moreDiags...)
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

// planReleases compares the releases of the stack with the cluster. The
// diagnostics are warnings of the plan (keptDisabled).
//
// A release of the stack is in the stack whether it is enabled or not: what
// a plan removes is a release the stack owns whose entry is gone (design
// 9.3). A release that was installed and is disabled since stays installed,
// and nothing of the stack changes it until it is enabled again.
func (e *Engine) planReleases(ctx context.Context, plan *StackPlan, res *eval.StackResult) (hcl.Diagnostics, error) {
	owner := host.StackOwner(plan.namespace, plan.Stack)
	inStack := map[host.ReleaseRef]bool{}

	for _, rr := range res.Releases {
		ref := host.ReleaseRef{Namespace: rr.Namespace, Name: rr.Name}
		inStack[ref] = true

		rp := ReleasePlan{Release: ref, Action: ActionDisabled}

		if rr.Enabled {
			var err error

			m := origin(rr.Release)

			// A module of the stack's own directory came with the files
			// of the stack: it is of their commit.
			if m.commit == "" && rr.Release.Address == nil {
				m.commit = plan.commit
			}

			rp, err = e.planRelease(ctx, ref, owner, plan.Region, m, rr.Values, rr.Result, settings(rr.Release))
			if err != nil {
				return nil, fmt.Errorf("release %s: %w", rr.Name, err)
			}
		}

		rp.DependsOn = rr.DependsOn
		plan.Releases = append(plan.Releases, rp)
	}

	owned, err := e.releases.Owned(ctx, owner)
	if err != nil {
		return nil, err
	}

	installed := map[host.ReleaseRef]host.Revision{}

	for _, rev := range owned {
		installed[rev.Release] = rev

		if !inStack[rev.Release] {
			plan.Releases = append(plan.Releases, ReleasePlan{
				Release: rev.Release, Action: ActionRemove, Module: rev.Module, Revision: rev.Number,
			})
		}
	}

	return keptDisabled(plan, res, installed)
}

// keptDisabled puts into the plan what stays of the disabled releases the
// stack has installed: the revision and its module. It warns of one whose
// module in the stack is not the one installed (another version, or another
// content of the same version): the stack does not update a release that is
// disabled. Its values are not compared, since a disabled release is not
// evaluated. The releases of res are the first of the plan, in their order.
func keptDisabled(plan *StackPlan, res *eval.StackResult, installed map[host.ReleaseRef]host.Revision) (hcl.Diagnostics, error) {
	var diags hcl.Diagnostics

	for i, rr := range res.Releases {
		rp := &plan.Releases[i]

		rev, ok := installed[rp.Release]
		if rr.Enabled || !ok {
			continue
		}

		rp.Module, rp.Revision = rev.Module, rev.Number

		if rr.Release == nil || rr.Release.Module == nil {
			continue
		}

		digest, err := lang.Digest(rr.Release.Module)
		if err != nil {
			return nil, fmt.Errorf("release %s: %w", rr.Name, err)
		}

		md := rr.Release.Module.Metadata
		if md.Version == rev.Module.Version && digest == rev.Module.Digest {
			continue
		}

		has := fmt.Sprintf("the module %s %s", md.Name, md.Version)
		if md.Version == rev.Module.Version {
			has = fmt.Sprintf("another content of the module %s %s", md.Name, md.Version)
		}

		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagWarning, Summary: "Disabled release not updated",
			Detail: fmt.Sprintf("The release %s is disabled and stays installed at revision %d, of the module %s %s. "+
				"The stack has %s for it, which is applied when the release is enabled again. "+
				"To uninstall the release, remove its block and apply with prune.",
				rr.Name, rev.Number, rev.Module.Name, rev.Module.Version, has),
			Subject: rr.Release.Block.DefRange.Ptr(),
		})
	}

	return diags, nil
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
	// commit is the git commit the module was fetched at.
	commit     string
	signers    []string
	signatures []host.Signature
}

// origin is the module of a release of a stack and where it came from: the
// address and the tag of a source, the path of a local module.
func origin(r *lang.StackRelease) moduleOrigin {
	o := moduleOrigin{module: r.Module, source: r.Source, signers: r.Signers}
	if r.Locked != nil {
		o.ref, o.commit = r.Locked.Ref, r.Locked.Commit
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
		return e.planNewRelease(ctx, rp)
	case err != nil:
		return ReleasePlan{}, err
	}

	rp.Revision = current.Number

	switch {
	case current.ForeignManager != "":
		rp.TakesOver = "the package manager " + current.ForeignManager
	case current.Owner != "" && current.Owner != owner:
		rp.TakesOver = "the stack " + current.Owner
	}

	// A release of the same content that no stack owns becomes the stack's;
	// one that did not succeed is applied again.
	unchanged := current.Status == host.StatusDeployed &&
		current.Digest == req.Digest && current.Module.Digest == req.Module.Digest && current.Owner == owner
	if unchanged && rp.TakesOver == "" {
		rp.Action = ActionUnchanged

		return rp, nil
	}

	rp.Action = ActionUpgrade

	// A plan writes nothing: it shows what a take-over would change, and
	// the apply of it needs Adopt (CheckAdopt).
	preview := req
	preview.Adopt = rp.TakesOver != ""

	server, err := e.releases.Plan(ctx, preview)
	if err != nil {
		return ReleasePlan{}, err
	}

	rp.Changes = server.Changes

	return rp, nil
}

// planNewRelease plans a release of a stack that does not exist yet. The servers check
// a new release too (a dry run): its resources are planned as created, and
// a resource they would refuse is found before any release of the stack is
// applied.
func (e *Engine) planNewRelease(ctx context.Context, rp ReleasePlan) (ReleasePlan, error) {
	rp.Action = ActionInstall

	server, err := e.releases.Plan(ctx, rp.request)
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
			Name: md.Name, Version: md.Version, Source: m.source, Ref: m.ref, Commit: m.commit, Digest: moduleDigest, Requires: &md.Requires,
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
	return eval.Config{
		FileBytes: e.limits.FileBytes, Resources: e.limits.Resources, Jobs: e.jobs, Decoder: e.decoder, Kinds: e.kinds,
	}
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
		evalCtx, cancel := e.evaluationContext(ctx)
		defer cancel()

		_, moreDiags := eval.EvaluateStack(evalCtx, s.s, vars, func(string) (string, bool) { return generate(), false }, e.evalConfig())
		diags = append(diags, moreDiags...)
	}

	span.End(ctx, diagsErr(diags))

	return diags
}
