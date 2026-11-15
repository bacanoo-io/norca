// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/mitchellh/cli"
	"github.com/posener/complete"
)

type CertificateCACommand struct {
	Meta
}

func (c *CertificateCACommand) Name() string { return "certificate ca" }

func (c *CertificateCACommand) Run(args []string) int {
	return cli.RunResultHelp
}

func (c *CertificateCACommand) Synopsis() string {
	return "Provides tools for the issuing CA of the built-in certificate issuer"
}

func (c *CertificateCACommand) Help() string {
	helpText := `
Usage: norca certificate ca <subcommand> [options]

  This command groups subcommands for the issuing CA of the region: the CA
  that signs the certificates of the built-in issuer. The leader creates it
  with the first built-in CertificateIssuer and rotates it at two thirds of
  its life.

  Bring a CA of the organisation in its place:

      $ norca certificate ca import -cert=ca.pem -key=ca-key.pem

  Replace it with a new CA of the leader:

      $ norca certificate ca rotate

  Please see the individual subcommand help for detailed usage information.
  `
	return strings.TrimSpace(helpText)
}

func formatCertificateAuthority(ca *api.CertificateAuthority) string {
	origin := "created by the leader"
	if ca.Imported {
		origin = "imported"
	}
	return formatKV([]string{
		fmt.Sprintf("ID|%s", ca.ID),
		fmt.Sprintf("Region|%s", ca.Region),
		fmt.Sprintf("Origin|%s", origin),
		fmt.Sprintf("Not Before|%s", ca.NotBefore.Format(time.RFC3339)),
		fmt.Sprintf("Not After|%s", ca.NotAfter.Format(time.RFC3339)),
	})
}

type CertificateCAImportCommand struct {
	Meta
}

func (c *CertificateCAImportCommand) Name() string { return "certificate ca import" }

func (c *CertificateCAImportCommand) Synopsis() string {
	return "Bring a CA of the organisation in the place of the issuing CA"
}

func (c *CertificateCAImportCommand) Help() string {
	helpText := `
Usage: norca certificate ca import [options]

  Puts a CA certificate and its key in the place of the issuing CA of the
  region. The certificates of the built-in issuer are issued again with it;
  the CA it replaces stays in the trust bundle until the certificates it
  signed have expired. The leader never rotates an imported CA: import its
  successor before it expires.

  The key is sent to the servers, which keep it encrypted with the keyring.

  If ACLs are enabled, this command requires a management token.

General Options:

  ` + generalOptionsUsage(usageOptsDefault|usageOptsNoNamespace) + `

Import Options:

  -cert=<file>
    The PEM encoded CA certificate. If it is not self-signed, the
    certificates above it follow it in the file, the root last.

  -key=<file>
    The PEM encoded private key of the CA (PKCS #8, PKCS #1 or SEC 1).
`
	return strings.TrimSpace(helpText)
}

func (c *CertificateCAImportCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-cert": complete.PredictFiles("*"),
			"-key":  complete.PredictFiles("*"),
		})
}

func (c *CertificateCAImportCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *CertificateCAImportCommand) Run(args []string) int {
	var certFile, keyFile string
	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.StringVar(&certFile, "cert", "", "")
	flags.StringVar(&keyFile, "key", "", "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if len(flags.Args()) != 0 {
		c.Ui.Error("This command takes no arguments")
		c.Ui.Error(commandErrorText(c))
		return 1
	}
	if certFile == "" || keyFile == "" {
		c.Ui.Error("Both -cert and -key are required")
		c.Ui.Error(commandErrorText(c))
		return 1
	}
	cert, err := os.ReadFile(certFile)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error reading the certificate: %s", err))
		return 1
	}
	key, err := os.ReadFile(keyFile)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error reading the key: %s", err))
		return 1
	}

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}
	resp, err := client.Certificates().ImportAuthority(&api.CertificateAuthorityImportRequest{
		Cert: string(cert),
		Key:  string(key),
	}, nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error importing the certificate authority: %s", err))
		return 1
	}
	c.Ui.Output("Imported the issuing certificate authority")
	c.Ui.Output(formatCertificateAuthority(resp.Authority))
	return 0
}

type CertificateCARotateCommand struct {
	Meta
}

func (c *CertificateCARotateCommand) Name() string { return "certificate ca rotate" }

func (c *CertificateCARotateCommand) Synopsis() string {
	return "Replace the issuing CA with a new one of the leader"
}

func (c *CertificateCARotateCommand) Help() string {
	helpText := `
Usage: norca certificate ca rotate [options]

  Replaces the issuing CA of the region with a new one the leader creates.
  The certificates of the built-in issuer are issued again with it; the CA
  it replaces stays in the trust bundle until the certificates it signed
  have expired. Clients have to take the new trust bundle
  ("norca certificate roots").

  If ACLs are enabled, this command requires a management token.

General Options:

  ` + generalOptionsUsage(usageOptsDefault|usageOptsNoNamespace) + `
`
	return strings.TrimSpace(helpText)
}

func (c *CertificateCARotateCommand) AutocompleteFlags() complete.Flags {
	return c.Meta.AutocompleteFlags(FlagSetClient)
}

func (c *CertificateCARotateCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *CertificateCARotateCommand) Run(args []string) int {
	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
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
	resp, err := client.Certificates().RotateAuthority(nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error rotating the certificate authority: %s", err))
		return 1
	}
	c.Ui.Output("Rotated the issuing certificate authority")
	c.Ui.Output(formatCertificateAuthority(resp.Authority))
	return 0
}
