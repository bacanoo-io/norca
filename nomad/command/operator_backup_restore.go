// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/hashicorp/nomad/api"
	flaghelper "github.com/hashicorp/nomad/helper/flags"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/posener/complete"
)

type OperatorBackupRestoreCommand struct {
	Meta
}

func (c *OperatorBackupRestoreCommand) Name() string { return "operator backup restore" }

func (c *OperatorBackupRestoreCommand) Synopsis() string {
	return "Restore jobs, Variables, namespaces or ACL objects from a backup"
}

func (c *OperatorBackupRestoreCommand) Help() string {
	helpText := `
Usage: norca operator backup restore [options] <id>

  Restore selected objects from a backup (selective restore). Every item is
  written with the normal API on behalf of your token: a job is registered as
  a new version (the admission chain and quotas apply), a Variables entry is
  re-encrypted with the current keys with a check-and-set write, namespaces and
  ACL policies and roles are upserted. A full restore of the region is not
  available yet.

  Jobs and Variables are restored in the namespace given with -namespace
  (default: "default").

  If ACLs are enabled, this command requires a token with the 'read'
  capability of the 'backup' policy and the permissions of the normal writes
  (for example 'submit-job', Variables 'write', or a management token for
  namespaces and ACL objects).

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Restore Options:
` + backupIdentityUsage + `

  -job <id>[@<version>]
    Restore a job, at the given version of the backup or at the version that
    was current at backup time. Can be given several times.

  -variable <path>[@<check-index>]
    Restore a Variables entry. Without a check index the entry is only
    restored if it does not exist; to overwrite an existing entry give its
    current modify index (shown by -dry-run). Can be given several times.

  -restore-namespace <name>
    Restore a namespace. Can be given several times.

  -acl-policy <name>
    Restore an ACL policy. Can be given several times.

  -acl-role <name>
    Restore an ACL role. Can be given several times.

  -dry-run
    Only show what would be restored.

  -destination <name>
    The destination to read the backup from.

  -json
    Output the result in JSON format.
`
	return strings.TrimSpace(helpText)
}

func (c *OperatorBackupRestoreCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-age-identity":      complete.PredictFiles("*"),
			"-job":               complete.PredictAnything,
			"-variable":          complete.PredictAnything,
			"-restore-namespace": complete.PredictAnything,
			"-acl-policy":        complete.PredictAnything,
			"-acl-role":          complete.PredictAnything,
			"-dry-run":           complete.PredictNothing,
			"-destination":       complete.PredictAnything,
			"-json":              complete.PredictNothing,
		})
}

func (c *OperatorBackupRestoreCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

// splitAt splits "name@number".
func splitAt(s string) (string, *uint64, error) {
	name, num, ok := strings.Cut(s, "@")
	if !ok {
		return s, nil, nil
	}
	n, err := strconv.ParseUint(num, 10, 64)
	if err != nil {
		return "", nil, fmt.Errorf("invalid number in %q", s)
	}
	return name, pointer.Of(n), nil
}

func (c *OperatorBackupRestoreCommand) Run(args []string) int {
	var dryRun, json bool
	var identityPath, destination string
	var jobs, variables, namespaces, policies, roles flaghelper.StringFlag

	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.StringVar(&identityPath, "age-identity", "", "")
	flags.Var(&jobs, "job", "")
	flags.Var(&variables, "variable", "")
	flags.Var(&namespaces, "restore-namespace", "")
	flags.Var(&policies, "acl-policy", "")
	flags.Var(&roles, "acl-role", "")
	flags.BoolVar(&dryRun, "dry-run", false, "")
	flags.StringVar(&destination, "destination", "", "")
	flags.BoolVar(&json, "json", false, "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if len(flags.Args()) != 1 {
		c.Ui.Error("This command takes one argument: <id>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}

	namespace := c.Meta.namespace
	if namespace == "" {
		namespace = "default"
	}
	var items []*api.BackupRestoreItem
	for _, j := range jobs {
		id, version, err := splitAt(j)
		if err != nil {
			c.Ui.Error(err.Error())
			return 1
		}
		items = append(items, &api.BackupRestoreItem{Type: api.BackupRestoreItemJob, Namespace: namespace, ID: id, Version: version})
	}
	for _, v := range variables {
		path, index, err := splitAt(v)
		if err != nil {
			c.Ui.Error(err.Error())
			return 1
		}
		items = append(items, &api.BackupRestoreItem{Type: api.BackupRestoreItemVariable, Namespace: namespace, ID: path, CheckIndex: index})
	}
	for _, n := range namespaces {
		items = append(items, &api.BackupRestoreItem{Type: api.BackupRestoreItemNamespace, ID: n})
	}
	for _, p := range policies {
		items = append(items, &api.BackupRestoreItem{Type: api.BackupRestoreItemACLPolicy, ID: p})
	}
	for _, r := range roles {
		items = append(items, &api.BackupRestoreItem{Type: api.BackupRestoreItemACLRole, ID: r})
	}
	if len(items) == 0 {
		c.Ui.Error("Nothing to restore: use -job, -variable, -restore-namespace, -acl-policy or -acl-role")
		return 1
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

	resp, _, err := client.Backups().Restore(flags.Args()[0], &api.BackupRestoreRequest{
		Mode:        api.BackupRestoreModeSelective,
		Items:       items,
		DryRun:      dryRun,
		Destination: destination,
		AgeIdentity: identity,
	}, nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error restoring from backup: %s", err))
		return 1
	}

	if json {
		out, err := Format(true, "", resp)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error formatting output: %s", err))
			return 1
		}
		c.Ui.Output(out)
	} else {
		out := []string{"Item|Action|Description|Result"}
		for _, r := range resp.Results {
			result := "would " + r.Action
			switch {
			case r.Error != "":
				result = "error: " + r.Error
			case !dryRun:
				result = fmt.Sprintf("restored at index %d", r.Index)
			}
			out = append(out, fmt.Sprintf("%s|%s|%s|%s", formatBackupRestoreItem(r.Item), r.Action, r.Description, result))
		}
		c.Ui.Output(formatList(out))
		if resp.Restore != nil {
			c.Ui.Output(fmt.Sprintf("\nRestore %q %s", resp.Restore.ID, resp.Restore.Status))
		}
	}

	for _, r := range resp.Results {
		if r.Error != "" {
			return 1
		}
	}
	return 0
}

func formatBackupRestoreItem(item *api.BackupRestoreItem) string {
	if item == nil {
		return ""
	}
	s := item.Type + " "
	if item.Namespace != "" {
		s += item.Namespace + "/"
	}
	s += item.ID
	if item.Version != nil {
		s += fmt.Sprintf("@%d", *item.Version)
	}
	return s
}
