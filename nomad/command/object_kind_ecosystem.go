// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"strings"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/helper/objectlint"
	"github.com/mitchellh/cli"
	"github.com/posener/complete"
)

// ObjectKindWebhookCommand groups the commands of type-bound webhooks.
type ObjectKindWebhookCommand struct {
	Meta
}

func (c *ObjectKindWebhookCommand) Name() string { return "object-kind webhook" }

func (c *ObjectKindWebhookCommand) Synopsis() string {
	return "Approve or revoke webhooks bound to object kinds"
}

func (c *ObjectKindWebhookCommand) Help() string {
	helpText := `
Usage: norca object-kind webhook <subcommand> [options] <group>/<Kind> <webhook>

  Admission webhooks bound to an object kind review the writes of its
  objects. They are declared in the kind (usually by the package of the
  operator), but the servers call them only after an administrator approved
  them: kinds applied with a management token approve their webhooks at
  once, others wait for an approval. Any change of a webhook's
  configuration revokes its approval.

  Approve a webhook:

      $ norca object-kind webhook approve norca.io/LoadBalancer validate

  Revoke the approval of a webhook:

      $ norca object-kind webhook revoke norca.io/LoadBalancer validate

  "norca object-kind inspect" shows the webhooks and their approvals.
`
	return strings.TrimSpace(helpText)
}

func (c *ObjectKindWebhookCommand) Run(_ []string) int {
	return cli.RunResultHelp
}

// ObjectKindWebhookApproveCommand approves (or revokes) a webhook.
type ObjectKindWebhookApproveCommand struct {
	Meta

	// revoke selects "object-kind webhook revoke".
	revoke bool
}

func (c *ObjectKindWebhookApproveCommand) Name() string {
	if c.revoke {
		return "object-kind webhook revoke"
	}
	return "object-kind webhook approve"
}

func (c *ObjectKindWebhookApproveCommand) Synopsis() string {
	if c.revoke {
		return "Revoke the approval of a webhook bound to an object kind"
	}
	return "Approve a webhook bound to an object kind"
}

func (c *ObjectKindWebhookApproveCommand) Help() string {
	verb, what := "approve", "Approve is used to approve a webhook bound to an object kind, so that the\n  servers call it. The approval covers the current configuration of the\n  webhook (URL, CA, mode, operations, timeout, failure policy)."
	if c.revoke {
		verb, what = "revoke", "Revoke is used to remove the approval of a webhook bound to an object\n  kind. Writes the webhook reviews then fail, unless its failure policy is\n  \"ignore\"."
	}
	helpText := `
Usage: norca object-kind webhook ` + verb + ` [options] <group>/<Kind> <webhook>

  ` + what + `

  If ACLs are enabled, this command requires a management token.

General Options:

  ` + generalOptionsUsage(usageOptsDefault)
	return strings.TrimSpace(helpText)
}

func (c *ObjectKindWebhookApproveCommand) AutocompleteFlags() complete.Flags {
	return c.Meta.AutocompleteFlags(FlagSetClient)
}

func (c *ObjectKindWebhookApproveCommand) AutocompleteArgs() complete.Predictor {
	return objectKindPredictor(c.Meta.Client)
}

func (c *ObjectKindWebhookApproveCommand) Run(args []string) int {
	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	if err := flags.Parse(args); err != nil {
		return 1
	}
	args = flags.Args()
	if len(args) != 2 {
		c.Ui.Error("This command takes two arguments: <group>/<Kind> <webhook>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}
	group, kind, err := splitKindRef(args[0])
	if err != nil {
		c.Ui.Error(err.Error())
		return 1
	}
	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}
	if _, _, err := client.ObjectKinds().ApproveWebhook(group, kind, args[1], c.revoke, nil); err != nil {
		c.Ui.Error(fmt.Sprintf("Error updating the webhook approval: %s", err))
		return 1
	}
	if c.revoke {
		c.Ui.Output(fmt.Sprintf("Revoked the approval of webhook %q of %s", args[1], args[0]))
	} else {
		c.Ui.Output(fmt.Sprintf("Approved webhook %q of %s", args[1], args[0]))
	}
	return 0
}

// ObjectKindLintCommand checks an object kind offline.
type ObjectKindLintCommand struct {
	Meta
}

func (c *ObjectKindLintCommand) Name() string { return "object-kind lint" }

func (c *ObjectKindLintCommand) Synopsis() string {
	return "Check an object kind and sample objects without a server"
}

func (c *ObjectKindLintCommand) Help() string {
	helpText := `
Usage: norca object-kind lint [options] <kind file> [<object file>...]

  Lint compiles the JSON Schema, the CEL rules and the migration of an
  object kind like the servers do, without contacting them, and reports
  problems and deviations from the conventions of the operator framework
  (status fields the controller SDK writes, webhook settings, limits).

  Sample objects given as further arguments (in the "norca object apply"
  format) are validated against the kind: the defaults of the schema are
  filled in, then the schema and the CEL rules (as for a create) are
  checked.

      $ norca object-kind lint loadbalancer.kind.hcl examples/web.hcl

  The exit code is 1 if there are errors, 0 otherwise (warnings included).

Lint Options:

  -json
    Parse the kind file as a JSON object kind (also used for .json files).

  -show-defaults
    Print the sample objects with the defaults of the schema filled in.
`
	return strings.TrimSpace(helpText)
}

func (c *ObjectKindLintCommand) AutocompleteFlags() complete.Flags {
	return complete.Flags{
		"-json":          complete.PredictNothing,
		"-show-defaults": complete.PredictNothing,
	}
}

func (c *ObjectKindLintCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictOr(complete.PredictFiles("*.hcl"), complete.PredictFiles("*.json"))
}

func (c *ObjectKindLintCommand) Run(args []string) int {
	var jsonInput, showDefaults bool
	flags := c.Meta.FlagSet(c.Name(), FlagSetNone)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.BoolVar(&jsonInput, "json", false, "")
	flags.BoolVar(&showDefaults, "show-defaults", false, "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	args = flags.Args()
	if len(args) < 1 {
		c.Ui.Error("This command takes at least one argument: <kind file> [<object file>...]")
		c.Ui.Error(commandErrorText(c))
		return 1
	}

	content, path, err := readObjectInput(args[0])
	if err != nil {
		c.Ui.Error(err.Error())
		return 1
	}
	kind, err := parseObjectKind(path, content, jsonInput)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("error: %s: %v", args[0], err))
		return 1
	}
	var samples []*api.CustomObject
	for _, file := range args[1:] {
		content, path, err := readObjectInput(file)
		if err != nil {
			c.Ui.Error(err.Error())
			return 1
		}
		obj, err := parseCustomObject(path, content, false)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("error: %s: %v", file, err))
			return 1
		}
		samples = append(samples, obj)
	}

	report, err := objectlint.Lint(kind, samples)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error linting the object kind: %v", err))
		return 1
	}
	errors, warnings := 0, 0
	for _, f := range report.Findings {
		if f.Severity == objectlint.SeverityError {
			errors++
			c.Ui.Error(f.String())
		} else {
			warnings++
			c.Ui.Warn(f.String())
		}
	}
	if showDefaults {
		for _, obj := range report.Objects {
			c.Ui.Output(fmt.Sprintf("%s %s: %s", obj.KindRef(), obj.Name, compactJSON(obj.Spec)))
		}
	}
	c.Ui.Output(fmt.Sprintf("%s: %d errors, %d warnings, %d objects checked", kind.KindRef(), errors, warnings, len(samples)))
	if report.HasErrors() {
		return 1
	}
	return 0
}
