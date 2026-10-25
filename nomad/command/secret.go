// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/mitchellh/cli"
)

type SecretCommand struct {
	Meta
}

func (c *SecretCommand) Help() string {
	helpText := `
Usage: norca secret <subcommand> [options] [args]

  This command groups subcommands for the secrets of the built-in secret
  store. Secrets are versioned key/value items kept encrypted on the servers.
  Their values are only returned by an explicit, audited read; listings, the
  metadata and the event stream show paths, metadata and a keyed value hash.

  Write a secret, reading the password from a file:

      $ norca secret put prod/db/postgres username=app password=@pw.txt

  List the secrets below a prefix:

      $ norca secret list prod/

  Show the metadata and versions of a secret:

      $ norca secret info prod/db/postgres

  Print one item of the current version:

      $ norca secret get -item password prod/db/postgres

  Please see the individual subcommand help for detailed usage information.
`
	return strings.TrimSpace(helpText)
}

func (c *SecretCommand) Synopsis() string {
	return "Interact with secrets"
}

func (c *SecretCommand) Name() string { return "secret" }

func (c *SecretCommand) Run(args []string) int {
	return cli.RunResultHelp
}

// parseSecretVersions parses a comma separated list of versions.
func parseSecretVersions(s string) ([]uint64, error) {
	if s == "" {
		return nil, nil
	}
	var out []uint64
	for _, part := range strings.Split(s, ",") {
		v, err := strconv.ParseUint(strings.TrimSpace(part), 10, 64)
		if err != nil || v == 0 {
			return nil, fmt.Errorf("invalid version %q", part)
		}
		out = append(out, v)
	}
	return out, nil
}

// optionalCheckIndex returns the check index of a -check-index flag, nil if
// it was not set (negative).
func optionalCheckIndex(index int64) *uint64 {
	if index < 0 {
		return nil
	}
	v := uint64(index)
	return &v
}

// formatSecretTime formats an optional time.
func formatSecretTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "<none>"
	}
	return formatTime(*t)
}

// formatSecretLabels formats labels as k=v pairs.
func formatSecretLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return "<none>"
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + labels[k]
	}
	return strings.Join(parts, ",")
}

// formatSecretMetadata renders the metadata of a secret, never values.
func formatSecretMetadata(s *api.Secret) string {
	rotate := "<none>"
	if s.RotateEvery > 0 {
		rotate = s.RotateEvery.String()
	}
	out := []string{
		fmt.Sprintf("Namespace|%s", s.Namespace),
		fmt.Sprintf("Path|%s", s.Path),
		fmt.Sprintf("Current Version|%d", s.CurrentVersion),
		fmt.Sprintf("Deleted|%t", s.Deleted),
		fmt.Sprintf("Destroyed|%t", s.Destroyed),
		fmt.Sprintf("Items|%s", strings.Join(s.ItemKeys, ",")),
		fmt.Sprintf("Value Hash|%s", s.ValueHash),
		fmt.Sprintf("Description|%s", s.Description),
		fmt.Sprintf("Owner|%s", s.Owner),
		fmt.Sprintf("Labels|%s", formatSecretLabels(s.Labels)),
		fmt.Sprintf("Expires At|%s", formatSecretTime(s.ExpiresAt)),
		fmt.Sprintf("Rotate Every|%s", rotate),
		fmt.Sprintf("Max Versions|%d", s.MaxVersions),
		fmt.Sprintf("Create Time|%s", formatUnixNanoTime(s.CreateTime)),
		fmt.Sprintf("Modify Time|%s", formatUnixNanoTime(s.ModifyTime)),
		fmt.Sprintf("Modify Index|%d", s.ModifyIndex),
	}
	return formatKV(out)
}

// formatSecretVersions renders the versions of a secret.
func formatSecretVersions(s *api.Secret) string {
	rows := []string{"Version|Created|Deleted|Destroyed|Value Hash"}
	for i := len(s.Versions) - 1; i >= 0; i-- {
		v := s.Versions[i]
		rows = append(rows, fmt.Sprintf("%d|%s|%t|%t|%s",
			v.Version, formatUnixNanoTime(v.CreateTime), v.Deleted, v.Destroyed, v.ValueHash))
	}
	return formatList(rows)
}
