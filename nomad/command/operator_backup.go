// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	humanize "github.com/dustin/go-humanize"
	"github.com/hashicorp/nomad/api"
	"github.com/mitchellh/cli"
)

type OperatorBackupCommand struct {
	Meta
}

func (c *OperatorBackupCommand) Name() string { return "operator backup" }

func (c *OperatorBackupCommand) Synopsis() string {
	return "List, browse and restore the scheduled backups of a region"
}

func (c *OperatorBackupCommand) Help() string {
	helpText := `
Usage: norca operator backup <subcommand> [options] [args]

  This command groups subcommands for the backups that the leader of a region
  takes into S3-compatible stores and directories, as configured in the
  server's backup block.
  Backups are encrypted with age; browsing, comparing and restoring them needs
  an age identity (private key) given with -age-identity. The identity is sent
  to the server for the request only and is never stored.

  List the backups and the state of the schedule:

      $ norca operator backup list

  Take a manual, pinned backup:

      $ norca operator backup save -pin -label note="before upgrade"

  Browse the jobs of a backup:

      $ norca operator backup contents -age-identity key.txt <id>

  Compare a backup with the current state:

      $ norca operator backup diff -age-identity key.txt <id>

  Restore version 3 of a deleted job and a Variables entry:

      $ norca operator backup restore -age-identity key.txt \
          -job example@3 -variable nomad/jobs/example <id>

  Request a full restore of the region, which a second administrator
  approves (with another management token) to run it:

      $ norca operator backup restore -full -age-identity key.txt \
          -confirm <region> <id>
      $ norca operator backup restores <restore_id>
      $ norca operator backup approve -age-identity key.txt <restore_id>

  Build a new cluster from a backup, on its first new server:

      $ norca operator backup restore-bootstrap -age-identity key.txt \
          -from s3://<bucket>/<prefix>/<yyyy>/<mm>/<dd>/<id>/

  or from a copy of the folder of a directory destination:

      $ norca operator backup restore-bootstrap -age-identity key.txt \
          -from file:///<path>/<prefix>/<yyyy>/<mm>/<dd>/<id>/

  Please see the individual subcommand help for detailed usage information.
`
	return strings.TrimSpace(helpText)
}

func (c *OperatorBackupCommand) Run(args []string) int {
	return cli.RunResultHelp
}

// backupIdentityUsage is the help text of the -age-identity flag.
const backupIdentityUsage = `
  -age-identity <path>
    Path of an age identity file holding the private key the backup is
    encrypted for. Use "-" to read it from stdin. The identity is sent to the
    server only for this request and is never stored.`

// readAgeIdentity reads an age identity file; "-" reads stdin.
func readAgeIdentity(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("an age identity is required: use -age-identity <path>")
	}
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return "", fmt.Errorf("failed to read the age identity: %w", err)
	}
	return string(data), nil
}

// parseBackupLabels parses key=value labels.
func parseBackupLabels(in []string) (map[string]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	labels := make(map[string]string, len(in))
	for _, l := range in {
		k, v, ok := strings.Cut(l, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid label %q, must be key=value", l)
		}
		labels[k] = v
	}
	return labels, nil
}

func formatBackupLabels(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+labels[k])
	}
	return strings.Join(parts, ",")
}

func formatBackupSize(b *api.Backup) string {
	for _, d := range b.Destinations {
		if obj := d.Objects[api.BackupObjectState]; obj != nil {
			return humanize.IBytes(uint64(obj.Size))
		}
	}
	return "-"
}

func formatBackupDestinations(b *api.Backup) string {
	parts := make([]string, 0, len(b.Destinations))
	for _, d := range b.Destinations {
		parts = append(parts, d.Name+":"+d.Status)
	}
	return strings.Join(parts, ",")
}

func formatBackupList(backups []*api.Backup) string {
	out := make([]string, len(backups)+1)
	out[0] = "ID|Started|Trigger|Status|Raft Index|Size|Destinations|Pinned|Labels"
	for i, b := range backups {
		out[i+1] = fmt.Sprintf("%s|%s|%s|%s|%d|%s|%s|%t|%s",
			b.ID, formatTime(b.StartedAt), b.Trigger, b.Status, b.RaftIndex, formatBackupSize(b),
			formatBackupDestinations(b), b.Pinned, formatBackupLabels(b.Labels))
	}
	return formatList(out)
}

func formatBackupSchedule(s *api.BackupScheduleStatus) string {
	if s == nil {
		return ""
	}
	every := s.Schedule
	if every == "" {
		every = s.Interval.String()
	}
	if !s.Enabled {
		every = "disabled"
	}
	out := []string{
		fmt.Sprintf("Schedule|%s", every),
		fmt.Sprintf("Destinations|%s", strings.Join(s.Destinations, ",")),
	}
	if !s.LastSuccess.IsZero() {
		out = append(out, fmt.Sprintf("Last Success|%s (%s)", formatTime(s.LastSuccess), s.LastSuccessID))
	}
	if s.LastError != "" {
		out = append(out, fmt.Sprintf("Last Error|%s: %s", formatTime(s.LastAttempt), s.LastError))
	}
	if !s.NextDue.IsZero() {
		out = append(out, fmt.Sprintf("Next Due|%s", formatTime(s.NextDue)))
	}
	if s.Overdue {
		out = append(out, "Warning|the last successful backup is older than two intervals")
	}
	return formatKV(out)
}

func formatBackup(b *api.Backup) string {
	basic := []string{
		fmt.Sprintf("ID|%s", b.ID),
		fmt.Sprintf("Region|%s", b.Region),
		fmt.Sprintf("Trigger|%s", b.Trigger),
		fmt.Sprintf("Status|%s", b.Status),
	}
	if b.StatusDescription != "" {
		basic = append(basic, fmt.Sprintf("Description|%s", b.StatusDescription))
	}
	basic = append(basic,
		fmt.Sprintf("Started|%s", formatTime(b.StartedAt)),
		fmt.Sprintf("Completed|%s", formatTime(b.CompletedAt)),
		fmt.Sprintf("Norca Version|%s", b.NorcaVersion),
		fmt.Sprintf("Raft Index|%d", b.RaftIndex),
		fmt.Sprintf("Raft Term|%d", b.RaftTerm),
		fmt.Sprintf("Leader|%s", b.LeaderID),
		fmt.Sprintf("Path|%s", b.Path),
		fmt.Sprintf("Key IDs|%s", strings.Join(b.KeyIDs, ",")),
		fmt.Sprintf("Pinned|%t", b.Pinned),
		fmt.Sprintf("Labels|%s", formatBackupLabels(b.Labels)),
	)
	out := formatKV(basic)

	if len(b.Summary) > 0 {
		keys := make([]string, 0, len(b.Summary))
		for k := range b.Summary {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		summary := make([]string, 0, len(keys))
		for _, k := range keys {
			summary = append(summary, fmt.Sprintf("%s|%d", k, b.Summary[k]))
		}
		out += "\n\n[bold]Summary[reset]\n" + formatKV(summary)
	}

	dests := []string{"Name|Status|Location|Size|Encryption|Duration|Error"}
	for _, d := range b.Destinations {
		size := "-"
		if obj := d.Objects[api.BackupObjectState]; obj != nil {
			size = humanize.IBytes(uint64(obj.Size))
		}
		dests = append(dests, fmt.Sprintf("%s|%s|%s|%s|%s %s|%s|%s",
			d.Name, d.Status, d.Location, size, d.EncryptionMode, d.RecipientsFingerprint,
			d.Duration.Round(time.Millisecond), d.Error))
	}
	out += "\n\n[bold]Destinations[reset]\n" + formatList(dests)
	return out
}
