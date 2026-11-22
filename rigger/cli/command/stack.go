// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/hashicorp/hcl/v2"

	rigger "github.com/bacanoo-io/norca/rigger"
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
	files   listFlag
	vars    listFlag
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

// runStack runs rigger stack <command> [dir] [flags].
func runStack(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, args []string) int {
	if len(args) == 0 {
		warn(stderr, "stack needs a command: sync, validate, plan, apply, status, destroy or publish")

		return exitError
	}

	cmd := args[0]

	f, err := parseStackFlags(stderr, cmd, args[1:])
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			warn(stderr, "%v", err)
		}

		return exitError
	}

	if f.server && (cmd == cmdSync || cmd == cmdPublish || cmd == cmdValidate) {
		warn(stderr, "%v", errServerOnly)

		return exitError
	}

	if f.address != "" && (cmd == cmdSync || cmd == cmdPublish) {
		warn(stderr, "stack %s works on a stack in a directory", cmd)

		return exitError
	}

	fn := stackCommand(logger, stdout, stderr, cmd, f)
	if fn == nil {
		warn(stderr, "unknown stack command %q", cmd)

		return exitError
	}

	return run(ctx, logger, stderr, "stack."+cmd, fn)
}

// stackCommand is the function of rigger stack <cmd>: here, or with
// -server on the servers; nil for an unknown command.
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

func parseStackFlags(stderr io.Writer, cmd string, args []string) (*stackFlags, error) {
	f := &stackFlags{dir: "."}

	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		f.dir, args = args[0], args[1:]
	}

	flags := flag.NewFlagSet("rigger stack "+cmd, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Var(&f.files, "f", "a file of values of the stack (values/dev.vars.hcl); may be given more than once")
	flags.Var(&f.vars, "var", "a value of the stack, name=value; may be given more than once")
	flags.BoolVar(&f.json, "json", false, "write the plan or the status as JSON")
	flags.BoolVar(&f.prune, "prune", false, "apply: uninstall the releases the stack owns and no longer has")
	flags.BoolVar(&f.adopt, "adopt", false, "apply: take over releases the pack manager applied or another stack owns")
	flags.StringVar(&f.region, "region", "", "the region to talk to (default: the region of the stack)")
	flags.BoolVar(&f.upgrade, "upgrade", false, "sync: take the newest version that meets each constraint, not the locked one")
	flags.StringVar(&f.version, "version", "", "a published stack: the version constraint (\"~> 1.2\"); none takes the newest")
	flags.BoolVar(&f.server, "server", serverDefault(), serverUsage)
	flags.StringVar(&f.namespace, "namespace", "", "-server, a published stack: the namespace of the stack (default: NORCA_NAMESPACE)")
	flags.StringVar(&f.key, "key", "", "publish: the private key the version is signed with (password in RIGGER_KEY_PASSWORD)")

	if err := flags.Parse(args); err != nil {
		return nil, err
	}

	switch flags.NArg() {
	case 0:
	case 1:
		if f.dir != "." {
			return nil, errOneStackDir
		}

		f.dir = flags.Arg(0)
	default:
		return nil, errOneStackDir
	}

	if _, err := os.Stat(f.dir); err != nil && isStackAddress(f.dir) {
		f.address = f.dir
	}

	return f, nil
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

		values, moreDiags := readValues(&renderFlags{files: f.files, vars: f.vars})

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

	values, moreDiags := readValues(&renderFlags{files: f.files, vars: f.vars})

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

	if len(f.files) > 0 || len(f.vars) > 0 || len(s.ValueFiles()) == 0 {
		diags = append(diags, eng.ValidateStack(ctx, s, values)...)
	} else {
		for _, p := range s.ValueFiles() {
			src, err := s.ReadValueFile(p)
			if err != nil {
				return err
			}

			vals, moreDiags := rigger.ParseValues(p, src)
			diags = append(diags, moreDiags...)

			if !moreDiags.HasErrors() {
				diags = append(diags, eng.ValidateStack(ctx, s, vals)...)
			}
		}
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

	if diags.HasErrors() {
		return reportDiags(stderr, f.dir, diags)
	}

	return nil
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
		status := "done"

		switch {
		case o.Skipped:
			status = "skipped"
		case o.Action == rigger.ActionUnchanged || o.Action == rigger.ActionDisabled:
			continue
		}

		if _, err := fmt.Fprintf(w, "%s %s/%s: %s (revision %d)\n", o.Action, o.Release.Namespace, o.Release.Name, status, o.Revision); err != nil {
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

	if err := os.WriteFile(p, lock, 0o644); err != nil { //nolint:gosec // G306: the lock file is kept in version control, not secret
		return err
	}

	_, err = fmt.Fprintf(stdout, "wrote %s\n", p)

	return err
}

// lockFile is the name of the lock file of a stack.
const lockFile = "rigger.lock.hcl"
