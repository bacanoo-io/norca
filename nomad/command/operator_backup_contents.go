// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/posener/complete"
)

var backupContentTypes = []string{
	api.BackupContentNamespaces, api.BackupContentJobs, api.BackupContentJobVersions,
	api.BackupContentVariables, api.BackupContentSecrets, api.BackupContentACLPolicies, api.BackupContentACLRoles,
	api.BackupContentDeployments, api.BackupContentAllocations, api.BackupContentConsulKV,
}

type OperatorBackupContentsCommand struct {
	Meta
}

func (c *OperatorBackupContentsCommand) Name() string { return "operator backup contents" }

func (c *OperatorBackupContentsCommand) Synopsis() string {
	return "Browse the content of a backup"
}

func (c *OperatorBackupContentsCommand) Help() string {
	helpText := `
Usage: norca operator backup contents [options] <id>

  Browse the content of a backup. The server downloads the backup, decrypts it
  with the given age identity and loads it into a separate in-memory state; the
  running cluster is not touched. Variables are listed with their key names
  only, never their values.

  Without -namespace, all namespaces are shown. Objects in namespaces the token
  can't read are hidden.

  If ACLs are enabled, this command requires a token with the 'read'
  capability of the 'backup' policy.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Contents Options:
` + backupIdentityUsage + `

  -type <type>
    The content to show: ` + strings.Join(backupContentTypes, ", ") + `.
    Defaults to jobs. job-versions requires -job.

  -job <id>
    Only show the job versions, deployments and allocations of this job.

  -prefix <prefix>
    Only show the Variables under this path, or the objects whose ID starts
    with it.

  -destination <name>
    The destination to read from. Defaults to the first one the backup was
    verified on.

  -json
    Output the content in JSON format.

  -t
    Format and display the content using a Go template.
`
	return strings.TrimSpace(helpText)
}

func (c *OperatorBackupContentsCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-age-identity": complete.PredictFiles("*"),
			"-type":         complete.PredictSet(backupContentTypes...),
			"-job":          complete.PredictAnything,
			"-prefix":       complete.PredictAnything,
			"-destination":  complete.PredictAnything,
			"-json":         complete.PredictNothing,
			"-t":            complete.PredictAnything,
		})
}

func (c *OperatorBackupContentsCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *OperatorBackupContentsCommand) Run(args []string) int {
	var json bool
	var identityPath, contentType, jobID, prefix, destination, tmpl string

	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.StringVar(&identityPath, "age-identity", "", "")
	flags.StringVar(&contentType, "type", api.BackupContentJobs, "")
	flags.StringVar(&jobID, "job", "", "")
	flags.StringVar(&prefix, "prefix", "", "")
	flags.StringVar(&destination, "destination", "", "")
	flags.BoolVar(&json, "json", false, "")
	flags.StringVar(&tmpl, "t", "", "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if len(flags.Args()) != 1 {
		c.Ui.Error("This command takes one argument: <id>")
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

	q := &api.QueryOptions{}
	if c.Meta.namespace == "" {
		q.Namespace = "*"
	}
	contents, _, err := client.Backups().Contents(flags.Args()[0], &api.BackupContentsOptions{
		Type: contentType, JobID: jobID, PathPrefix: prefix, Destination: destination, AgeIdentity: identity,
	}, q)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error browsing backup: %s", err))
		return 1
	}

	if json || tmpl != "" {
		out, err := Format(json, tmpl, contents)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error formatting output: %s", err))
			return 1
		}
		c.Ui.Output(out)
		return 0
	}

	out := formatBackupContents(contents)
	if len(out) <= 1 {
		c.Ui.Output(fmt.Sprintf("No %s found in backup %s", contents.Type, contents.BackupID))
		return 0
	}
	c.Ui.Output(formatList(out))
	return 0
}

func formatUnixNano(ns int64) string {
	if ns == 0 {
		return "-"
	}
	return formatTime(time.Unix(0, ns))
}

func formatBackupContents(c *api.BackupContents) []string {
	var out []string
	switch c.Type {
	case api.BackupContentNamespaces:
		out = append(out, "Name|Description")
		for _, ns := range c.Namespaces {
			out = append(out, fmt.Sprintf("%s|%s", ns.Name, ns.Description))
		}
	case api.BackupContentJobs:
		out = append(out, "ID|Namespace|Type|Status|Version|Versions")
		for _, j := range c.Jobs {
			versions := make([]string, 0, len(j.Versions))
			for _, v := range j.Versions {
				versions = append(versions, fmt.Sprint(v))
			}
			status := j.Status
			if j.Stop {
				status += " (stopped)"
			}
			out = append(out, fmt.Sprintf("%s|%s|%s|%s|%d|%s",
				j.ID, j.Namespace, j.Type, status, j.Version, strings.Join(versions, ",")))
		}
	case api.BackupContentJobVersions:
		out = append(out, "ID|Namespace|Version|Submit Time|Stop|Job Modify Index")
		for _, j := range c.JobVersions {
			out = append(out, fmt.Sprintf("%s|%s|%d|%s|%t|%d",
				*j.ID, *j.Namespace, *j.Version, formatUnixNano(*j.SubmitTime), *j.Stop, *j.JobModifyIndex))
		}
	case api.BackupContentVariables:
		out = append(out, "Namespace|Path|Keys|Modify Time")
		for _, v := range c.Variables {
			out = append(out, fmt.Sprintf("%s|%s|%s|%s",
				v.Namespace, v.Path, strings.Join(v.Keys, ","), formatUnixNano(v.ModifyTime)))
		}
	case api.BackupContentSecrets:
		out = append(out, "Namespace|Path|Version|Deleted|Value Hash|Modify Time")
		for _, s := range c.Secrets {
			out = append(out, fmt.Sprintf("%s|%s|%d|%t|%s|%s",
				s.Namespace, s.Path, s.CurrentVersion, s.Deleted, s.ValueHash, formatUnixNano(s.ModifyTime)))
		}
	case api.BackupContentACLPolicies:
		out = append(out, "Name|Description")
		for _, p := range c.ACLPolicies {
			out = append(out, fmt.Sprintf("%s|%s", p.Name, p.Description))
		}
	case api.BackupContentACLRoles:
		out = append(out, "Name|Description|Policies")
		for _, r := range c.ACLRoles {
			policies := make([]string, 0, len(r.Policies))
			for _, p := range r.Policies {
				policies = append(policies, p.Name)
			}
			out = append(out, fmt.Sprintf("%s|%s|%s", r.Name, r.Description, strings.Join(policies, ",")))
		}
	case api.BackupContentDeployments:
		out = append(out, "ID|Namespace|Job ID|Job Version|Status|Description")
		for _, d := range c.Deployments {
			out = append(out, fmt.Sprintf("%s|%s|%s|%d|%s|%s",
				d.ID, d.Namespace, d.JobID, d.JobVersion, d.Status, d.StatusDescription))
		}
	case api.BackupContentConsulKV:
		out = append(out, "Key|Size|Revision|Value Hash|Created")
		for _, e := range c.ConsulKV {
			out = append(out, fmt.Sprintf("%s|%d|%d|%s|%s",
				e.Key, e.Size, e.Revision, limit(e.SHA256, 12), formatTime(e.Created)))
		}
	case api.BackupContentAllocations:
		out = append(out, "ID|Namespace|Job ID|Job Version|Task Group|Node ID|Desired|Status|Modified")
		for _, a := range c.Allocations {
			out = append(out, fmt.Sprintf("%s|%s|%s|%d|%s|%s|%s|%s|%s",
				a.ID, a.Namespace, a.JobID, a.JobVersion, a.TaskGroup, limit(a.NodeID, 8),
				a.DesiredStatus, a.ClientStatus, formatUnixNano(a.ModifyTime)))
		}
	}
	return out
}
