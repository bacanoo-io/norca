// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/hashicorp/hcl/v2/hclwrite"

	"github.com/bacanoo-io/norca/norca/api"

	rigger "github.com/bacanoo-io/norca/rigger"
)

// The server mode (design/rig-integration.md 4.6c): with -server, or
// RIGGER_SERVER=1, the stack commands, install and bundle import are sent
// to the servers of the cluster, which fetch and evaluate; an apply or a
// destroy runs there as an operation whose log the command follows. Without
// it, rigger evaluates here and talks to the cluster through its API.

// serverEnv turns the server mode on by default.
const serverEnv = "RIGGER_SERVER"

// serverDefault is the default of -server where it works: RIGGER_SERVER
// set to a true value (1, true).
func serverDefault() bool {
	on, err := strconv.ParseBool(os.Getenv(serverEnv))

	return err == nil && on
}

const serverUsage = "send the command to the servers of the cluster, which evaluate it (default: RIGGER_SERVER)"

// errServerOnly is the error of -server on a command the servers don't run.
var errServerOnly = errors.New("-server works with stack plan, apply, status and destroy, install and bundle import")

// errDetached is the result of a command that stopped following an
// operation that goes on.
var errDetached = errors.New("detached")

// valuesHCL writes values as a file of values: one attribute per variable,
// by name.
func valuesHCL(values rigger.Values) string {
	f := hclwrite.NewEmptyFile()
	body := f.Body()

	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}

	slices.Sort(names)

	for _, name := range names {
		body.SetAttributeValue(name, values[name].Value)
	}

	return string(f.Bytes())
}

// serverStack is a stack request of the flags: the files of a stack in a
// directory, or the source of a published one, with the values of the
// flags, and the client of its region in its namespace.
func serverStack(ctx context.Context, logger *slog.Logger, stderr io.Writer, f *stackFlags) (*api.Client, *api.RiggerStackRequest, error) {
	req := &api.RiggerStackRequest{Source: f.address, Version: f.version, Prune: f.prune, Adopt: f.adopt}
	namespace, region := f.namespace, f.region

	if f.address == "" {
		s, err := localStack(ctx, logger, stderr, f)
		if err != nil {
			return nil, nil, err
		}

		if req.Files, err = s.Archive(); err != nil {
			return nil, nil, err
		}

		namespace = s.Namespace()
		region = cmp.Or(region, s.Region())
	}

	values, diags := readValues(&renderFlags{files: f.files, vars: f.vars})
	if diags.HasErrors() {
		return nil, nil, reportDiags(stderr, f.dir, diags)
	}

	if len(values) > 0 {
		req.Values = valuesHCL(values)
	}

	client, err := newClient(region)
	if err != nil {
		return nil, nil, err
	}

	if namespace != "" {
		client.SetNamespace(namespace)
	}

	return client, req, nil
}

// localStack loads the stack in the directory of the flags, without a
// cluster: what is sent to the servers.
func localStack(ctx context.Context, logger *slog.Logger, stderr io.Writer, f *stackFlags) (*rigger.Stack, error) {
	eng, err := offlineEngine(logger)
	if err != nil {
		return nil, err
	}

	s, _, diags, err := loadStack(ctx, eng, f)
	if err != nil {
		return nil, err
	}

	if diags.HasErrors() {
		return nil, reportDiags(stderr, f.dir, diags)
	}

	return s, nil
}

// serverWarnings writes the warnings the servers return.
func serverWarnings(w io.Writer, warnings []string) {
	for _, s := range warnings {
		warn(w, "%s", s)
	}
}

// serverPlan plans a stack on the servers. A plan with changes exits with 2.
func serverPlan(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, f *stackFlags) error {
	client, req, err := serverStack(ctx, logger, stderr, f)
	if err != nil {
		return err
	}

	out, _, err := client.Rigger().StackPlan(req, (&api.WriteOptions{}).WithContext(ctx))
	if err != nil {
		return err
	}

	serverWarnings(stderr, out.Warnings)

	var plan rigger.StackPlan
	if err := convertJSON(out.Plan, &plan); err != nil {
		return err
	}

	if err := writePlan(stdout, &plan, f.json, f.prune); err != nil {
		return err
	}

	if plan.HasChanges() {
		return errChanges
	}

	return nil
}

// serverStatus reads the status of a stack on the servers.
func serverStatus(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, f *stackFlags) error {
	client, req, err := serverStack(ctx, logger, stderr, f)
	if err != nil {
		return err
	}

	out, _, err := client.Rigger().StackStatus(req, (&api.WriteOptions{}).WithContext(ctx))
	if err != nil {
		return err
	}

	var status []rigger.ReleaseStatus
	if err := convertJSON(out.Releases, &status); err != nil {
		return err
	}

	return writeStatus(stdout, status, f.json)
}

// serverOperation starts an apply (or with destroy, a destroy) of a stack
// on the servers and follows it to its end.
func serverOperation(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, f *stackFlags, destroy bool) error {
	client, req, err := serverStack(ctx, logger, stderr, f)
	if err != nil {
		return err
	}

	start := client.Rigger().StackApply
	if destroy {
		start = client.Rigger().StackDestroy
	}

	op, _, err := start(req, (&api.WriteOptions{}).WithContext(ctx))
	if err != nil {
		return err
	}

	if _, err := fmt.Fprintf(stderr, "operation %s of the stack %s in %s\n", op.Name, op.Spec.Stack, op.Namespace); err != nil {
		return err
	}

	for _, r := range op.Status.Plan {
		if _, err := fmt.Fprintf(stdout, "  %s %s/%s\n", r.Action, r.Namespace, r.Release); err != nil {
			return err
		}
	}

	return followOperation(ctx, logger, stdout, stderr, client, op.Namespace, op.Name)
}

// operationLogWait is how long a read of the log of an operation waits for
// its next records.
const operationLogWait = 20 * time.Second

// followOperation writes the log of an operation through the logger as it
// comes, then what it did. Done ctx (a Ctrl-C) asks the servers to cancel
// the operation, which stops before its next release, and goes on
// following; a second Ctrl-C detaches and leaves it running.
func followOperation(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, client *api.Client, namespace, name string) error {
	follow, detach := context.WithCancel(context.Background())
	defer detach()

	go func() {
		select {
		case <-follow.Done():
			return
		case <-ctx.Done():
		}

		warn(stderr, "cancelling the operation %s: it stops before its next release; Ctrl-C again to leave it running", name)

		// The first Ctrl-C cancelled ctx; the second comes here.
		interrupt := make(chan os.Signal, 1)
		signal.Notify(interrupt, os.Interrupt)

		defer signal.Stop(interrupt)

		if _, _, err := client.Rigger().CancelOperation(name, (&api.WriteOptions{}).WithContext(follow)); err != nil {
			warn(stderr, "the operation %s could not be cancelled: %v", name, err)
		}

		select {
		case <-follow.Done():
		case <-interrupt:
			detach()
		}
	}()

	var after uint64

	for {
		page, _, err := client.Rigger().OperationLog(name, after, 0, operationLogWait, (&api.QueryOptions{}).WithContext(follow))
		if err != nil {
			if follow.Err() != nil {
				warn(stderr, "left the operation %s running: rigger operation follow %s -namespace %s", name, name, namespace)

				return errDetached
			}

			return err
		}

		for _, line := range page.Lines {
			logLine(follow, logger, line.Line)
			after = line.Seq
		}

		if page.Finished {
			break
		}
	}

	op, _, err := client.Rigger().Operation(name, nil)
	if err != nil {
		return err
	}

	return writeOperation(stdout, op)
}

// logLine writes a record of the log of an operation through logger, as
// the records of a command run here are written.
func logLine(ctx context.Context, logger *slog.Logger, line map[string]any) {
	level := slog.LevelInfo
	if s, ok := line[slog.LevelKey].(string); ok {
		_ = level.UnmarshalText([]byte(s))
	}

	if !logger.Enabled(ctx, level) {
		return
	}

	at := time.Now()

	if s, ok := line[slog.TimeKey].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			at = t
		}
	}

	msg, _ := line[slog.MessageKey].(string)
	r := slog.NewRecord(at, level, msg, 0)

	keys := make([]string, 0, len(line))
	for k := range line {
		if k != slog.LevelKey && k != slog.TimeKey && k != slog.MessageKey {
			keys = append(keys, k)
		}
	}

	slices.Sort(keys)

	for _, k := range keys {
		r.AddAttrs(logAttr(k, line[k]))
	}

	_ = logger.Handler().Handle(ctx, r)
}

// logAttr is an attribute of a record of the log of an operation: a whole
// number of JSON is an integer again (the depth of an operation, a
// revision), for the handlers that read it as one.
func logAttr(key string, v any) slog.Attr {
	if f, ok := v.(float64); ok && f == math.Trunc(f) && math.Abs(f) < 1<<53 {
		return slog.Int64(key, int64(f))
	}

	return slog.Any(key, v)
}

// writeOperation writes what an operation did, and returns its error when
// it did not succeed.
func writeOperation(w io.Writer, op *api.RiggerOperation) error {
	for _, o := range op.Status.Outcomes {
		status := "done"
		if o.Skipped {
			status = "skipped"
		}

		if o.Action == string(rigger.ActionUnchanged) || o.Action == string(rigger.ActionDisabled) {
			continue
		}

		if _, err := fmt.Fprintf(w, "%s %s/%s: %s (revision %d)\n", o.Action, o.Namespace, o.Release, status, o.Revision); err != nil {
			return err
		}
	}

	switch op.Status.Phase {
	case "Succeeded":
		return nil
	case "Cancelled":
		return fmt.Errorf("the operation %s was cancelled", op.Name)
	default:
		return fmt.Errorf("the operation %s is %s: %s", op.Name, op.Status.Phase, op.Status.Error)
	}
}

// serverInstall installs one release of the module in a directory through
// the servers.
func serverInstall(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, f *installFlags) error {
	eng, err := offlineEngine(logger)
	if err != nil {
		return err
	}

	m, diags, err := loadModule(ctx, eng, f.dir)
	if err != nil {
		return err
	}

	if diags.HasErrors() {
		return reportDiags(stderr, f.dir, diags)
	}

	values, diags := readValues(&f.renderFlags)
	if diags.HasErrors() {
		return reportDiags(stderr, f.dir, diags)
	}

	files, err := m.Archive()
	if err != nil {
		return err
	}

	client, err := newClient(f.region)
	if err != nil {
		return err
	}

	client.SetNamespace(f.namespace)

	req := &api.RiggerInstallRequest{Release: f.name, Files: files, Adopt: f.adopt, Timeout: f.timeout}
	if len(values) > 0 {
		req.Values = valuesHCL(values)
	}

	out, _, err := client.Rigger().Install(req, false, (&api.WriteOptions{}).WithContext(ctx))
	if err != nil {
		return err
	}

	serverWarnings(stderr, out.Warnings)

	if out.Action == string(rigger.ActionUnchanged) {
		_, err := fmt.Fprintf(stdout, "%s/%s is unchanged (revision %d)\n", f.namespace, f.name, out.Revision)

		return err
	}

	_, err = fmt.Fprintf(stdout, "%s %s/%s: revision %d\n", out.Action, f.namespace, f.name, out.Revision)

	return err
}

// serverImportBundle sends a bundle to the servers, which check it and keep
// it for the whole cluster.
func serverImportBundle(ctx context.Context, w io.Writer, p string) error {
	data, err := os.ReadFile(filepath.Clean(p))
	if err != nil {
		return err
	}

	client, err := newClient("")
	if err != nil {
		return err
	}

	b, _, err := client.Rigger().ImportBundle(data, (&api.WriteOptions{}).WithContext(ctx))
	if err != nil {
		return err
	}

	for _, m := range b.Modules {
		if _, err := fmt.Fprintf(w, "imported %s\n", m); err != nil {
			return err
		}
	}

	_, err = fmt.Fprintf(w, "the cluster keeps the bundle as %s\n", b.Name)

	return err
}

// convertJSON copies from into to through JSON: a type of the API into the
// type of the library it was written from.
func convertJSON(from, to any) error {
	data, err := json.Marshal(from)
	if err != nil {
		return err
	}

	return json.Unmarshal(data, to)
}
