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
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2"

	"github.com/bacanoo-io/norca/norca/api"

	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/cli/norcaapi"
	"github.com/bacanoo-io/norca/rigger/host"
	"github.com/bacanoo-io/norca/rigger/source"
)

// installFlags are the flags of rigger install.
type installFlags struct {
	renderFlags

	// address is the source of the module when the argument names no
	// directory, or with -registry the name of a module of that registry;
	// empty for a module in the directory dir.
	address string
	// version is the version constraint of a module of an address.
	version string
	// registry is a registry of the cluster the servers install from.
	registry string
	name     string
	timeout  time.Duration
	adopt    bool
	server   bool
}

var (
	errInstallName           = errors.New("install needs -name, the name of the release")
	errInstallVersionDir     = errors.New("-version picks a version of a module by its address; a module in a directory is of the version it has")
	errInstallRegistryServer = errors.New("-registry names a registry of the cluster, which its servers install from: it works with -server")
	errInstallRegistryModule = errors.New("-registry needs the module, as the registry lists it: rigger install <name> -registry <registry> -server")
)

// runInstall runs rigger install <dir|address> -name x: one release of a
// module, a stack of one owned by no stack (design 10).
func runInstall(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, args []string) int {
	f, err := parseInstallFlags(stderr, args)
	if err != nil {
		if !errors.Is(err, errReported) {
			warn(stderr, "%v", err)
		}

		return exitError
	}

	return run(ctx, logger, stderr, "install", func(ctx context.Context) error {
		if f.server {
			return serverInstall(ctx, logger, stdout, stderr, f)
		}

		return install(ctx, logger, stdout, stderr, f)
	})
}

// parseInstallFlags parses [dir|address] [flags]. The argument is a module
// in a directory when it names one (or is missing: the current directory),
// else the address of a module (isModuleAddress); with -registry it is the
// name of a module of that registry. An error the flag set wrote already
// is errReported.
func parseInstallFlags(stderr io.Writer, args []string) (*installFlags, error) {
	f := &installFlags{}

	var arg string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		arg, args = args[0], args[1:]
	}

	flags := flag.NewFlagSet("rigger install", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Var(&f.files, "f", "a file of values (*.vars.hcl); may be given more than once")
	flags.Var(&f.vars, "var", "a value, name=value; may be given more than once"+varSyntax)
	flags.StringVar(&f.name, "name", "", "name of the release")
	flags.StringVar(&f.namespace, "namespace", "default", "namespace of the release")
	flags.StringVar(&f.region, "region", "", "region of the release (default: the region of the cluster)")
	flags.StringVar(&f.version, "version", "", "a module by its address: the version constraint (\"~> 1.2\"); none takes the newest")
	flags.StringVar(&f.registry, "registry", "", "-server: a registry of the cluster to install from; the argument is the module as it lists it")
	flags.DurationVar(&f.timeout, "timeout", 0, "how long to wait until the release is ready (default: the server's)")
	flags.BoolVar(&f.adopt, "adopt", false, "take over a release another package manager applied or a stack owns")
	flags.BoolVar(&f.server, "server", serverDefault(), serverUsage)

	if err := flags.Parse(args); err != nil {
		return nil, errReported
	}

	if f.name == "" || flags.NArg() > 0 {
		return nil, errInstallName
	}

	switch {
	case f.registry != "":
		if !f.server {
			return nil, errInstallRegistryServer
		}

		if arg == "" {
			return nil, errInstallRegistryModule
		}

		f.address = arg
	case arg == "":
		f.dir = "."
	case isModuleAddress(arg):
		f.address = arg
	default:
		f.dir = arg
	}

	if f.address == "" && f.version != "" {
		return nil, errInstallVersionDir
	}

	return f, nil
}

// isModuleAddress reports whether arg is the address of a module rather
// than a directory: no path that exists, and an address source.Parse
// reads (<namespace>/<name>, builtin/<name>, git, oci://, dir://).
func isModuleAddress(arg string) bool {
	if strings.HasPrefix(arg, ".") || filepath.IsAbs(arg) || !strings.Contains(arg, "/") {
		return false
	}

	if _, err := os.Stat(arg); err == nil {
		return false
	}

	_, err := source.Parse(arg, nil)

	return err == nil
}

// target is what the messages of an install name: the directory, or the
// address of the module.
func (f *installFlags) target() string {
	if f.address != "" {
		return f.address
	}

	return f.dir
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

	m, diags, err := installModule(ctx, eng, f)
	if err != nil {
		return err
	}

	if diags.HasErrors() {
		return reportDiags(stderr, f.target(), diags)
	}

	// The warnings too: of the version fetched (an advisory, no
	// signature), of a variable of the environment that is not used.
	values, moreDiags := readValues(&f.renderFlags, m.FormSchema())
	if err := checkDiags(stderr, f.target(), append(diags, moreDiags...)); err != nil {
		return err
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

	rp, diags := eng.PlanInstall(ctx, m, rigger.Install{Release: f.name, Namespace: f.namespace, Region: region, Values: values},
		host.Settings{Wait: true, Timeout: f.timeout})
	if err := checkDiags(stderr, f.target(), diags); err != nil {
		return err
	}

	if rp.Action == rigger.ActionUnchanged {
		_, err := fmt.Fprintf(stdout, "%s/%s is unchanged (revision %d)\n", rp.Release.Namespace, rp.Release.Name, rp.Revision)

		return err
	}

	outcome, err := eng.ApplyInstall(ctx, rp, rigger.ApplyOptions{Adopt: f.adopt})
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(stdout, "%s %s/%s: revision %d\n", outcome.Action, rp.Release.Namespace, rp.Release.Name, outcome.Revision)

	return err
}

// installModule loads the module of an install: the newest version of the
// address that meets -version, with the warnings of that version (an
// advisory of the global registry, no signature), else the module in the
// directory. Of a directory only the errors count: its warnings are for
// its author, whom rigger module validate shows them.
func installModule(ctx context.Context, eng *rigger.Engine, f *installFlags) (*rigger.Module, hcl.Diagnostics, error) {
	if f.address != "" {
		m, diags := eng.FetchNewestModule(ctx, f.address, f.version)

		return m, diags, nil
	}

	m, diags, err := loadModule(ctx, eng, f.dir)
	if err != nil || diags.HasErrors() {
		return m, diags, err
	}

	return m, nil, nil
}

// errServerSource is the error of -server with an address the servers do
// not fetch from: they read only the registries they synced.
var errServerSource = errors.New("the servers fetch modules only from the registries of the cluster they have synced (rigger registry sync <name>) " +
	"and their built-in modules: install a module of a registered repository with -registry <name>, or install without -server")

// moduleInstallRequest is the request of rigger install -server for a
// module the servers fetch: a module of a registry of the cluster
// (-registry), of the global registry by its address <namespace>/<name>,
// or a built-in module (builtin/<name>, no registry). The values are the
// caller's to add.
func moduleInstallRequest(f *installFlags) (*api.RiggerInstallRequest, error) {
	req := &api.RiggerInstallRequest{Release: f.name, Version: f.version, Adopt: f.adopt, Timeout: f.timeout}

	if f.registry != "" {
		req.Registry, req.Module = f.registry, f.address

		return req, nil
	}

	src, err := source.Parse(f.address, nil)
	if err != nil {
		return nil, err
	}

	switch src.Kind {
	case source.KindGlobal:
		req.Registry, req.Module = api.RiggerGlobalRegistry, f.address
	case source.KindBuiltin:
		req.Module = f.address
	case source.KindDirectory:
		return nil, fmt.Errorf("%s: %w", source.Redact(f.address), rigger.ErrDirectorySource)
	case source.KindGit, source.KindOCI:
		return nil, fmt.Errorf("%s: %w", source.Redact(f.address), errServerSource)
	}

	return req, nil
}
