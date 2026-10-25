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
	return "Restore objects from a backup, or request a full restore of the region"
}

func (c *OperatorBackupRestoreCommand) Help() string {
	helpText := `
Usage: norca operator backup restore [options] <id>

  Restore selected objects from a backup (selective restore). Every item is
  written with the normal API on behalf of your token: a job is registered as
  a new version (the admission chain and quotas apply), a Variables entry is
  re-encrypted with the current keys with a check-and-set write, the value of
  a secret is written as a new version of the secret, namespaces and ACL
  policies and roles are upserted.

  With -full, request a full restore of the region instead: the whole state
  of the region returns to the time of the backup, and every change since is
  lost. The request shows a preview of the changes (-dry-run shows only the
  preview) and must be confirmed with -confirm <region>. A second
  administrator then approves it with "norca operator backup approve", which
  runs it: an automatic, pinned backup of the current state is taken first,
  the keys of the backup are added to the keystore of every server, and the
  Raft state of the backup is restored. The request expires after the
  approval_ttl of the server's backup configuration.

  Jobs, Variables and secrets are restored in the namespace given with -namespace
  (default: "default").

  If ACLs are enabled, this command requires a token with the 'read'
  capability of the 'backup' policy and the permissions of the normal writes
  (for example 'submit-job', Variables 'write', 'write-secret', or a management token for
  namespaces and ACL objects). A full restore requires a management token,
  and ACLs must be enabled.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Restore Options:
` + backupIdentityUsage + `

  -full
    Request a full restore of the region from the backup.

  -confirm <region>
    The name of the region, to confirm a full restore.

  -job <id>[@<version>]
    Restore a job, at the given version of the backup or at the version that
    was current at backup time. Can be given several times.

  -variable <path>[@<check-index>]
    Restore a Variables entry. Without a check index the entry is only
    restored if it does not exist; to overwrite an existing entry give its
    current modify index (shown by -dry-run). Can be given several times.

  -secret <path>[@<check-index>]
    Restore a secret of the built-in secret store: the items of one version
    of the secret in the backup (the version that was current at backup
    time, or the one given with -version) are written as a new version on
    top of the secret's current versions, which stay. A soft deleted secret
    becomes live again; a secret that does not exist is created. The check
    index is an optional guard: without it the new version is always
    written, with 0 only if the secret does not exist, otherwise only if it
    is the secret's current modify index (shown by -dry-run). The result
    shows the new version. Can be given several times.

  -version <n>
    The version of the secret in the backup to restore. Only used with a
    single -secret.

  -with-metadata
    Also restore the metadata of the secrets (labels, owner, description,
    expires_at, rotate_every, max_versions) from the backup. By default only
    the value is restored and the current metadata is kept; a secret that
    does not exist gets the backup's metadata either way.

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
			"-full":              complete.PredictNothing,
			"-confirm":           complete.PredictAnything,
			"-job":               complete.PredictAnything,
			"-variable":          complete.PredictAnything,
			"-secret":            complete.PredictAnything,
			"-version":           complete.PredictAnything,
			"-with-metadata":     complete.PredictNothing,
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
	var dryRun, json, full, withMetadata bool
	var identityPath, destination, confirm string
	var secretVersion int64
	var jobs, variables, secrets, namespaces, policies, roles flaghelper.StringFlag

	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.StringVar(&identityPath, "age-identity", "", "")
	flags.BoolVar(&full, "full", false, "")
	flags.StringVar(&confirm, "confirm", "", "")
	flags.Var(&jobs, "job", "")
	flags.Var(&variables, "variable", "")
	flags.Var(&secrets, "secret", "")
	flags.Int64Var(&secretVersion, "version", -1, "")
	flags.BoolVar(&withMetadata, "with-metadata", false, "")
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
	switch {
	case secretVersion != -1 && len(secrets) != 1:
		c.Ui.Error("-version is only used with a single -secret")
		return 1
	case secretVersion < -1:
		c.Ui.Error("-version must not be negative")
		return 1
	case withMetadata && len(secrets) == 0:
		c.Ui.Error("-with-metadata is only used with -secret")
		return 1
	}
	for _, s := range secrets {
		path, index, err := splitAt(s)
		if err != nil {
			c.Ui.Error(err.Error())
			return 1
		}
		item := &api.BackupRestoreItem{Type: api.BackupRestoreItemSecret, Namespace: namespace, ID: path,
			CheckIndex: index, WithMetadata: withMetadata}
		if secretVersion >= 0 {
			item.Version = pointer.Of(uint64(secretVersion))
		}
		items = append(items, item)
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
	switch {
	case full && len(items) > 0:
		c.Ui.Error("A full restore takes no -job, -variable, -secret, -restore-namespace, -acl-policy or -acl-role")
		return 1
	case !full && len(items) == 0:
		c.Ui.Error("Nothing to restore: use -job, -variable, -secret, -restore-namespace, -acl-policy, -acl-role or -full")
		return 1
	case !full && confirm != "":
		c.Ui.Error("-confirm is only used with -full")
		return 1
	case full && !dryRun && confirm == "":
		c.Ui.Error("Confirm the full restore with -confirm <region>, or preview it with -dry-run")
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

	if full {
		return c.runFull(client, flags.Args()[0], identity, destination, confirm, dryRun, json)
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
			case !dryRun && r.NewVersion > 0:
				result = fmt.Sprintf("restored as version %d at index %d", r.NewVersion, r.Index)
			case !dryRun:
				result = fmt.Sprintf("restored at index %d", r.Index)
			case r.NewVersion > 0:
				result = fmt.Sprintf("would %s as version %d", r.Action, r.NewVersion)
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
	switch {
	case item.Version != nil && item.Type == api.BackupRestoreItemSecret:
		s += fmt.Sprintf(" version %d", *item.Version)
	case item.Version != nil:
		s += fmt.Sprintf("@%d", *item.Version)
	}
	return s
}
