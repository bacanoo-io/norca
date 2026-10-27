// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"errors"
	"fmt"
	"strings"

	"github.com/hashicorp/nomad/api"
	"github.com/posener/complete"
)

// secretVersionsCommand is the common implementation of secret delete,
// undelete and destroy.
type secretVersionsCommand struct {
	Meta
	op string
}

func (c *secretVersionsCommand) run(args []string, help func() string, name string) int {
	var versionsFlag string
	var checkIndex int64
	var all bool

	flags := c.Meta.FlagSet(name, FlagSetClient)
	flags.Usage = func() { c.Ui.Output(help()) }
	flags.StringVar(&versionsFlag, "versions", "", "")
	flags.Int64Var(&checkIndex, "check-index", -1, "")
	if c.op == "destroy" {
		flags.BoolVar(&all, "all", false, "")
	}
	if err := flags.Parse(args); err != nil {
		return 1
	}

	args = flags.Args()
	if len(args) != 1 {
		c.Ui.Error("This command takes one argument: <path>")
		c.Ui.Error(fmt.Sprintf("For additional help, try 'norca %s -help'", name))
		return 1
	}
	path := args[0]
	versions, err := parseSecretVersions(versionsFlag)
	if err != nil {
		c.Ui.Error(err.Error())
		return 1
	}
	if c.op == "destroy" {
		switch {
		case all && len(versions) > 0:
			c.Ui.Error("Only one of -versions and -all can be given")
			return 1
		case !all && len(versions) == 0:
			c.Ui.Error("Name the versions to destroy with -versions, or destroy the whole secret with -all")
			return 1
		}
	}

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}

	opts := &api.SecretVersionsOptions{Versions: versions, CheckIndex: optionalCheckIndex(checkIndex)}
	secrets := client.Secrets()
	var secret *api.Secret
	switch c.op {
	case "delete":
		secret, _, err = secrets.Delete(path, opts, nil)
	case "undelete":
		secret, _, err = secrets.Undelete(path, opts, nil)
	case "destroy":
		secret, _, err = secrets.Destroy(path, opts, nil)
	}
	if err != nil {
		if errors.Is(err, api.ErrSecretNotFound) {
			c.Ui.Error(fmt.Sprintf("Secret %q not found", path))
			return 1
		}
		c.Ui.Error(fmt.Sprintf("Error: %s", err))
		return 1
	}

	target := "the current version"
	if len(versions) > 0 {
		target = "versions " + versionsString(versions)
	}
	switch {
	case secret == nil:
		c.Ui.Output(fmt.Sprintf("Destroyed secret %q with all of its versions", path))
	default:
		c.Ui.Output(fmt.Sprintf("Secret %q: %s %s", path, secretOpPastTense[c.op], target))
	}
	return 0
}

// secretOpPastTense is the past tense of the operations for the output.
var secretOpPastTense = map[string]string{
	"delete":   "deleted",
	"undelete": "undeleted",
	"destroy":  "destroyed",
}

func versionsString(versions []uint64) string {
	parts := make([]string, len(versions))
	for i, v := range versions {
		parts[i] = fmt.Sprint(v)
	}
	return strings.Join(parts, ",")
}

const secretVersionsOptions = `
  -versions <n,...>
    Comma separated versions. Defaults to the current version.
` + secretCheckIndexOption

const secretCheckIndexOption = `
  -check-index <index>
    Apply only if the secret's modify index is the given one.
`

// SecretDeleteCommand soft deletes versions of a secret.
type SecretDeleteCommand struct{ Meta }

func (c *SecretDeleteCommand) Name() string     { return "secret delete" }
func (c *SecretDeleteCommand) Synopsis() string { return "Soft delete versions of a secret" }
func (c *SecretDeleteCommand) Help() string {
	return strings.TrimSpace(`
Usage: norca secret delete [options] <path>

  Soft delete versions of a secret: their values can no longer be read, but
  they can be restored with 'secret undelete' until they are destroyed.

  If ACLs are enabled, this command requires the 'write-secret' capability
  for the path.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Delete Options:
` + secretVersionsOptions)
}
func (c *SecretDeleteCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{"-versions": complete.PredictAnything, "-check-index": complete.PredictAnything})
}
func (c *SecretDeleteCommand) AutocompleteArgs() complete.Predictor { return complete.PredictAnything }
func (c *SecretDeleteCommand) Run(args []string) int {
	return (&secretVersionsCommand{Meta: c.Meta, op: "delete"}).run(args, c.Help, c.Name())
}

// SecretUndeleteCommand restores soft deleted versions of a secret.
type SecretUndeleteCommand struct{ Meta }

func (c *SecretUndeleteCommand) Name() string     { return "secret undelete" }
func (c *SecretUndeleteCommand) Synopsis() string { return "Restore soft deleted versions of a secret" }
func (c *SecretUndeleteCommand) Help() string {
	return strings.TrimSpace(`
Usage: norca secret undelete [options] <path>

  Restore soft deleted versions of a secret.

  If ACLs are enabled, this command requires the 'write-secret' capability
  for the path.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Undelete Options:
` + secretVersionsOptions)
}
func (c *SecretUndeleteCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{"-versions": complete.PredictAnything, "-check-index": complete.PredictAnything})
}
func (c *SecretUndeleteCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictAnything
}
func (c *SecretUndeleteCommand) Run(args []string) int {
	return (&secretVersionsCommand{Meta: c.Meta, op: "undelete"}).run(args, c.Help, c.Name())
}

// SecretDestroyCommand destroys versions of a secret, or the whole secret.
type SecretDestroyCommand struct{ Meta }

func (c *SecretDestroyCommand) Name() string     { return "secret destroy" }
func (c *SecretDestroyCommand) Synopsis() string { return "Destroy versions of a secret permanently" }
func (c *SecretDestroyCommand) Help() string {
	return strings.TrimSpace(`
Usage: norca secret destroy [options] -versions <n,...> <path>
       norca secret destroy [options] -all <path>

  Destroy versions of a secret permanently, or with -all the whole secret
  with all of its versions and its metadata. Destroyed values can not be
  restored. One of -versions and -all is required: there is no default, so
  that a destroy never removes more than was named.

  If ACLs are enabled, this command requires the 'destroy-secret' capability
  for the path.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Destroy Options:

  -all
    Destroy the whole secret with all of its versions and its metadata.

  -versions <n,...>
    Comma separated versions to destroy. Required unless -all is given.
` + secretCheckIndexOption)
}
func (c *SecretDestroyCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-all":         complete.PredictNothing,
			"-versions":    complete.PredictAnything,
			"-check-index": complete.PredictAnything,
		})
}
func (c *SecretDestroyCommand) AutocompleteArgs() complete.Predictor { return complete.PredictAnything }
func (c *SecretDestroyCommand) Run(args []string) int {
	return (&secretVersionsCommand{Meta: c.Meta, op: "destroy"}).run(args, c.Help, c.Name())
}
