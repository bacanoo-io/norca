// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/posener/complete"

	"github.com/hashicorp/nomad/pack/internal/pkg/flag"
	"github.com/hashicorp/nomad/pack/terminal"
)

// releaseClient initializes the API client and checks that the servers
// provide the release API.
func (c *baseCommand) releaseClient() (*api.Client, bool) {
	client, err := c.getAPIClient()
	if err != nil {
		c.ui.ErrorWithContext(err, "failed to initialize client")
		return nil, false
	}
	if !releasesSupported(client) {
		c.ui.ErrorWithContext(api.ErrReleasesNotSupported, "this command needs pack release support on the servers")
		return nil, false
	}
	return client, true
}

// conditionStatus returns the status of a release condition, or "-".
func conditionStatus(s *api.ReleaseStatus, t string) string {
	if c := s.Condition(t); c != nil {
		return c.Status
	}
	return "-"
}

// printRelease prints the details of a release.
func printRelease(ui terminal.UI, rel *api.Release) {
	spec, status := rel.Spec, rel.Status
	if spec == nil {
		spec = &api.ReleaseSpec{}
	}
	if status == nil {
		status = &api.ReleaseStatus{}
	}
	values := []terminal.NamedValue{
		{Name: "Name", Value: rel.Name},
		{Name: "Namespace", Value: rel.Namespace},
		{Name: "Phase", Value: status.Phase},
		{Name: "Deployed revision", Value: status.Revision},
		{Name: "Latest revision", Value: spec.Revision},
		{Name: "Atomic", Value: spec.Atomic},
		{Name: "Max history", Value: spec.MaxHistory},
	}
	if p := spec.Pack; p != nil {
		values = append(values,
			terminal.NamedValue{Name: "Pack", Value: p.Name},
			terminal.NamedValue{Name: "Version", Value: p.Version},
			terminal.NamedValue{Name: "Registry", Value: p.Registry})
		if p.Digest != "" {
			values = append(values, terminal.NamedValue{Name: "Digest", Value: p.Digest})
		}
		if p.TrustedSigner != "" {
			values = append(values, terminal.NamedValue{Name: "Trusted signer", Value: p.TrustedSigner})
		}
	}
	ui.NamedValues(values)

	if len(status.Conditions) > 0 {
		ui.Output("")
		tbl := terminal.NewTable("Condition", "Status", "Reason", "Message")
		for _, c := range status.Conditions {
			tbl.Rows = append(tbl.Rows, []string{c.Type, c.Status, c.Reason, limit(c.Message, 100)})
		}
		ui.Table(tbl)
	}
	if op := status.LastOperation; op != nil {
		ui.Output("")
		msg := fmt.Sprintf("Last operation: %s of revision %d by %s, started %s", op.Action, op.Revision, op.User, op.StartTime)
		if op.Error != "" {
			msg += "\nError: " + op.Error
		}
		ui.Output(msg)
	}
	if len(status.Resources) > 0 {
		ui.Output("")
		tbl := terminal.NewTable("Type", "Namespace", "Name", "Version")
		for _, r := range status.Resources {
			v := ""
			if r.Version > 0 {
				v = strconv.FormatUint(r.Version, 10)
			}
			tbl.Rows = append(tbl.Rows, []string{r.Type, r.Namespace, r.Name, v})
		}
		ui.Table(tbl)
	}
	if t := status.LastTest; t != nil {
		ui.Output("")
		ui.Output(fmt.Sprintf("Last test of revision %d at %s: succeeded=%t", t.Revision, t.EndTime, t.Succeeded))
	}
}

// printReleases prints a release list.
func printReleases(ui terminal.UI, releases []*api.Release) {
	tbl := terminal.NewTable("Release", "Namespace", "Pack", "Version", "Revision", "Phase", "Drifted")
	for _, rel := range releases {
		pack, ver := "", ""
		if rel.Spec != nil && rel.Spec.Pack != nil {
			pack, ver = rel.Spec.Pack.Name, rel.Spec.Pack.Version
		}
		status := rel.Status
		if status == nil {
			status = &api.ReleaseStatus{}
		}
		tbl.Rows = append(tbl.Rows, []string{
			rel.Name, rel.Namespace, pack, ver, strconv.FormatUint(status.Revision, 10), status.Phase,
			conditionStatus(status, api.ReleaseConditionDrifted),
		})
	}
	ui.Table(tbl)
}

// HistoryCommand lists the revisions of a release.
type HistoryCommand struct {
	*baseCommand
}

func (c *HistoryCommand) Run(args []string) int {
	c.cmdKey = "history"
	if err := c.Init(WithExactArgs(1, args), WithFlags(c.Flags()), WithNoConfig()); err != nil {
		c.ui.ErrorWithContext(err, ErrParsingArgsOrFlags)
		c.ui.Info(c.helpUsageMessage())
		return 1
	}
	client, ok := c.releaseClient()
	if !ok {
		return 1
	}
	revs, _, err := client.Releases().History(c.args[0], nil)
	if err != nil {
		c.ui.ErrorWithContext(err, "failed to read the release history", "Release: "+c.args[0])
		return 1
	}
	if len(revs) == 0 {
		c.ui.Warning(fmt.Sprintf("release %q not found", c.args[0]))
		return 1
	}
	tbl := terminal.NewTable("Revision", "Created", "Action", "Pack", "Version", "Phase", "User", "Description")
	for _, rev := range revs {
		s, st := rev.Spec, rev.Status
		if s == nil {
			s = &api.ReleaseRevisionSpec{}
		}
		if st == nil {
			st = &api.ReleaseRevisionStatus{}
		}
		pack, ver := "", ""
		if s.Pack != nil {
			pack, ver = s.Pack.Name, s.Pack.Version
		}
		desc := s.Description
		if st.Message != "" {
			desc = strings.TrimSpace(desc + " " + limit(st.Message, 80))
		}
		tbl.Rows = append(tbl.Rows, []string{
			strconv.FormatUint(rev.Revision, 10), s.CreateTime, s.Action, pack, ver, st.Phase, s.User, desc,
		})
	}
	c.ui.Table(tbl)
	return 0
}

func (c *HistoryCommand) Flags() *flag.Sets {
	return c.flagSet(flagSetNomadClient, nil)
}

func (c *HistoryCommand) AutocompleteArgs() complete.Predictor { return complete.PredictAnything }
func (c *HistoryCommand) AutocompleteFlags() complete.Flags    { return c.Flags().Completions() }
func (c *HistoryCommand) Synopsis() string                     { return "List the revisions of a release" }
func (c *HistoryCommand) Help() string {
	c.Example = `
	# List the revisions of the release "redis"
	norca pack history redis
	`
	return formatHelp(`
	Usage: norca pack history <release> [options]

	List the revisions of a release, newest first: the action (install,
	upgrade, rollback), the pack version and the phase of every kept revision.

` + c.GetExample() + c.Flags().Help())
}

// RollbackCommand rolls a release back to an earlier revision.
type RollbackCommand struct {
	*baseCommand
	atomic  string
	timeout time.Duration
	detach  bool
	dryRun  bool
}

func (c *RollbackCommand) Run(args []string) int {
	c.cmdKey = "rollback"
	if err := c.Init(WithMinimumNArgs(1, args), WithMaximumNArgs(2, args), WithFlags(c.Flags()), WithNoConfig()); err != nil {
		c.ui.ErrorWithContext(err, ErrParsingArgsOrFlags)
		c.ui.Info(c.helpUsageMessage())
		return 1
	}
	req := &api.ReleaseRollbackRequest{Timeout: c.timeout, Wait: !c.detach, DryRun: c.dryRun}
	if len(c.args) == 2 {
		n, err := strconv.ParseUint(c.args[1], 10, 64)
		if err != nil || n == 0 {
			c.ui.ErrorWithContext(fmt.Errorf("invalid revision %q", c.args[1]), ErrParsingArgsOrFlags)
			return 1
		}
		req.Revision = n
	}
	opts := releaseOptions{atomic: c.atomic}
	var err error
	if req.Atomic, err = opts.atomicPtr(); err != nil {
		c.ui.ErrorWithContext(err, ErrParsingArgsOrFlags)
		return 1
	}
	client, ok := c.releaseClient()
	if !ok {
		return 1
	}
	name := c.args[0]
	resp, _, err := client.Releases().Rollback(name, req, nil)
	if err != nil {
		c.ui.ErrorWithContext(err, "rollback failed", "Release: "+name)
		return 1
	}
	printChanges(c.ui, resp.Changes)
	printWarnings(c.ui, resp.Warnings)
	if c.dryRun {
		c.ui.Success(fmt.Sprintf("Rollback of release %q planned as revision %d", name, resp.Revision))
		return 0
	}
	c.ui.Success(fmt.Sprintf("Release %q rolled back; deployed at revision %d", name, resp.Revision))
	return 0
}

func (c *RollbackCommand) Flags() *flag.Sets {
	return c.flagSet(flagSetNomadClient, func(set *flag.Sets) {
		f := set.NewSet("Rollback Options")
		f.StringVar(&flag.StringVar{
			Name:   "atomic",
			Target: &c.atomic,
			Usage:  `"true" or "false"; if not set, production namespaces are atomic.`,
		})
		f.DurationVar(&flag.DurationVar{
			Name: "timeout", Target: &c.timeout, Default: 5 * time.Minute,
			Usage: `How long the servers wait for deployments and hooks.`,
		})
		f.BoolVar(&flag.BoolVar{
			Name: "detach", Target: &c.detach,
			Usage: `Don't wait for the deployments of the jobs.`,
		})
		f.BoolVar(&flag.BoolVar{
			Name: "dry-run", Target: &c.dryRun,
			Usage: `Show the changes without rolling back.`,
		})
	})
}

func (c *RollbackCommand) AutocompleteArgs() complete.Predictor { return complete.PredictAnything }
func (c *RollbackCommand) AutocompleteFlags() complete.Flags    { return c.Flags().Completions() }
func (c *RollbackCommand) Synopsis() string                     { return "Roll a release back to an earlier revision" }
func (c *RollbackCommand) Help() string {
	c.Example = `
	# Roll the release "redis" back to the previous revision
	norca pack rollback redis

	# Roll back to revision 2
	norca pack rollback redis 2
	`
	return formatHelp(`
	Usage: norca pack rollback <release> [revision] [options]

	Apply the resources of an earlier revision of a release as a new
	revision. Without a revision, the revision deployed before the current
	one is restored.

` + c.GetExample() + c.Flags().Help())
}

// UninstallCommand uninstalls a release.
type UninstallCommand struct {
	*baseCommand
	orphan  bool
	noHooks bool
	timeout time.Duration
}

func (c *UninstallCommand) Run(args []string) int {
	c.cmdKey = "uninstall"
	if err := c.Init(WithExactArgs(1, args), WithFlags(c.Flags()), WithNoConfig()); err != nil {
		c.ui.ErrorWithContext(err, ErrParsingArgsOrFlags)
		c.ui.Info(c.helpUsageMessage())
		return 1
	}
	client, ok := c.releaseClient()
	if !ok {
		return 1
	}
	return c.uninstallRelease(client, c.args[0], c.orphan, c.noHooks, c.timeout)
}

// uninstallRelease uninstalls a release and reports the result.
func (c *baseCommand) uninstallRelease(client *api.Client, name string, orphan, noHooks bool, timeout time.Duration) int {
	opts := &api.ReleaseDeleteOptions{NoHooks: noHooks, Timeout: timeout}
	if orphan {
		opts.Propagation = api.ReleasePropagationOrphan
	}
	resp, _, err := client.Releases().Delete(name, opts, nil)
	if err != nil {
		c.ui.ErrorWithContext(err, "uninstall failed", "Release: "+name)
		return 1
	}
	printChanges(c.ui, resp.Changes)
	c.ui.Success(fmt.Sprintf("Release %q uninstalled", name))
	return 0
}

func (c *UninstallCommand) Flags() *flag.Sets {
	return c.flagSet(flagSetNomadClient, func(set *flag.Sets) {
		f := set.NewSet("Uninstall Options")
		f.BoolVar(&flag.BoolVar{
			Name: "orphan", Target: &c.orphan,
			Usage: `Keep the resources of the release; only the release and its
					revisions are deleted.`,
		})
		f.BoolVar(&flag.BoolVar{
			Name: "no-hooks", Target: &c.noHooks,
			Usage: `Don't run the pre-delete and post-delete hooks.`,
		})
		f.DurationVar(&flag.DurationVar{
			Name: "timeout", Target: &c.timeout, Default: 5 * time.Minute,
			Usage: `How long the servers wait for the hooks.`,
		})
	})
}

func (c *UninstallCommand) AutocompleteArgs() complete.Predictor { return complete.PredictAnything }
func (c *UninstallCommand) AutocompleteFlags() complete.Flags    { return c.Flags().Completions() }
func (c *UninstallCommand) Synopsis() string                     { return "Uninstall a release and delete its resources" }
func (c *UninstallCommand) Help() string {
	c.Example = `
	# Uninstall the release "redis" and delete its resources in reverse order
	norca pack uninstall redis

	# Delete only the release record and keep the resources
	norca pack uninstall redis --orphan
	`
	return formatHelp(`
	Usage: norca pack uninstall <release> [options]

	Uninstall a release: run its pre-delete hooks, delete its resources in
	the reverse of the application order, run its post-delete hooks, and
	delete the release with its revisions.

` + c.GetExample() + c.Flags().Help())
}

// ReleaseTestCommand runs the test hooks of a release.
type ReleaseTestCommand struct {
	*baseCommand
	timeout time.Duration
}

func (c *ReleaseTestCommand) Run(args []string) int {
	c.cmdKey = "test"
	if err := c.Init(WithExactArgs(1, args), WithFlags(c.Flags()), WithNoConfig()); err != nil {
		c.ui.ErrorWithContext(err, ErrParsingArgsOrFlags)
		c.ui.Info(c.helpUsageMessage())
		return 1
	}
	client, ok := c.releaseClient()
	if !ok {
		return 1
	}
	result, _, err := client.Releases().Test(c.args[0], c.timeout, nil)
	if err != nil {
		c.ui.ErrorWithContext(err, "test failed", "Release: "+c.args[0])
		return 1
	}
	if len(result.Hooks) == 0 {
		c.ui.Warning(fmt.Sprintf("release %q has no test hooks", c.args[0]))
		return 0
	}
	tbl := terminal.NewTable("Job", "Namespace", "Status", "Message")
	for _, h := range result.Hooks {
		tbl.Rows = append(tbl.Rows, []string{h.Job, h.Namespace, h.Status, h.Message})
	}
	c.ui.Table(tbl)
	if !result.Succeeded {
		c.ui.ErrorWithContext(fmt.Errorf("tests of release %q failed", c.args[0]), "test failed")
		return 1
	}
	c.ui.Success(fmt.Sprintf("Tests of release %q succeeded", c.args[0]))
	return 0
}

func (c *ReleaseTestCommand) Flags() *flag.Sets {
	return c.flagSet(flagSetNomadClient, func(set *flag.Sets) {
		f := set.NewSet("Test Options")
		f.DurationVar(&flag.DurationVar{
			Name: "timeout", Target: &c.timeout, Default: 5 * time.Minute,
			Usage: `How long the servers wait for the test hooks.`,
		})
	})
}

func (c *ReleaseTestCommand) AutocompleteArgs() complete.Predictor { return complete.PredictAnything }
func (c *ReleaseTestCommand) AutocompleteFlags() complete.Flags    { return c.Flags().Completions() }
func (c *ReleaseTestCommand) Synopsis() string                     { return "Run the test hooks of a release" }
func (c *ReleaseTestCommand) Help() string {
	c.Example = `
	# Run the test hooks of the release "redis"
	norca pack test redis
	`
	return formatHelp(`
	Usage: norca pack test <release> [options]

	Run the hook jobs with the "test" role (job meta pack.hook = "test") of
	the deployed revision of a release, and record the result in the
	release.

` + c.GetExample() + c.Flags().Help())
}

// DiffCommand compares a release with the live state of its resources.
type DiffCommand struct {
	*baseCommand
	live bool
}

func (c *DiffCommand) Run(args []string) int {
	c.cmdKey = "diff"
	if err := c.Init(WithExactArgs(1, args), WithFlags(c.Flags()), WithNoConfig()); err != nil {
		c.ui.ErrorWithContext(err, ErrParsingArgsOrFlags)
		c.ui.Info(c.helpUsageMessage())
		return 1
	}
	if !c.live {
		c.ui.ErrorWithContext(fmt.Errorf("only --live is supported"), ErrParsingArgsOrFlags)
		return 1
	}
	client, ok := c.releaseClient()
	if !ok {
		return 1
	}
	drift, _, err := client.Releases().Drift(c.args[0], nil)
	if err != nil {
		c.ui.ErrorWithContext(err, "drift check failed", "Release: "+c.args[0])
		return 1
	}
	if !drift.Drifted {
		c.ui.Success(fmt.Sprintf("Release %q matches revision %d", c.args[0], drift.Revision))
		return 0
	}
	tbl := terminal.NewTable("Type", "Namespace", "Name", "Drift")
	for _, e := range drift.Resources {
		tbl.Rows = append(tbl.Rows, []string{e.Type, e.Namespace, e.Name, e.Reason})
	}
	c.ui.Table(tbl)
	c.ui.Warning(fmt.Sprintf("Release %q drifted from revision %d", c.args[0], drift.Revision))
	return 2
}

func (c *DiffCommand) Flags() *flag.Sets {
	return c.flagSet(flagSetNomadClient, func(set *flag.Sets) {
		f := set.NewSet("Diff Options")
		f.BoolVar(&flag.BoolVar{
			Name: "live", Target: &c.live,
			Usage: `Compare the deployed revision with the live state of the
					resources (drift detection).`,
		})
	})
}

func (c *DiffCommand) AutocompleteArgs() complete.Predictor { return complete.PredictAnything }
func (c *DiffCommand) AutocompleteFlags() complete.Flags    { return c.Flags().Completions() }
func (c *DiffCommand) Synopsis() string                     { return "Show the drift of a release from its live resources" }
func (c *DiffCommand) Help() string {
	c.Example = `
	# Check whether the resources of the release "redis" were changed by hand
	norca pack diff redis --live
	`
	return formatHelp(`
	Usage: norca pack diff <release> --live [options]

	Compare the deployed revision of a release with the live state of its
	resources. Resources that were deleted or changed outside the release are
	listed, the Drifted condition of the release is updated, and the command
	exits with 2. Drift is never corrected automatically.

` + c.GetExample() + c.Flags().Help())
}
