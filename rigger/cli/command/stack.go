// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/hashicorp/hcl/v2"
	"github.com/spf13/cobra"

	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/cli/internal/atomicfile"
	"github.com/bacanoo-io/norca/rigger/source"
)

// stackFlags are the flags of rigger stack <command>.
type stackFlags struct {
	dir string
	// address is the source of a published stack, version its constraint
	// (design 7.5); empty for a stack in a directory.
	address string
	version string
	key     string
	files   []string
	vars    []string
	json    bool
	prune   bool
	adopt   bool
	upgrade bool
	region  string
	// server sends the command to the servers; namespace is the namespace
	// of a published stack there (a stack in a directory names its own).
	server    bool
	namespace string
}

// The commands of rigger stack.
const (
	cmdPlan    = "plan"
	cmdApply   = "apply"
	cmdStatus  = "status"
	cmdDestroy = "destroy"
	cmdSync    = "sync"
)

// The flags of the stack commands, and of install and module render.
const (
	valuesFlag    = "values"
	varFlag       = "var"
	regionFlag    = "region"
	namespaceFlag = "namespace"
	versionFlag   = "version"
	serverFlag    = "server"
	adoptFlag     = "adopt"
	keyFlag       = "key"
	pruneFlag     = "prune"
)

// stackDoc is the help of a stack command and the flags it shows; it
// parses the others too, hidden, as each command took them all before.
type stackDoc struct {
	use, short, long, example string
	flags                     []string
}

// stackDocs are the stack commands, in the order of the help.
var stackDocs = []stackDoc{
	{
		use:   "sync [dir]",
		short: "Resolve, fetch and lock the versions of the modules of a stack",
		long: `Resolve the versions of the modules of the stack in dir (default: the
current directory), fetch and verify them, and write rigger.lock.hcl next to
the stack. A module keeps the version its lock pins while that version meets
its constraint; --upgrade takes the newest version that meets it.`,
		example: `  rigger stack sync
  rigger stack sync stacks/platform --upgrade`,
		flags: []string{"upgrade"},
	},
	{
		use:   "validate [dir|source]",
		short: "Evaluate a stack without a cluster",
		long: `Evaluate a stack without a cluster: with the values of --values and --var,
or, without them, with each file of values under its values/ directory (or
none, when it has none).`,
		example: `  rigger stack validate
  rigger stack validate stacks/platform --values values/dev.vars.hcl
  rigger stack validate acme/data-platform --version "~> 1.2"`,
		flags: []string{valuesFlag, varFlag, versionFlag},
	},
	{
		use:   "plan [dir|source]",
		short: "Show what an apply would change",
		long: `Plan a stack against its cluster: the releases an apply installs, upgrades,
keeps or removes, with the changes of their resources. The command exits
with 2 when the plan has changes, with 0 when it has none.`,
		example: `  rigger stack plan --values values/prod.vars.hcl
  rigger stack plan acme/data-platform --version "~> 1.2" --values my.vars.hcl --json
  rigger stack plan --server`,
		flags: []string{valuesFlag, varFlag, jsonFlag, pruneFlag, adoptFlag, regionFlag, versionFlag, serverFlag, namespaceFlag},
	},
	{
		use:   "apply [dir|source]",
		short: "Plan a stack and apply it, release after release",
		long: `Plan a stack and apply the plan, release after release. --prune uninstalls
the releases the stack owns and no longer has; --adopt takes over releases
another package manager applied or another stack owns.

With --server the servers apply it as an operation, whose log the command
follows: Ctrl-C cancels the operation before its next release; a second
Ctrl-C leaves it running (rigger operation follow).`,
		example: `  rigger stack apply --values values/prod.vars.hcl
  rigger stack apply stacks/platform -f values/prod.vars.hcl --var replicas=3 --prune
  rigger stack apply acme/data-platform --version "~> 1.2" --server --namespace data`,
		flags: []string{valuesFlag, varFlag, jsonFlag, pruneFlag, adoptFlag, regionFlag, versionFlag, serverFlag, namespaceFlag},
	},
	{
		use:   "status [dir|source]",
		short: "Show the releases of a stack, their revision and drift",
		long: `Show the releases the stack owns in its cluster: their revision, their
status, the module and version they run, and whether their resources drifted
from what was applied.`,
		example: `  rigger stack status
  rigger stack status stacks/platform --json`,
		flags: []string{jsonFlag, regionFlag, versionFlag, serverFlag, namespaceFlag},
	},
	{
		use:   "destroy [dir|source]",
		short: "Uninstall the releases of a stack",
		long: `Uninstall the releases the stack owns in its cluster. With --server the
servers do it, as an operation whose log the command follows.`,
		example: `  rigger stack destroy
  rigger stack destroy acme/data-platform --server --namespace data`,
		flags: []string{regionFlag, versionFlag, serverFlag, namespaceFlag},
	},
	{
		use:   "publish [dir]",
		short: "Sign a stack, commit the signature and tag its version",
		long: `Validate a stack with metadata.hcl and its lock file, sign it with the
private key of --key (its password in RIGGER_KEY_PASSWORD), commit the
signature and tag the version in its git repository. Nothing is pushed.`,
		example: `  rigger stack publish --key release.key`,
		flags:   []string{keyFlag},
	},
}

// newStackCommand is rigger stack and its commands.
func (a *app) newStackCommand() *cobra.Command {
	sub := make([]*cobra.Command, 0, len(stackDocs))
	for _, doc := range stackDocs {
		sub = append(sub, a.newStackSubcommand(doc))
	}

	return newGroup(&cobra.Command{
		Use:   "stack",
		Short: "Work with a stack: sync, validate, plan, apply, status, destroy, publish",
		Long: `The commands of a stack, a set of modules installed together, in the
directory dir (default: the current one).

Every stack command but sync and publish takes, instead of a directory, a
published stack: <namespace>/<name> of the global registry or a git address,
with --version "~> 1.2".

With --server (or RIGGER_SERVER=1), plan, apply, status and destroy go to the
servers of the cluster, which fetch and evaluate; an apply or a destroy runs
there as an operation whose log the command follows.`,
	}, sub...)
}

// newStackSubcommand is one command of rigger stack.
func (a *app) newStackSubcommand(doc stackDoc) *cobra.Command {
	f := &stackFlags{}
	name, _, _ := strings.Cut(doc.use, " ")

	cmd := &cobra.Command{
		Use: doc.use, Short: doc.short, Long: doc.long, Example: doc.example,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := f.setDir(args); err != nil {
				return err
			}

			if f.server && isLocalStackCommand(name) {
				return errServerOnly
			}

			if f.address != "" && (name == cmdSync || name == cmdPublish) {
				return fmt.Errorf("stack %s works on a stack in a directory", name)
			}

			fn := stackCommand(a.logger, a.stdout, a.stderr, name, f)
			if fn == nil {
				return fmt.Errorf("unknown stack command %q", name)
			}

			return fn(cmd.Context())
		},
	}

	flags := cmd.Flags()
	flags.StringArrayVarP(&f.files, valuesFlag, "f", nil, "a `file` of values of the stack (values/dev.vars.hcl); may be given more than once")
	flags.StringArrayVar(&f.vars, varFlag, nil, "a value of the stack, `name=value`; may be given more than once"+varSyntax)
	flags.BoolVar(&f.json, jsonFlag, false, "write the plan or the status as JSON")
	flags.BoolVar(&f.prune, pruneFlag, false, "uninstall the releases the stack owns and no longer has")
	flags.BoolVar(&f.adopt, adoptFlag, false, "take over releases another package manager applied or another stack owns")
	flags.StringVar(&f.region, regionFlag, "", "the region to talk to (default: the region of the stack)")
	flags.BoolVar(&f.upgrade, "upgrade", false, "take the newest version that meets each constraint, not the locked one")
	flags.StringVar(&f.version, versionFlag, "", `a published stack: the version `+"`constraint`"+` ("~> 1.2"); none takes the newest`)
	flags.BoolVar(&f.server, serverFlag, serverDefault() && !isLocalStackCommand(name), serverUsage)
	flags.StringVar(&f.namespace, namespaceFlag, "", "with --server, a published stack: the namespace of the stack (default: NORCA_NAMESPACE)")
	flags.StringVar(&f.key, keyFlag, "", "the private `key` the version is signed with (password in RIGGER_KEY_PASSWORD)")

	showOnly(cmd, doc.flags...)

	return cmd
}

// isLocalStackCommand reports whether a stack command works here alone:
// --server is an error for it, and RIGGER_SERVER doesn't apply to it.
func isLocalStackCommand(cmd string) bool {
	return cmd == cmdSync || cmd == cmdPublish || cmd == cmdValidate
}

// stackCommand is the function of rigger stack <cmd>: here, or with
// --server on the servers; nil for an unknown command.
func stackCommand(logger *slog.Logger, stdout, stderr io.Writer, cmd string, f *stackFlags) func(context.Context) error {
	local := map[string]func(context.Context) error{
		cmdPublish:  func(ctx context.Context) error { return publishStack(ctx, logger, stdout, stderr, f) },
		cmdValidate: func(ctx context.Context) error { return validateStack(ctx, logger, stdout, stderr, f) },
		cmdSync:     func(ctx context.Context) error { return syncStack(ctx, logger, stdout, stderr, f) },
		cmdPlan:     func(ctx context.Context) error { return planApplyStack(ctx, logger, stdout, stderr, f, false) },
		cmdApply:    func(ctx context.Context) error { return planApplyStack(ctx, logger, stdout, stderr, f, true) },
		cmdStatus:   func(ctx context.Context) error { return stackStatus(ctx, logger, stdout, stderr, f) },
		cmdDestroy:  func(ctx context.Context) error { return destroyStack(ctx, logger, stdout, stderr, f) },
	}

	server := map[string]func(context.Context) error{
		cmdPlan:    func(ctx context.Context) error { return serverPlan(ctx, logger, stdout, stderr, f) },
		cmdApply:   func(ctx context.Context) error { return serverOperation(ctx, logger, stdout, stderr, f, false) },
		cmdStatus:  func(ctx context.Context) error { return serverStatus(ctx, logger, stdout, stderr, f) },
		cmdDestroy: func(ctx context.Context) error { return serverOperation(ctx, logger, stdout, stderr, f, true) },
	}

	if f.server {
		return server[cmd]
	}

	return local[cmd]
}

// setDir takes the one directory or source of a stack command, the current
// directory without one. An argument that names no path and reads as an
// address is the source of a published stack.
func (f *stackFlags) setDir(args []string) error {
	switch len(args) {
	case 0:
		f.dir = "."
	case 1:
		f.dir = args[0]
	default:
		return errOneStackDir
	}

	if _, err := os.Stat(f.dir); err != nil && isStackAddress(f.dir) {
		f.address = f.dir
	}

	return nil
}

var errOneStackDir = errors.New("a stack command takes one directory or the source of a published stack")

// isStackAddress reports whether arg is the source of a published stack
// rather than a directory: <namespace>/<name> or a git address.
func isStackAddress(arg string) bool {
	if strings.HasPrefix(arg, ".") || filepath.IsAbs(arg) || !strings.Contains(arg, "/") {
		return false
	}

	req, err := source.Parse(arg, nil)

	return err == nil && (req.Kind == source.KindGlobal || req.Kind == source.KindGit)
}

// loadStack loads the stack in dir, with the root it reads its modules
// from, and the values of the flags (files relative to the working
// directory).
func loadStack(ctx context.Context, eng *rigger.Engine, f *stackFlags) (*rigger.Stack, rigger.Values, hcl.Diagnostics, error) {
	if f.address != "" {
		s, diags := eng.FetchStack(ctx, f.address, f.version)
		if diags.HasErrors() {
			return s, nil, diags, nil
		}

		values, moreDiags := readValues(&renderFlags{files: f.files, vars: f.vars}, s.FormSchema())

		return s, values, append(diags, moreDiags...), nil
	}

	info, err := os.Stat(f.dir)
	if err != nil {
		return nil, nil, nil, err
	}

	if !info.IsDir() {
		return nil, nil, nil, fmt.Errorf("%s is not a directory", f.dir)
	}

	root, rel, err := moduleRoot(f.dir)
	if err != nil {
		return nil, nil, nil, err
	}

	s, diags := eng.LoadStack(ctx, os.DirFS(root), rel)
	if diags.HasErrors() {
		return s, nil, diags, nil
	}

	values, moreDiags := readValues(&renderFlags{files: f.files, vars: f.vars}, s.FormSchema())

	return s, values, append(diags, moreDiags...), nil
}

// stackClusterEngine connects to the cluster of a stack.
func stackClusterEngine(ctx context.Context, logger *slog.Logger, f *stackFlags) (*rigger.Engine, *rigger.Stack, rigger.Values, hcl.Diagnostics, error) {
	offline, err := offlineEngine(logger)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	s, values, diags, err := loadStack(ctx, offline, f)
	if err != nil || diags.HasErrors() {
		return nil, s, values, diags, err
	}

	region := f.region
	if region == "" {
		region = s.Region()
	}

	client, err := newClient(region)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	eng, err := clusterEngine(logger, client, s.Namespace())
	if err != nil {
		return nil, nil, nil, nil, err
	}

	// Loaded again by the engine of the cluster, which evaluates it.
	s, values, diags, err = loadStack(ctx, eng, f)

	return eng, s, values, diags, err
}

// validateStack evaluates a stack without a cluster: with the values of
// the flags, or with each value file under values/ when none are given.
func validateStack(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, f *stackFlags) error {
	eng, err := offlineEngine(logger)
	if err != nil {
		return err
	}

	s, values, diags, err := loadStack(ctx, eng, f)
	if err != nil {
		return err
	}

	if diags.HasErrors() {
		return reportDiags(stderr, f.dir, diags)
	}

	if len(f.files) > 0 || len(f.vars) > 0 {
		diags = append(diags, eng.ValidateStack(ctx, s, values)...)
	} else {
		// Without -f and -var the values are those of the environment
		// only: they go over each value file, as they do in a plan.
		moreDiags, err := validateValueFiles(ctx, eng, s, values)
		if err != nil {
			return err
		}

		diags = append(diags, moreDiags...)
	}

	if len(diags) > 0 {
		if err := writeDiags(stderr, f.dir, diags); err != nil {
			return err
		}
	}

	if diags.HasErrors() {
		return errReported
	}

	_, err = fmt.Fprintf(stdout, "stack %s in %s is valid\n", s.Name(), f.dir)

	return err
}

// validateValueFiles evaluates a stack with each value file under its
// values/, or without one when it has none. The values of over win over
// those of a file; nil for none.
func validateValueFiles(ctx context.Context, eng *rigger.Engine, s *rigger.Stack, over rigger.Values) (hcl.Diagnostics, error) {
	files := s.ValueFiles()
	if len(files) == 0 {
		return eng.ValidateStack(ctx, s, over), nil
	}

	var diags hcl.Diagnostics

	for _, p := range files {
		src, err := s.ReadValueFile(p)
		if err != nil {
			return nil, err
		}

		vals, moreDiags := rigger.ParseValues(p, src)
		diags = append(diags, moreDiags...)

		if !moreDiags.HasErrors() {
			maps.Copy(vals, over)

			diags = append(diags, eng.ValidateStack(ctx, s, vals)...)
		}
	}

	return diags, nil
}

// planApplyStack plans a stack and, with apply, applies the plan. A plan
// with changes exits with 2.
func planApplyStack(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, f *stackFlags, apply bool) error {
	eng, s, values, diags, err := stackClusterEngine(ctx, logger, f)
	if err != nil {
		return err
	}

	if diags.HasErrors() {
		return reportDiags(stderr, f.dir, diags)
	}

	warnings := diags

	plan, diags := eng.PlanStack(ctx, s, values)
	if diags.HasErrors() {
		return reportDiags(stderr, f.dir, append(warnings, diags...))
	}

	if err := writeWarnings(stderr, f.dir, append(warnings, diags...)); err != nil {
		return err
	}

	if err := writePlan(stdout, plan, f.json, f.prune); err != nil {
		return err
	}

	if !apply {
		if plan.HasChanges() {
			return errChanges
		}

		return nil
	}

	outcomes, diags := eng.ApplyStack(ctx, plan, rigger.ApplyOptions{Prune: f.prune, Adopt: f.adopt})
	if err := writeOutcomes(stdout, outcomes); err != nil {
		return err
	}

	return checkDiags(stderr, f.dir, diags)
}

// planSigns are the signs of the actions of a plan.
var planSigns = map[rigger.ReleaseAction]string{
	rigger.ActionInstall: "+", rigger.ActionUpgrade: "~", rigger.ActionUnchanged: "=",
	rigger.ActionRemove: "-", rigger.ActionDisabled: " ",
}

// writePlan writes a plan for a person, or as JSON.
func writePlan(w io.Writer, plan *rigger.StackPlan, asJSON, prune bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")

		return enc.Encode(plan)
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "stack %s, region %s\n", plan.Stack, plan.Region)

	for _, r := range plan.Releases {
		action := string(r.Action)
		if r.Action == rigger.ActionRemove && !prune {
			action += " (with -prune)"
		}

		if r.TakesOver != "" {
			action += " (takes it over from " + r.TakesOver + ", with -adopt)"
		}

		// A disabled release the stack installed before stays as it is.
		if r.Action == rigger.ActionDisabled && r.Revision > 0 {
			action += fmt.Sprintf(" (stays installed, revision %d)", r.Revision)
		}

		_, _ = fmt.Fprintf(tw, "  %s %s\t%s\t%s\t%s %s\n", planSigns[r.Action], r.Release.Name, r.Release.Namespace, action, r.Module.Name, r.Module.Version)

		for _, c := range r.Changes {
			_, _ = fmt.Fprintf(tw, "      %s %s: %s\t\t\t\n", c.Kind, c.Name, c.Action)
		}
	}

	for _, s := range plan.Secrets {
		_, _ = fmt.Fprintf(tw, "  secret %s\t\t%s\t\n", s.Path, s.Action)
	}

	if len(plan.Generate) > 0 {
		_, _ = fmt.Fprintf(tw, "  generate %s\t\t\t\n", strings.Join(plan.Generate, ", "))
	}

	if !plan.HasChanges() {
		_, _ = fmt.Fprintln(tw, "nothing to do")
	}

	return tw.Flush()
}

// writeOutcomes writes what an apply did.
func writeOutcomes(w io.Writer, outcomes []rigger.ReleaseOutcome) error {
	for _, o := range outcomes {
		if !o.Skipped && (o.Action == rigger.ActionUnchanged || o.Action == rigger.ActionDisabled) {
			continue
		}

		line := outcomeLine(string(o.Action), o.Release.Namespace, o.Release.Name, o.Revision, o.Skipped, o.Failed)
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}

	return nil
}

// stackStatus writes the releases a stack owns, their revision and drift.
func stackStatus(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, f *stackFlags) error {
	eng, s, _, diags, err := stackClusterEngine(ctx, logger, f)
	if err != nil {
		return err
	}

	if diags.HasErrors() {
		return reportDiags(stderr, f.dir, diags)
	}

	status, err := eng.StackStatus(ctx, s)
	if err != nil {
		return err
	}

	return writeStatus(stdout, status, f.json)
}

// writeStatus writes the releases of a stack for a person, or as JSON.
func writeStatus(stdout io.Writer, status []rigger.ReleaseStatus, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")

		return enc.Encode(status)
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)

	for _, st := range status {
		rev := st.Revision
		drift := "in sync"

		if len(st.Drift) > 0 {
			drift = fmt.Sprintf("%d drifted", len(st.Drift))
		}

		_, _ = fmt.Fprintf(tw, "%s\t%s\trevision %d\t%s\t%s %s\t%s\n", rev.Release.Name, rev.Release.Namespace, rev.Number, rev.Status,
			rev.Module.Name, rev.Module.Version, drift)
	}

	return tw.Flush()
}

// destroyStack uninstalls the releases a stack owns.
func destroyStack(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, f *stackFlags) error {
	eng, s, _, diags, err := stackClusterEngine(ctx, logger, f)
	if err != nil {
		return err
	}

	if diags.HasErrors() {
		return reportDiags(stderr, f.dir, diags)
	}

	removed, err := eng.DestroyStack(ctx, s)
	for _, ref := range removed {
		if _, werr := fmt.Fprintf(stdout, "uninstalled %s/%s\n", ref.Namespace, ref.Name); werr != nil {
			return werr
		}
	}

	return err
}

// syncStack resolves the versions of the modules of a stack, fetches and
// verifies them, and writes the lock file next to the stack (design 8.3).
func syncStack(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, f *stackFlags) error {
	eng, err := offlineEngine(logger)
	if err != nil {
		return err
	}

	root, rel, err := moduleRoot(f.dir)
	if err != nil {
		return err
	}

	_, lock, diags := eng.SyncStack(ctx, os.DirFS(root), rel, f.upgrade)
	if diags.HasErrors() {
		return reportDiags(stderr, f.dir, diags)
	}

	if err := writeWarnings(stderr, f.dir, diags); err != nil {
		return err
	}

	p := filepath.Join(f.dir, lockFile)

	old, _ := os.ReadFile(filepath.Clean(p))
	if bytes.Equal(old, lock) {
		_, err := fmt.Fprintf(stdout, "%s is up to date\n", p)

		return err
	}

	// Never half written: nothing reads half a lock file, and a sync that
	// is cut would lose the versions the stack was locked to. The file is
	// kept in version control, not secret.
	if err := atomicfile.Write(p, lock, 0o644); err != nil {
		return err
	}

	_, err = fmt.Fprintf(stdout, "wrote %s\n", p)

	return err
}

// lockFile is the name of the lock file of a stack.
const lockFile = "rigger.lock.hcl"
