// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"strings"

	"github.com/hashicorp/nomad/api"
	"github.com/posener/complete"
)

type OperatorBackupDiffCommand struct {
	Meta
}

func (c *OperatorBackupDiffCommand) Name() string { return "operator backup diff" }

func (c *OperatorBackupDiffCommand) Synopsis() string {
	return "Compare a backup with the current state or another backup"
}

func (c *OperatorBackupDiffCommand) Help() string {
	helpText := `
Usage: norca operator backup diff [options] <id> [<other-id>]

  Compare a backup with the current state of the region, or with another
  (newer) backup. Objects are reported as added (only on the right side),
  deleted (only in the backup) or modified. The comparison covers namespaces,
  jobs, job versions, Variables, ACL policies and roles, deployments and
  allocations; -type restricts it. With -job, the job diff of that job is
  shown too.

  Without -namespace, all namespaces are compared.

  If ACLs are enabled, this command requires a token with the 'read'
  capability of the 'backup' policy.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Diff Options:
` + backupIdentityUsage + `

  -type <type>
    Only compare one content type: ` + strings.Join(backupContentTypes, ", ") + `.

  -job <id>
    Only compare the objects of a job, and show the job diff.

  -destination <name>
    The destination to read the backups from.

  -json
    Output the comparison in JSON format.
`
	return strings.TrimSpace(helpText)
}

func (c *OperatorBackupDiffCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-age-identity": complete.PredictFiles("*"),
			"-type":         complete.PredictSet(backupContentTypes...),
			"-job":          complete.PredictAnything,
			"-destination":  complete.PredictAnything,
			"-json":         complete.PredictNothing,
		})
}

func (c *OperatorBackupDiffCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *OperatorBackupDiffCommand) Run(args []string) int {
	var json bool
	var identityPath, contentType, jobID, destination string

	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.StringVar(&identityPath, "age-identity", "", "")
	flags.StringVar(&contentType, "type", "", "")
	flags.StringVar(&jobID, "job", "", "")
	flags.StringVar(&destination, "destination", "", "")
	flags.BoolVar(&json, "json", false, "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	args = flags.Args()
	if len(args) < 1 || len(args) > 2 {
		c.Ui.Error("This command takes one or two arguments: <id> [<other-id>]")
		c.Ui.Error(commandErrorText(c))
		return 1
	}
	against := "current"
	if len(args) == 2 {
		against = args[1]
	}
	identity, err := readAgeIdentity(identityPath)
	if err != nil {
		c.Ui.Error(err.Error())
		return 1
	}

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}

	q := &api.QueryOptions{}
	if c.Meta.namespace == "" && jobID == "" {
		q.Namespace = "*"
	}
	diff, _, err := client.Backups().Diff(args[0], &api.BackupDiffOptions{
		Against: against, Type: contentType, JobID: jobID, Destination: destination, AgeIdentity: identity,
	}, q)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error comparing backup: %s", err))
		return 1
	}

	if json {
		out, err := Format(true, "", diff)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error formatting output: %s", err))
			return 1
		}
		c.Ui.Output(out)
		return 0
	}

	c.Ui.Output(fmt.Sprintf("Comparing backup %s (index %d) with %s (index %d)", diff.From, diff.FromIndex, diff.To, diff.ToIndex))
	if len(diff.Objects) == 0 {
		c.Ui.Output("No differences")
	} else {
		out := []string{"Type|Change|Namespace|ID|Version|Status"}
		for _, d := range diff.Objects {
			out = append(out, fmt.Sprintf("%s|%s|%s|%s|%s|%s", d.Type, d.Change, d.Namespace, d.ID,
				formatBackupDiffValue(versionString(d.FromVersion), versionString(d.ToVersion)),
				formatBackupDiffValue(d.FromStatus, d.ToStatus)))
		}
		c.Ui.Output(formatList(out))
	}

	if diff.JobDiff != nil {
		c.Ui.Output(c.Colorize().Color("\n[bold]Job diff[reset]"))
		c.Ui.Output(c.Colorize().Color(strings.TrimSpace(formatJobDiff(diff.JobDiff, true))))
	}
	return 0
}

func versionString(v *uint64) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(*v)
}

func formatBackupDiffValue(from, to string) string {
	switch {
	case from == to:
		return from
	case from == "":
		return to
	case to == "":
		return from
	}
	return from + " -> " + to
}
