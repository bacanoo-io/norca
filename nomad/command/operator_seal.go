// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/posener/complete"
	"golang.org/x/crypto/ssh/terminal"
)

// OperatorSealStatusCommand shows the keyring seal status of the servers.
type OperatorSealStatusCommand struct {
	Meta
}

func (c *OperatorSealStatusCommand) Help() string {
	helpText := `
Usage: norca operator seal-status [options]

  Show the keyring seal of the region and, for every server, whether it is
  sealed, the progress of its unseal and the state of the secret store core.
  A sealed server serves everything that needs no decryption and rejects
  Variables, workload identity signing, mTLS issuance and the secret store.

  If ACLs are enabled, this command requires a token with operator:read.

General Options:

  ` + generalOptionsUsage(usageOptsDefault|usageOptsNoNamespace) + `

Seal Status Options:

  -server-id <name or ID>
    Show only this server.

  -json
    Output the status in JSON format.

  -t
    Format and display the status using a Go template.
`
	return strings.TrimSpace(helpText)
}

func (c *OperatorSealStatusCommand) Synopsis() string {
	return "Show the keyring seal status of the servers"
}

func (c *OperatorSealStatusCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-server-id": complete.PredictAnything,
			"-json":      complete.PredictNothing,
			"-t":         complete.PredictAnything,
		})
}

func (c *OperatorSealStatusCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *OperatorSealStatusCommand) Name() string { return "operator seal-status" }

func (c *OperatorSealStatusCommand) Run(args []string) int {
	var serverID, tmpl string
	var json bool
	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.StringVar(&serverID, "server-id", "", "")
	flags.BoolVar(&json, "json", false, "")
	flags.StringVar(&tmpl, "t", "", "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if len(flags.Args()) != 0 {
		c.Ui.Error("This command takes no arguments")
		c.Ui.Error(commandErrorText(c))
		return 1
	}

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}
	status, _, err := client.Operator().SealStatus(serverID, nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error reading the seal status: %s", err))
		return 1
	}
	if json || tmpl != "" {
		out, err := Format(json, tmpl, status)
		if err != nil {
			c.Ui.Error(err.Error())
			return 1
		}
		c.Ui.Output(out)
		return 0
	}
	c.Ui.Output(formatSealStatus(status))
	return 0
}

// formatSealStatus renders the seal status of a region.
func formatSealStatus(status *api.SealStatusResponse) string {
	var b strings.Builder
	cfg := status.Config
	seals := "aead (implicit: no seal configuration)"
	if cfg != nil {
		seals = formatSealInfos(cfg.Seals)
		if seals == "" {
			seals = "none (not initialized)"
		}
	}
	kv := []string{"Region seals|" + seals}
	if cfg != nil && cfg.Rewrap != nil {
		kv = append(kv,
			"Rewrap|"+cfg.Rewrap.ID+" to "+formatSealInfos(cfg.Rewrap.To),
			fmt.Sprintf("Rewrap started|%s", time.Unix(0, cfg.Rewrap.StartTime).UTC().Format(time.RFC3339)),
			fmt.Sprintf("Servers done|%d (%s)", len(cfg.Rewrap.Done), strings.Join(cfg.Rewrap.Done, ", ")))
	}
	b.WriteString(formatKV(kv))
	b.WriteString("\n\n")

	rows := []string{"Name|Leader|Sealed|Seal|Unseal Progress|Secret Store|Configured Seal"}
	for _, s := range status.Servers {
		if s.Error != "" {
			rows = append(rows, fmt.Sprintf("%s|-|-|-|-|-|error: %s", s.Name, s.Error))
			continue
		}
		progress := "-"
		if s.Threshold > 0 && s.Sealed {
			progress = fmt.Sprintf("%d/%d", s.Progress, s.Threshold)
		}
		core := "not initialized"
		switch {
		case s.SecretCoreActive:
			core = "active"
		case s.SecretCoreInitialized && s.SecretCoreSealed:
			core = "sealed"
		case s.SecretCoreInitialized:
			core = "standby"
		}
		seal := strings.Join(s.Seals, ",")
		if !s.Initialized {
			seal = "not initialized"
		}
		rows = append(rows, fmt.Sprintf("%s|%t|%t|%s|%s|%s|%s",
			s.Name, s.Leader, s.Sealed, seal, progress, core, s.ConfiguredSeal))
	}
	b.WriteString(formatList(rows))
	return b.String()
}

func formatSealInfos(seals []*api.SealInfo) string {
	parts := make([]string, 0, len(seals))
	for _, s := range seals {
		p := s.Type
		if s.Type == api.SealTypeShares {
			p = fmt.Sprintf("shares (%d of %d, key %s)", s.Threshold, s.Shares, shortID(s.KeyID))
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, ", ")
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// OperatorUnsealCommand enters one unseal share on a server.
type OperatorUnsealCommand struct {
	Meta

	// testStdin replaces os.Stdin in tests.
	testStdin io.Reader
}

func (c *OperatorUnsealCommand) Help() string {
	helpText := `
Usage: norca operator unseal [options]

  Enter one unseal share of the shares seal on a server. A server started
  with the shares seal is sealed until a threshold of distinct shares has
  been entered; every server is unsealed on its own.

  The share is read from standard input, or prompted for without echo when
  standard input is a terminal. It is never taken from the command line, so
  it does not end up in the shell history.

  If ACLs are enabled, this command requires a token with operator:write.

General Options:

  ` + generalOptionsUsage(usageOptsDefault|usageOptsNoNamespace) + `

Unseal Options:

  -server-id <name or ID>
    The server to unseal. Defaults to the server of the agent at -address.

  -reset
    Discard the shares entered so far on the server.
`
	return strings.TrimSpace(helpText)
}

func (c *OperatorUnsealCommand) Synopsis() string {
	return "Enter an unseal share on a server"
}

func (c *OperatorUnsealCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-server-id": complete.PredictAnything,
			"-reset":     complete.PredictNothing,
		})
}

func (c *OperatorUnsealCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *OperatorUnsealCommand) Name() string { return "operator unseal" }

func (c *OperatorUnsealCommand) Run(args []string) int {
	var serverID string
	var reset bool
	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.StringVar(&serverID, "server-id", "", "")
	flags.BoolVar(&reset, "reset", false, "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if rest := flags.Args(); len(rest) > 1 || (len(rest) == 1 && rest[0] != "-") {
		c.Ui.Error("This command takes no arguments: the share is read from standard input or a prompt")
		c.Ui.Error(commandErrorText(c))
		return 1
	}

	req := &api.UnsealRequest{ServerID: serverID, Reset: reset}
	if !reset {
		share, err := c.readShare()
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error reading the unseal share: %s", err))
			return 1
		}
		req.Key = share
	}

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}
	st, _, err := client.Operator().Unseal(req, nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error unsealing: %s", err))
		return 1
	}
	switch {
	case !st.Sealed:
		c.Ui.Output(fmt.Sprintf("Server %q is unsealed", st.Name))
	case reset:
		c.Ui.Output(fmt.Sprintf("Unseal progress of server %q reset", st.Name))
	default:
		c.Ui.Output(fmt.Sprintf("Server %q is sealed: %d of %d shares entered", st.Name, st.Progress, st.Threshold))
	}
	return 0
}

// readShare reads one share from standard input or a prompt without echo.
func (c *OperatorUnsealCommand) readShare() (string, error) {
	var in io.Reader = os.Stdin
	if c.testStdin != nil {
		in = c.testStdin
	} else if terminal.IsTerminal(int(os.Stdin.Fd())) {
		share, err := c.Ui.AskSecret("Unseal share (will be hidden):")
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(share) == "" {
			return "", fmt.Errorf("no share entered")
		}
		return strings.TrimSpace(share), nil
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return "", fmt.Errorf("no share on standard input")
	}
	return line, nil
}

// OperatorKeyringRewrapCommand moves the keys of the region to another
// seal.
type OperatorKeyringRewrapCommand struct {
	Meta
}

func (c *OperatorKeyringRewrapCommand) Help() string {
	helpText := `
Usage: norca operator keyring rewrap -to <seal> [options]

  Move the keys of the region to another keyring seal, online: every server
  adds wraps of the new seal to its root keys and to the key of the secret
  store, and once every server is done the wraps of the old seal are dropped.
  Until then both seals unwrap the keys. Follow the progress with
  'norca operator seal-status'.

  Rewrapping to "shares" generates a new unseal key and prints its shares
  once: they are stored nowhere, hand them to the share holders. In a new
  region whose servers are configured with seal "shares", the first rewrap
  to shares initializes the keyring.

  A rewrap does not rotate the keys: after leaving the aead seal, rotate the
  root keys (norca operator root keyring rotate -full), since copies of the
  old key files or backups may still hold the keys unprotected.

  If ACLs are enabled, this command requires a token with operator:write.

General Options:

  ` + generalOptionsUsage(usageOptsDefault|usageOptsNoNamespace) + `

Rewrap Options:

  -to <seal>
    The seal to move to: shares or aead.

  -shares <n>
    The number of unseal shares of a new shares seal. Defaults to the
    leader's configuration, or 5.

  -threshold <n>
    The number of shares that unseal a server. Defaults to the leader's
    configuration, or 3.
`
	return strings.TrimSpace(helpText)
}

func (c *OperatorKeyringRewrapCommand) Synopsis() string {
	return "Move the keyring to another seal"
}

func (c *OperatorKeyringRewrapCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-to":        complete.PredictSet(api.SealTypeShares, api.SealTypeAEAD),
			"-shares":    complete.PredictAnything,
			"-threshold": complete.PredictAnything,
		})
}

func (c *OperatorKeyringRewrapCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *OperatorKeyringRewrapCommand) Name() string { return "operator keyring rewrap" }

func (c *OperatorKeyringRewrapCommand) Run(args []string) int {
	var to string
	var shares, threshold int
	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.StringVar(&to, "to", "", "")
	flags.IntVar(&shares, "shares", 0, "")
	flags.IntVar(&threshold, "threshold", 0, "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if len(flags.Args()) != 0 || to == "" {
		c.Ui.Error("This command requires -to and takes no arguments")
		c.Ui.Error(commandErrorText(c))
		return 1
	}

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}
	resp, _, err := client.Keyring().Rewrap(&api.KeyringRewrapRequest{
		To: to, Shares: shares, Threshold: threshold,
	}, nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error starting the rewrap: %s", err))
		return 1
	}

	c.Ui.Output(fmt.Sprintf("Rewrap %s to %q started", resp.RewrapID, to))
	if len(resp.Shares) > 0 {
		t := threshold
		if resp.Config != nil && resp.Config.Rewrap != nil && len(resp.Config.Rewrap.To) > 0 {
			t = resp.Config.Rewrap.To[0].Threshold
		}
		c.Ui.Output("")
		for i, s := range resp.Shares {
			c.Ui.Output(fmt.Sprintf("Unseal share %d: %s", i+1, s))
		}
		c.Ui.Output("")
		c.Ui.Warn(fmt.Sprintf("The unseal shares are shown only this once and are stored nowhere. A restarted\n"+
			"server stays sealed until %d of them are entered with 'norca operator unseal'.\n"+
			"Losing more than %d of the %d shares makes the keys unrecoverable.",
			t, len(resp.Shares)-t, len(resp.Shares)))
	}
	c.Ui.Output("Follow the progress with 'norca operator seal-status'.")
	return 0
}
