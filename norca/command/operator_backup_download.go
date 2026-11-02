// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"strings"

	"github.com/posener/complete"
)

type OperatorBackupDownloadCommand struct {
	Meta
}

func (c *OperatorBackupDownloadCommand) Name() string { return "operator backup download" }

func (c *OperatorBackupDownloadCommand) Synopsis() string {
	return "Print a short-lived download URL of a backup object"
}

func (c *OperatorBackupDownloadCommand) Help() string {
	helpText := `
Usage: norca operator backup download [options] <id>

  Print a short-lived (15 minutes) pre-signed URL of an encrypted backup
  object. The object stays encrypted; decrypt it with the age CLI.

  Only S3 destinations sign URLs. Without -destination the first S3
  destination the backup was verified on is used; a backup stored only on
  directory destinations has no download URL, and the error names its file
  on the servers instead.

  If ACLs are enabled, this command requires a token with the 'write'
  capability of the 'backup' policy.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Download Options:

  -destination <name>
    The destination to download from. Defaults to the first S3 destination
    the backup was verified on.

  -object <name>
    The object: "state" (default), "keyring" or "manifest".
`
	return strings.TrimSpace(helpText)
}

func (c *OperatorBackupDownloadCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-destination": complete.PredictAnything,
			"-object":      complete.PredictSet("state", "keyring", "manifest"),
		})
}

func (c *OperatorBackupDownloadCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *OperatorBackupDownloadCommand) Run(args []string) int {
	var destination, object string

	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.StringVar(&destination, "destination", "", "")
	flags.StringVar(&object, "object", "state", "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if len(flags.Args()) != 1 {
		c.Ui.Error("This command takes one argument: <id>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}
	resp, _, err := client.Backups().DownloadURL(flags.Args()[0], destination, object, nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error creating the download URL: %s", err))
		return 1
	}
	c.Ui.Output(resp.URL)
	return 0
}
