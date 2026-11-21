// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	rig "github.com/bacanoo-io/norca-rig"
	"github.com/bacanoo-io/norca-rig/cli/norcaapi"
	"github.com/bacanoo-io/norca-rig/host"
)

// installFlags are the flags of rig install.
type installFlags struct {
	renderFlags

	name    string
	timeout time.Duration
}

var errInstallName = errors.New("install needs -name, the name of the release")

// runInstall runs rig install <dir> -name x: one release of a module, a
// stack of one owned by no stack (design 10).
func runInstall(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, args []string) int {
	f := &installFlags{}
	f.dir = "."

	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		f.dir, args = args[0], args[1:]
	}

	flags := flag.NewFlagSet("rig install", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Var(&f.files, "f", "a file of values (*.vars.hcl); may be given more than once")
	flags.Var(&f.vars, "var", "a value, name=value; may be given more than once")
	flags.StringVar(&f.name, "name", "", "name of the release")
	flags.StringVar(&f.namespace, "namespace", "default", "namespace of the release")
	flags.StringVar(&f.region, "region", "", "region of the release (default: the region of the cluster)")
	flags.DurationVar(&f.timeout, "timeout", 0, "how long to wait until the release is ready (default: the server's)")

	if err := flags.Parse(args); err != nil {
		return exitError
	}

	if f.name == "" || flags.NArg() > 0 {
		warn(stderr, "%v", errInstallName)

		return exitError
	}

	return run(ctx, logger, stderr, "install", func(ctx context.Context) error {
		return install(ctx, logger, stdout, stderr, f)
	})
}

func install(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, f *installFlags) error {
	client, err := newClient(f.region)
	if err != nil {
		return err
	}

	eng, err := clusterEngine(logger, client, f.namespace)
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

	region := f.region
	if region == "" {
		if info, err := norcaapi.NewCluster(client).Info(ctx); err == nil {
			region = info.Region
		}
	}

	if region == "" {
		region = "global"
	}

	rp, diags := eng.PlanInstall(ctx, m, rig.Install{Release: f.name, Namespace: f.namespace, Region: region, Values: values},
		host.Settings{Wait: true, Timeout: f.timeout})
	if diags.HasErrors() {
		return reportDiags(stderr, f.dir, diags)
	}

	if rp.Action == rig.ActionUnchanged {
		_, err := fmt.Fprintf(stdout, "%s/%s is unchanged (revision %d)\n", rp.Release.Namespace, rp.Release.Name, rp.Revision)

		return err
	}

	outcome, err := eng.ApplyInstall(ctx, rp)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(stdout, "%s %s/%s: revision %d\n", outcome.Action, rp.Release.Namespace, rp.Release.Name, outcome.Revision)

	return err
}
