// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/nomad/api"
	"github.com/posener/complete"
)

// maxPreviewObjects is the number of changed objects of a full restore
// preview shown in the human readable output.
const maxPreviewObjects = 50

// runFull requests (or with dryRun, previews) a full restore of the region.
func (c *OperatorBackupRestoreCommand) runFull(client *api.Client, id, identity, destination, confirm string,
	dryRun, json bool) int {

	resp, _, err := client.Backups().Restore(id, &api.BackupRestoreRequest{
		Mode:        api.BackupRestoreModeFull,
		DryRun:      dryRun,
		Confirm:     confirm,
		Destination: destination,
		AgeIdentity: identity,
	}, nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error requesting the full restore: %s", err))
		return 1
	}

	if json {
		out, err := Format(true, "", resp)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error formatting output: %s", err))
			return 1
		}
		c.Ui.Output(out)
		return 0
	}

	c.Ui.Output(c.Colorize().Color(formatBackupRestorePreview(resp.Preview)))
	if resp.Restore == nil {
		c.Ui.Output("\nDry run: nothing was requested.")
		return 0
	}
	c.Ui.Output(fmt.Sprintf("\nFull restore %q requested; it waits for approval until %s.",
		resp.Restore.ID, formatTime(resp.Restore.ExpiresAt)))
	c.Ui.Output(fmt.Sprintf("A second administrator must approve it with:\n\n    norca operator backup approve -age-identity <path> %s",
		resp.Restore.ID))
	return 0
}

// formatBackupRestorePreview formats the preview of a full restore.
func formatBackupRestorePreview(p *api.BackupRestorePreview) string {
	if p == nil {
		return ""
	}
	basic := []string{
		fmt.Sprintf("Backup|%s (Raft index %d)", p.BackupID, p.BackupIndex),
		fmt.Sprintf("Region|%s (current Raft index %d)", p.Region, p.CurrentIndex),
		fmt.Sprintf("Allocations Stopped|%d", p.AllocationsStopped),
		fmt.Sprintf("ACL Tokens Removed|%d", p.ACLTokensRemoved),
		fmt.Sprintf("Keys Added|%s", strings.Join(p.KeysAdded, ",")),
	}
	out := "[bold]Full Restore Preview[reset]\n" + formatKV(basic)

	if len(p.Counts) > 0 {
		keys := make([]string, 0, len(p.Counts))
		for k := range p.Counts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		counts := []string{"Type|Change|Count"}
		for _, k := range keys {
			typ, change, _ := strings.Cut(k, "/")
			counts = append(counts, fmt.Sprintf("%s|%s|%d", typ, change, p.Counts[k]))
		}
		out += "\n\n[bold]Changes[reset]\n" + formatList(counts)
	} else {
		out += "\n\nThe backup does not differ from the current state."
	}

	if len(p.Objects) > 0 {
		objects := []string{"Type|Change|Namespace|ID|Backup|Current"}
		for i, o := range p.Objects {
			if i == maxPreviewObjects {
				break
			}
			change := "reverted"
			switch o.Change {
			case "added":
				change = "removed"
			case "deleted":
				change = "restored"
			}
			objects = append(objects, fmt.Sprintf("%s|%s|%s|%s|%s|%s", o.Type, change, o.Namespace, o.ID,
				formatDiffSide(o.FromVersion, o.FromStatus), formatDiffSide(o.ToVersion, o.ToStatus)))
		}
		out += "\n\n[bold]Changed Objects[reset]\n" + formatList(objects)
		if len(p.Objects) > maxPreviewObjects || p.Truncated {
			out += "\n(more objects change; use -json for the full list)"
		}
	}

	if len(p.Warnings) > 0 {
		out += "\n\n[bold]Warnings[reset]"
		for _, w := range p.Warnings {
			out += "\n  * " + w
		}
	}
	return out
}

func formatDiffSide(version *uint64, status string) string {
	var parts []string
	if version != nil {
		parts = append(parts, fmt.Sprintf("v%d", *version))
	}
	if status != "" {
		parts = append(parts, status)
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, " ")
}

// formatBackupRestore formats a restore record.
func formatBackupRestore(r *api.BackupRestore) string {
	basic := []string{
		fmt.Sprintf("ID|%s", r.ID),
		fmt.Sprintf("Backup|%s", r.BackupID),
		fmt.Sprintf("Mode|%s", r.Mode),
		fmt.Sprintf("Status|%s", r.Status),
	}
	if r.StatusDescription != "" {
		basic = append(basic, fmt.Sprintf("Description|%s", r.StatusDescription))
	}
	requester := r.RequestedBy
	if r.RequestedByName != "" {
		requester += " (" + r.RequestedByName + ")"
	}
	basic = append(basic,
		fmt.Sprintf("Requested By|%s", requester),
		fmt.Sprintf("Requested|%s", formatTime(r.CreateTime)),
	)
	if r.Mode == api.BackupRestoreModeFull {
		basic = append(basic, fmt.Sprintf("Expires|%s", formatTime(r.ExpiresAt)))
	}
	if len(r.Approvals) > 0 {
		basic = append(basic,
			fmt.Sprintf("Approved By|%s", strings.Join(r.Approvals, ",")),
			fmt.Sprintf("Approved|%s", formatTime(r.ApprovedAt)))
	}
	if r.RejectedBy != "" {
		basic = append(basic, fmt.Sprintf("Rejected By|%s", r.RejectedBy))
	}
	if r.PreRestoreBackupID != "" {
		basic = append(basic, fmt.Sprintf("Pre-restore Backup|%s", r.PreRestoreBackupID))
	}
	basic = append(basic, fmt.Sprintf("Modified|%s", formatTime(r.ModifyTime)))
	out := formatKV(basic)
	if r.Preview != nil {
		out += "\n\n" + formatBackupRestorePreview(r.Preview)
	}
	return out
}

type OperatorBackupApproveCommand struct {
	Meta
}

func (c *OperatorBackupApproveCommand) Name() string { return "operator backup approve" }

func (c *OperatorBackupApproveCommand) Synopsis() string {
	return "Approve and run a pending full restore"
}

func (c *OperatorBackupApproveCommand) Help() string {
	helpText := `
Usage: norca operator backup approve [options] <restore_id>

  Approve a pending full restore of the region as the second administrator,
  and run it. Review the preview first with "norca operator backup restores
  <restore_id>". The requester can't approve their own request.

  The restore takes an automatic, pinned backup of the current state first
  (shown as the pre-restore backup, to undo the restore), adds the keys of
  the backup to the keystore of every server, and restores the Raft state of
  the backup. Every change since the backup is lost; clients stop the
  allocations that are not in the backup. The command waits until the
  restore has finished.

  This command requires a management token, different from the token of the
  request.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Approve Options:
` + backupIdentityUsage + `
`
	return strings.TrimSpace(helpText)
}

func (c *OperatorBackupApproveCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{"-age-identity": complete.PredictFiles("*")})
}

func (c *OperatorBackupApproveCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *OperatorBackupApproveCommand) Run(args []string) int {
	var identityPath string
	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.StringVar(&identityPath, "age-identity", "", "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if len(flags.Args()) != 1 {
		c.Ui.Error("This command takes one argument: <restore_id>")
		c.Ui.Error(commandErrorText(c))
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
	restore, _, err := client.Backups().ApproveRestore(flags.Args()[0],
		&api.BackupRestoreApproveRequest{AgeIdentity: identity}, nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error approving the full restore: %s", err))
		return 1
	}
	restore.Preview = nil
	c.Ui.Output(c.Colorize().Color(formatBackupRestore(restore)))
	if restore.Status != api.BackupRestoreStatusComplete {
		return 1
	}
	return 0
}

type OperatorBackupRejectCommand struct {
	Meta
}

func (c *OperatorBackupRejectCommand) Name() string { return "operator backup reject" }

func (c *OperatorBackupRejectCommand) Synopsis() string {
	return "Reject a pending full restore"
}

func (c *OperatorBackupRejectCommand) Help() string {
	helpText := `
Usage: norca operator backup reject [options] <restore_id>

  Reject a pending full restore of the region. The requester can withdraw
  their own request this way.

  This command requires a management token.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Reject Options:

  -reason <text>
    The reason of the rejection, kept in the restore record.
`
	return strings.TrimSpace(helpText)
}

func (c *OperatorBackupRejectCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{"-reason": complete.PredictAnything})
}

func (c *OperatorBackupRejectCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *OperatorBackupRejectCommand) Run(args []string) int {
	var reason string
	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.StringVar(&reason, "reason", "", "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if len(flags.Args()) != 1 {
		c.Ui.Error("This command takes one argument: <restore_id>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}
	restore, _, err := client.Backups().RejectRestore(flags.Args()[0],
		&api.BackupRestoreRejectRequest{Reason: reason}, nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error rejecting the full restore: %s", err))
		return 1
	}
	c.Ui.Output(fmt.Sprintf("Full restore %q %s", restore.ID, restore.Status))
	return 0
}

type OperatorBackupRestoresCommand struct {
	Meta
}

func (c *OperatorBackupRestoresCommand) Name() string { return "operator backup restores" }

func (c *OperatorBackupRestoresCommand) Synopsis() string {
	return "List the restores of a region, or show one with its preview"
}

func (c *OperatorBackupRestoresCommand) Help() string {
	helpText := `
Usage: norca operator backup restores [options] [<restore_id>]

  Without an argument, list the selective and full restores of the region,
  newest first, with full restores waiting for approval. With a restore ID,
  show the restore with the preview of a full restore: the audit trail of
  who requested, approved or rejected it.

  If ACLs are enabled, this command requires a token with the 'read'
  capability of the 'backup' policy.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Restores Options:

  -json
    Output in JSON format.
`
	return strings.TrimSpace(helpText)
}

func (c *OperatorBackupRestoresCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{"-json": complete.PredictNothing})
}

func (c *OperatorBackupRestoresCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *OperatorBackupRestoresCommand) Run(args []string) int {
	var json bool
	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.BoolVar(&json, "json", false, "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if len(flags.Args()) > 1 {
		c.Ui.Error("This command takes at most one argument: <restore_id>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}

	var data any
	if len(flags.Args()) == 1 {
		restore, _, err := client.Backups().GetRestore(flags.Args()[0], nil)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error reading the restore: %s", err))
			return 1
		}
		if !json {
			c.Ui.Output(c.Colorize().Color(formatBackupRestore(restore)))
			return 0
		}
		data = restore
	} else {
		restores, _, err := client.Backups().ListRestores(nil)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error listing the restores: %s", err))
			return 1
		}
		if !json {
			if len(restores) == 0 {
				c.Ui.Output("No restores")
				return 0
			}
			out := []string{"ID|Backup|Mode|Status|Requested By|Requested|Expires"}
			for _, r := range restores {
				expires := "-"
				if r.Status == api.BackupRestoreStatusPending {
					expires = formatTime(r.ExpiresAt)
				}
				out = append(out, fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s", r.ID, r.BackupID, r.Mode, r.Status,
					r.RequestedBy, formatTime(r.CreateTime), expires))
			}
			c.Ui.Output(formatList(out))
			return 0
		}
		data = restores
	}
	out, err := Format(true, "", data)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error formatting output: %s", err))
		return 1
	}
	c.Ui.Output(out)
	return 0
}
