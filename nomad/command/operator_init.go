// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"strings"

	"github.com/hashicorp/nomad/command/operatorinit"
	"github.com/posener/complete"
)

// OperatorInitCommand scaffolds an operator project for the controller SDK.
type OperatorInitCommand struct {
	Meta
}

func (c *OperatorInitCommand) Name() string { return "operator init" }

func (c *OperatorInitCommand) Synopsis() string {
	return "Scaffold an operator project for the controller SDK"
}

func (c *OperatorInitCommand) Help() string {
	helpText := `
Usage: norca operator init [options] <group>/<Kind> [<directory>]

  Init creates the project of an operator (a controller of custom objects)
  for the Norca controller SDK, in the directory (default: the operator
  name). It does not contact the servers.

  The project contains the object kind (HCL, with a JSON Schema, a CEL rule,
  columns, schema version and limits) and a sample object, the Go module of
  the controller (reconciler with a finalizer, status conditions and the
  event log; leader election; metrics), a validating webhook, tests against
  the SDK's fake backend and against a dev agent (envtest), and an operator
  pack that installs the kind, the ACL policy of the controller and the
  controller job as one release.

      $ norca operator init -sdk ~/norca/nomad db.example.com/PostgresCluster
      $ cd postgres-cluster-operator && go mod tidy && go test ./...

Init Options:

  -name
    Name of the operator: binary, controller, job and pack (default:
    "<kind in kebab case>-operator").

  -module
    Go module path (default: "example.com/<name>").

  -cluster-scoped
    Make the kind cluster-scoped instead of namespaced.

  -sdk
    The "nomad" directory of a Norca checkout. go.mod then replaces the SDK
    and API modules with its controller and api directories; without it the
    replace directives are left for you to fill in.

  -force
    Overwrite existing files.
`
	return strings.TrimSpace(helpText)
}

func (c *OperatorInitCommand) AutocompleteFlags() complete.Flags {
	return complete.Flags{
		"-name":           complete.PredictAnything,
		"-module":         complete.PredictAnything,
		"-cluster-scoped": complete.PredictNothing,
		"-sdk":            complete.PredictDirs("*"),
		"-force":          complete.PredictNothing,
	}
}

func (c *OperatorInitCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *OperatorInitCommand) Run(args []string) int {
	var opts operatorinit.Options
	flags := c.Meta.FlagSet(c.Name(), FlagSetNone)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.StringVar(&opts.Name, "name", "", "")
	flags.StringVar(&opts.Module, "module", "", "")
	flags.BoolVar(&opts.ClusterScoped, "cluster-scoped", false, "")
	flags.StringVar(&opts.SDKPath, "sdk", "", "")
	flags.BoolVar(&opts.Force, "force", false, "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	args = flags.Args()
	if len(args) < 1 || len(args) > 2 {
		c.Ui.Error("This command takes one or two arguments: <group>/<Kind> [<directory>]")
		c.Ui.Error(commandErrorText(c))
		return 1
	}
	group, kind, err := splitKindRef(args[0])
	if err != nil {
		c.Ui.Error(err.Error())
		return 1
	}
	opts.Group, opts.Kind = group, kind

	files, err := operatorinit.Render(opts)
	if err != nil {
		c.Ui.Error(err.Error())
		return 1
	}
	opts.Dir = opts.Name
	if opts.Dir == "" {
		// The default name, as Render derived it.
		for _, f := range files {
			if strings.HasPrefix(f.Path, "pack/") {
				opts.Dir = strings.SplitN(f.Path, "/", 3)[1]
				break
			}
		}
	}
	if len(args) == 2 {
		opts.Dir = args[1]
	}

	written, err := operatorinit.Generate(opts)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error creating the project: %v", err))
		return 1
	}
	c.Ui.Output(fmt.Sprintf("Created the operator project of %s in %s:", args[0], opts.Dir))
	for _, f := range written {
		c.Ui.Output("  " + f)
	}
	c.Ui.Output("\nNext: see README.md (go mod tidy, norca object-kind lint, go test ./...)")
	if opts.SDKPath == "" {
		c.Ui.Warn("go.mod needs replace directives for the SDK and API modules of your Norca checkout (or use -sdk).")
	}
	return 0
}
