// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package cli

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/hashicorp/nomad/pack/internal/pkg/caching"
	packerrors "github.com/hashicorp/nomad/pack/internal/pkg/errors"
	"github.com/hashicorp/nomad/pack/internal/pkg/flag"
	"github.com/hashicorp/nomad/pack/internal/pkg/manager"
	"github.com/hashicorp/nomad/pack/internal/pkg/release"
	"github.com/hashicorp/nomad/pack/internal/pkg/renderer"
	"github.com/hashicorp/nomad/pack/internal/pkg/supplychain"
	"github.com/hashicorp/nomad/pack/internal/runner"
	"github.com/hashicorp/nomad/pack/internal/runner/job"
	"github.com/hashicorp/nomad/pack/sdk/pack"
	"github.com/hashicorp/nomad/pack/terminal"
)

// Releases: when the servers provide the release API, run, plan, upgrade,
// destroy and uninstall send the rendered resources of a pack to the
// servers, which apply them in a fixed order and record a revision
// (server-side apply). Against servers without the release API, and with
// --client-side, the jobs are deployed by the client as before; with
// --client-side the client then records a revision if the servers can.

// releaseOptions are the flags of the commands that apply releases.
type releaseOptions struct {
	atomic        string
	timeout       time.Duration
	takeOwnership bool
	maxHistory    int
	secrets       stringList
	clientSide    bool
	regions       string
}

// stringList is a repeatable string flag that does not split on commas.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, " ") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }
func (s *stringList) Type() string       { return "string" }

func (o *releaseOptions) addFlags(f *flag.Set) {
	f.StringVar(&flag.StringVar{
		Name:   "atomic",
		Target: &o.atomic,
		Usage: `"true" restores the previous revision if any step of the release
				fails (resources, deployments, hooks); "false" leaves a failed
				release as far as it got. If not set, production namespaces
				(namespace meta environment = "production") are atomic.`,
	})
	f.DurationVar(&flag.DurationVar{
		Name:    "timeout",
		Target:  &o.timeout,
		Default: 5 * time.Minute,
		Usage:   `How long the servers wait for deployments and hooks.`,
	})
	f.BoolVar(&flag.BoolVar{
		Name:   "take-ownership",
		Target: &o.takeOwnership,
		Usage:  `Take over resources that belong to another release.`,
	})
	f.IntVar(&flag.IntVar{
		Name:   "max-history",
		Target: &o.maxHistory,
		Usage:  `Number of revisions to keep (default 10 for new releases).`,
	})
	f.Var(&o.secrets, "var-secret", `A secret Variables item, as <variable path>:<item>=<value>. Secret
				items are written to the Variable but never stored in the
				release. Can be repeated.`)
	f.BoolVar(&flag.BoolVar{
		Name:   "client-side",
		Target: &o.clientSide,
		EnvVar: EnvClientSide,
		Usage: `Deploy the jobs from the client, as Nomad Pack does, instead of
				letting the servers apply the release; the servers only record
				the revision. Also set by NORCA_PACK_CLIENT_SIDE=1.`,
	})
	f.StringVar(&flag.StringVar{
		Name:   "regions",
		Target: &o.regions,
		Usage: `Comma separated regions to install the release in, in one
				operation: the servers apply it on the leader of every region
				(the authoritative region first) and report the result per
				region. Overrides the regions of metadata.hcl.`,
	})
}

// regionList returns the regions of a multi-region install: those of
// --regions, or else those of the metadata of the pack.
func (o *releaseOptions) regionList(md *pack.Metadata) []string {
	if o.regions != "" {
		return splitRegions(o.regions)
	}
	if md != nil {
		return splitRegions(strings.Join(md.Regions, ","))
	}
	return nil
}

// splitRegions splits a comma separated region list.
func splitRegions(s string) []string {
	var out []string
	for _, r := range strings.Split(s, ",") {
		if r = strings.TrimSpace(r); r != "" && !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out
}

func (o *releaseOptions) atomicPtr() (*bool, error) {
	if o.atomic == "" {
		return nil, nil
	}
	b, err := strconv.ParseBool(o.atomic)
	if err != nil {
		return nil, fmt.Errorf("invalid --atomic value %q", o.atomic)
	}
	return &b, nil
}

// releasesSupported reports whether the servers provide the release API.
func releasesSupported(client *api.Client) bool {
	ok, err := client.Releases().Supported(nil)
	return err == nil && ok
}

// releaseName returns the release name of the deployment: the deployment
// name (the pack name, or "pack@ref" with --ref, unless --name is given)
// with the characters that are not allowed in release names replaced, so
// that a release is the instance the deployment name identifies.
func (c *baseCommand) releaseName(cfg *caching.PackConfig) (string, error) {
	return releaseNameFor(cfg, c.deploymentName)
}

func releaseNameFor(cfg *caching.PackConfig, deploymentName string) (string, error) {
	if deploymentName == "" {
		deploymentName = defaultDeploymentName(cfg)
	}
	return release.Name(deploymentName)
}

// allRenders returns the rendered templates of the pack and its
// dependencies.
func allRenders(r *renderer.Rendered) map[string]string {
	out := make(map[string]string, r.LenParentRenders()+r.LenDependentRenders())
	for k, v := range r.DependentRenders() {
		out[k] = v
	}
	for k, v := range r.ParentRenders() {
		out[k] = v
	}
	return out
}

// jobRenders returns the rendered job templates and the names of the other
// resource templates.
func jobRenders(renders map[string]string) (jobs map[string]string, others []string) {
	jobs = map[string]string{}
	for name, content := range renders {
		if release.IsResourceTemplate(name) {
			// A resource template that renders nothing is a resource the
			// variables switched off.
			if strings.TrimSpace(content) != "" {
				others = append(others, name)
			}
		} else {
			jobs[name] = content
		}
	}
	sort.Strings(others)
	return jobs, others
}

// errResourcesNeedServer is returned when a pack with non-job resources is
// deployed without the release API.
func errResourcesNeedServer(templates []string) error {
	return fmt.Errorf("the pack has resources other than jobs (%s), which need servers with pack release support",
		strings.Join(templates, ", "))
}

// buildRelease turns the rendered pack into a release request.
func (c *baseCommand) buildRelease(
	packManager *manager.PackManager,
	packCfg *caching.PackConfig,
	provenance *supplychain.Result,
	jobCfg *job.CLIConfig,
	r *renderer.Rendered,
	opts *releaseOptions,
	errorContext *packerrors.UIErrorContext,
) (*api.ReleaseApplyRequest, bool) {
	renders := allRenders(r)
	jobTpls, others := jobRenders(renders)

	req := &api.ReleaseApplyRequest{
		Pack:          c.releasePack(packManager, packCfg, provenance),
		Timeout:       opts.timeout,
		TakeOwnership: opts.takeOwnership,
		MaxHistory:    opts.maxHistory,
	}
	var err error
	if req.Atomic, err = opts.atomicPtr(); err != nil {
		c.ui.ErrorWithContext(err, ErrParsingArgsOrFlags, errorContext.GetAll()...)
		return nil, false
	}
	if req.Secrets, err = release.Secrets(opts.secrets); err != nil {
		c.ui.ErrorWithContext(err, ErrParsingArgsOrFlags, errorContext.GetAll()...)
		return nil, false
	}
	// Variables with a secret block go to their Variable item.
	req.Secrets = manager.MergeSecrets(req.Secrets, packManager.Secrets())
	vars := maps.Clone(c.vars)
	for _, name := range packManager.SecretVariables() {
		delete(vars, name)
	}
	varFiles := map[string][]byte{}
	for _, f := range c.varFiles {
		if b, err := os.ReadFile(f); err == nil {
			varFiles[f] = b
		}
	}
	req.Values, req.ValuesHash = release.Values(vars, varFiles)
	_, req.VariableValues = packManager.VariablesSchemas()
	req.VariablesSchemas = packManager.SynthesizedVariablesSchemas()
	req.Regions = opts.regionList(packManager.Metadata())

	for _, name := range others {
		res, err := release.ParseResource(name, renders[name])
		if err != nil {
			c.ui.ErrorWithContext(err, "failed to parse resource template", errorContext.GetAll()...)
			return nil, false
		}
		req.Resources = append(req.Resources, res)
	}

	if len(jobTpls) > 0 {
		depConfig := runner.Config{
			PackName:       packCfg.Name,
			PathPath:       packCfg.Path,
			PackRef:        packCfg.Ref,
			DeploymentName: getDeploymentName(c, packCfg),
			RegistryName:   packCfg.Registry,
			PackDigest:     provenanceDigest(provenance),
			SignerKeyID:    provenance.SignerKeyID(),
		}
		// Parsing sets the namespace and region of the client from the
		// templates; use a client of its own.
		parseClient, err := c.getAPIClient()
		if err != nil {
			c.ui.ErrorWithContext(err, "failed to initialize client", errorContext.GetAll()...)
			return nil, false
		}
		jobRunner, err := generateRunner(parseClient, "job", jobCfg, &depConfig)
		if err != nil {
			c.ui.ErrorWithContext(err, "failed to generate deployer", errorContext.GetAll()...)
			return nil, false
		}
		jobRunner.SetTemplates(jobTpls)
		if errs := jobRunner.ParseTemplates(); errs != nil {
			for _, e := range errs {
				e.Context.Append(errorContext)
				c.ui.ErrorWithContext(e.Err, e.Subject, e.Context.GetAll()...)
			}
			return nil, false
		}
		// Jobs of other pack deployments and jobs not deployed by a pack
		// conflict, as they do for the client-side deployer, unless they are
		// taken over (--take-ownership or --deploy-override).
		if jobOverride(jobCfg) {
			req.TakeOwnership = true
		}
		if !req.TakeOwnership {
			// A multi-region install checks every region of the install
			// rather than the region named by the templates.
			regions := req.Regions
			if len(regions) == 0 {
				regions = []string{""}
			}
			for _, region := range regions {
				if region != "" {
					parseClient.SetRegion(region)
				}
				if errs := jobRunner.CheckForConflicts(errorContext); errs != nil {
					for _, e := range errs {
						ctx := e.Context.GetAll()
						if region != "" {
							ctx = append(ctx, "Region: "+region)
						}
						c.ui.ErrorWithContext(e.Err, e.Subject, ctx...)
					}
					return nil, false
				}
			}
		}
		parsed := jobRunner.ParsedTemplates().(map[string]job.ParsedTemplate)
		names := make([]string, 0, len(parsed))
		for name := range parsed {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			pt := parsed[name]
			j := pt.Job()
			// Jobs without an explicit namespace or region belong to the
			// namespace and region of the release.
			if !pt.HasNamespace() {
				j.Namespace = nil
			}
			if !pt.HasRegion() {
				j.Region = nil
			}
			req.Resources = append(req.Resources, &api.ReleaseResource{Type: api.ReleaseResourceJob, Source: name, Job: j})
		}
	}
	if len(req.Resources) == 0 {
		c.ui.ErrorWithContext(packerrors.ErrNoTemplatesRendered, "no templates rendered", errorContext.GetAll()...)
		return nil, false
	}
	release.SortResources(req.Resources)
	return req, true
}

// EnvClientSide makes run, plan and destroy use the client-side deployer.
const EnvClientSide = "NORCA_PACK_CLIENT_SIDE"

// jobOverride reports whether --deploy-override is set.
func jobOverride(cfg *job.CLIConfig) bool {
	if cfg == nil {
		return false
	}
	if cfg.RunConfig != nil && cfg.RunConfig.DeployOverride {
		return true
	}
	return cfg.PlanConfig != nil && cfg.PlanConfig.DeployOverride
}

// releasePack describes the pack for the release.
func (c *baseCommand) releasePack(packManager *manager.PackManager, packCfg *caching.PackConfig, provenance *supplychain.Result) *api.ReleasePack {
	p := &api.ReleasePack{
		Name:     packCfg.Name,
		Version:  packCfg.Ref,
		Registry: packCfg.Registry,
		Path:     packCfg.Path,
		Digest:   provenanceDigest(provenance),
		Signer:   provenance.SignerKeyID(),
	}
	if md := packManager.Metadata(); md != nil && md.Pack != nil {
		if md.Pack.Version != "" {
			p.Version = md.Pack.Version
		}
		if rq := md.Pack.Requires; rq != nil {
			p.Requires = &api.ReleasePackRequires{Norca: rq.Norca, Features: rq.Features}
			if p.Requires.Norca == "" {
				p.Requires.Norca = rq.NomadFork
			}
		}
	}
	if origin, err := supplychain.ReadOrigin(packCfg.Path); err == nil && origin != nil {
		for _, s := range origin.Signatures {
			p.Signatures = append(p.Signatures, &api.ReleasePackSignature{Payload: string(s.Payload), Signature: s.Signature})
		}
	}
	return p
}

// printChanges prints the changes of a release operation.
func printChanges(ui terminal.UI, changes []*api.ReleaseResourceChange) {
	if len(changes) == 0 {
		return
	}
	tbl := terminal.NewTable("Action", "Type", "Namespace", "Name", "Note")
	for _, ch := range changes {
		note := ch.Error
		if note == "" && ch.Owner != "" {
			note = "owned by " + ch.Owner
		}
		if note == "" && ch.Hook != "" {
			note = "hook " + ch.Hook
		}
		tbl.Rows = append(tbl.Rows, []string{ch.Action, ch.Type, ch.Namespace, ch.Name, note})
	}
	ui.Table(tbl)
}

// printWarnings prints the warnings of a release operation.
func printWarnings(ui terminal.UI, warnings []string) {
	for _, w := range warnings {
		ui.Warning(w)
	}
}

// applyRelease applies a release through the server-side path and reports
// the result. It returns the exit code.
func (c *baseCommand) applyRelease(client *api.Client, name string, req *api.ReleaseApplyRequest, upgradeOnly bool) int {
	if upgradeOnly {
		regions := req.Regions
		if len(regions) == 0 {
			regions = []string{""}
		}
		for _, region := range regions {
			if _, _, err := client.Releases().Info(name, &api.QueryOptions{Region: region}); err != nil {
				c.ui.ErrorWithContext(err, fmt.Sprintf("release %q not found%s; use run to install it", name, inRegion(region)))
				return 1
			}
		}
	}
	if len(req.Regions) > 0 {
		c.ui.Info(fmt.Sprintf("Applying release %q (%d resources) in the regions %s",
			name, len(req.Resources), strings.Join(req.Regions, ", ")))
		resp, _, err := client.Releases().Apply(name, req, nil)
		if err != nil {
			c.ui.ErrorWithContext(err, "release failed", "Release: "+name)
			return 1
		}
		return c.printRegionResults(name, resp, false)
	}
	c.ui.Info(fmt.Sprintf("Applying release %q (%d resources)", name, len(req.Resources)))
	resp, _, err := client.Releases().Apply(name, req, nil)
	if err != nil {
		c.ui.ErrorWithContext(err, "release failed", "Release: "+name)
		if rel, _, ierr := client.Releases().Info(name, nil); ierr == nil && rel.Status != nil {
			c.ui.Info(fmt.Sprintf("Release %q is %s at revision %d", name, rel.Status.Phase, rel.Status.Revision))
		}
		return 1
	}
	printChanges(c.ui, resp.Changes)
	printWarnings(c.ui, resp.Warnings)
	c.ui.Success(fmt.Sprintf("Release %q deployed at revision %d", name, resp.Revision))
	return 0
}

// recordRelease records a revision of jobs the client deployed itself.
func (c *baseCommand) recordRelease(client *api.Client, name string, req *api.ReleaseApplyRequest) {
	req.RecordOnly = true
	resp, _, err := client.Releases().Apply(name, req, nil)
	if err != nil {
		c.ui.Warning(fmt.Sprintf("Failed to record release %q: %v", name, err))
		return
	}
	printWarnings(c.ui, resp.Warnings)
	c.ui.Info(fmt.Sprintf("Recorded revision %d of release %q", resp.Revision, name))
}
