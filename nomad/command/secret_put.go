// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"
	flaghelper "github.com/hashicorp/nomad/helper/flags"
	"github.com/posener/complete"
)

type SecretPutCommand struct {
	Meta

	// stdin replaces os.Stdin in tests.
	stdin io.Reader
}

func (c *SecretPutCommand) Name() string { return "secret put" }

func (c *SecretPutCommand) Synopsis() string {
	return "Write a secret"
}

func (c *SecretPutCommand) Help() string {
	helpText := `
Usage: norca secret put [options] <path> [<item>=<value> | <item>=@<file> | <item>=- | -]...

  Write a new version of a secret, and optionally its metadata. Items are
  given as arguments:

    <item>=@<file>   the value is the content of the file
    <item>=-         the value is read from standard input
    -                all items are read from standard input, as a JSON object
    <item>=<value>   the value is the argument itself

  Values given as plain arguments end up in the shell history and the
  process list; prefer @<file> or standard input. Without items, only the
  metadata of an existing secret is updated. The metadata flags change only
  the fields they set; the other fields are kept.

  If ACLs are enabled, this command requires the 'write-secret' capability
  for the path.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Put Options:

  -check-index <index>
    Write only if the secret's modify index is the given one; 0 writes only
    if the secret does not exist yet.

  -description <text>
    Description of the secret.

  -owner <name>
    Person or team responsible for the secret.

  -label <key>=<value>
    Label of the secret; can be repeated.

  -expires-at <time>
    When the value expires: an RFC 3339 time, or a duration from now
    (for example "720h").

  -rotate-every <duration>
    How often the value should be rotated.

  -max-versions <n>
    Number of versions kept (default 10); older versions are destroyed.
`
	return strings.TrimSpace(helpText)
}

func (c *SecretPutCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-check-index":  complete.PredictAnything,
			"-description":  complete.PredictAnything,
			"-owner":        complete.PredictAnything,
			"-label":        complete.PredictAnything,
			"-expires-at":   complete.PredictAnything,
			"-rotate-every": complete.PredictAnything,
			"-max-versions": complete.PredictAnything,
		})
}

func (c *SecretPutCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictAnything
}

func (c *SecretPutCommand) Run(args []string) int {
	var checkIndex int64
	var maxVersions int
	var description, owner, expiresAt, rotateEvery string
	var labels flaghelper.StringFlag

	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.Int64Var(&checkIndex, "check-index", -1, "")
	flags.StringVar(&description, "description", "", "")
	flags.StringVar(&owner, "owner", "", "")
	flags.Var(&labels, "label", "")
	flags.StringVar(&expiresAt, "expires-at", "", "")
	flags.StringVar(&rotateEvery, "rotate-every", "", "")
	flags.IntVar(&maxVersions, "max-versions", 0, "")
	if err := flags.Parse(args); err != nil {
		return 1
	}

	args = flags.Args()
	if len(args) < 1 {
		c.Ui.Error("This command takes at least one argument: <path>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}
	path := args[0]

	items, plain, err := c.parseItems(args[1:])
	if err != nil {
		c.Ui.Error(err.Error())
		return 1
	}
	if plain {
		c.Ui.Warn("Warning: values given as <item>=<value> arguments can be recorded in the shell " +
			"history and the process list; use <item>=@<file> or standard input instead")
	}

	set := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { set[f.Name] = true })

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}

	write := &api.SecretWrite{Items: items}
	metaFlags := set["description"] || set["owner"] || set["label"] || set["expires-at"] ||
		set["rotate-every"] || set["max-versions"]
	if metaFlags {
		// Start from the existing metadata, so that only the flags given
		// change it.
		meta := &api.SecretMeta{}
		existing, _, err := client.Secrets().Info(path, nil)
		switch {
		case err == nil:
			m := existing.SecretMeta
			meta = &m
		case !errors.Is(err, api.ErrSecretNotFound):
			c.Ui.Error(fmt.Sprintf("Error reading secret metadata: %s", err))
			return 1
		}
		if set["description"] {
			meta.Description = description
		}
		if set["owner"] {
			meta.Owner = owner
		}
		if set["label"] {
			if meta.Labels == nil {
				meta.Labels = map[string]string{}
			}
			for _, l := range labels {
				k, v, ok := strings.Cut(l, "=")
				if !ok || k == "" {
					c.Ui.Error(fmt.Sprintf("Invalid label %q, must be <key>=<value>", l))
					return 1
				}
				meta.Labels[k] = v
			}
		}
		if set["expires-at"] {
			t, err := parseSecretExpiry(expiresAt, time.Now())
			if err != nil {
				c.Ui.Error(err.Error())
				return 1
			}
			meta.ExpiresAt = t
		}
		if set["rotate-every"] {
			d, err := time.ParseDuration(rotateEvery)
			if err != nil {
				c.Ui.Error(fmt.Sprintf("Invalid -rotate-every: %s", err))
				return 1
			}
			meta.RotateEvery = d
		}
		if set["max-versions"] {
			meta.MaxVersions = maxVersions
		}
		write.Meta = meta
	}
	if len(items) == 0 && write.Meta == nil {
		c.Ui.Error("Nothing to write: give at least one item or a metadata flag")
		c.Ui.Error(commandErrorText(c))
		return 1
	}

	var secret *api.Secret
	if index := optionalCheckIndex(checkIndex); index != nil {
		secret, _, err = client.Secrets().CheckedWrite(path, *index, write, nil)
	} else {
		secret, _, err = client.Secrets().Write(path, write, nil)
	}
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error writing secret: %s", err))
		return 1
	}
	if len(items) > 0 {
		c.Ui.Output(fmt.Sprintf("Wrote secret %q version %d", secret.Path, secret.CurrentVersion))
	} else {
		c.Ui.Output(fmt.Sprintf("Updated the metadata of secret %q", secret.Path))
	}
	return 0
}

// parseItems parses the item arguments. plain reports values given as plain
// arguments.
func (c *SecretPutCommand) parseItems(args []string) (map[string]string, bool, error) {
	items := map[string]string{}
	plain := false
	stdinUsed := false
	readStdin := func() ([]byte, error) {
		if stdinUsed {
			return nil, errors.New("standard input can only be used once")
		}
		stdinUsed = true
		in := c.stdin
		if in == nil {
			in = os.Stdin
		}
		return io.ReadAll(in)
	}

	for _, arg := range args {
		if arg == "-" {
			data, err := readStdin()
			if err != nil {
				return nil, false, err
			}
			var fromStdin map[string]string
			if err := json.Unmarshal(data, &fromStdin); err != nil {
				return nil, false, fmt.Errorf("standard input must be a JSON object of string items: %w", err)
			}
			for k, v := range fromStdin {
				items[k] = v
			}
			continue
		}
		key, value, ok := strings.Cut(arg, "=")
		if !ok || key == "" {
			return nil, false, fmt.Errorf("invalid item %q, must be <item>=<value>, <item>=@<file>, <item>=- or -", arg)
		}
		switch {
		case value == "-":
			data, err := readStdin()
			if err != nil {
				return nil, false, err
			}
			items[key] = string(data)
		case strings.HasPrefix(value, "@"):
			data, err := os.ReadFile(value[1:])
			if err != nil {
				return nil, false, fmt.Errorf("error reading item %q: %w", key, err)
			}
			items[key] = string(data)
		default:
			items[key] = value
			plain = true
		}
	}
	return items, plain, nil
}

// parseSecretExpiry parses an RFC 3339 time or a duration from now.
func parseSecretExpiry(s string, now time.Time) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return &t, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return nil, fmt.Errorf("invalid -expires-at %q: must be an RFC 3339 time or a duration", s)
	}
	t := now.Add(d).UTC().Truncate(time.Second)
	return &t, nil
}
