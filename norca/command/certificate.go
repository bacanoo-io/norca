// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"strings"

	"github.com/mitchellh/cli"
	"github.com/posener/complete"
)

type CertificateCommand struct {
	Meta
}

func (c *CertificateCommand) Name() string { return "certificate" }

func (c *CertificateCommand) Run(args []string) int {
	return cli.RunResultHelp
}

func (c *CertificateCommand) Synopsis() string {
	return "Provides tools for the built-in certificate issuer"
}

func (c *CertificateCommand) Help() string {
	helpText := `
Usage: norca certificate <subcommand> [options]

  This command groups subcommands for the certificates the servers keep
  issued. Certificates and issuers themselves are objects of the kinds
  norca.io/Certificate and norca.io/CertificateIssuer, written with
  "norca object apply" and read with "norca object get".

  Print the certificates a client of the built-in issuer has to trust:

      $ norca certificate roots

  Revoke the current certificate of a Certificate object; a new one is
  issued:

      $ norca certificate revoke -namespace=team-a web-tls

  Bring a CA of the organisation in the place of the issuing CA:

      $ norca certificate ca import -cert=ca.pem -key=ca-key.pem

  Please see the individual subcommand help for detailed usage information.
  `
	return strings.TrimSpace(helpText)
}

type CertificateRootsCommand struct {
	Meta
}

func (c *CertificateRootsCommand) Name() string { return "certificate roots" }

func (c *CertificateRootsCommand) Synopsis() string {
	return "Print the trust bundle of the built-in certificate issuer"
}

func (c *CertificateRootsCommand) Help() string {
	helpText := `
Usage: norca certificate roots [options]

  Prints the PEM certificates a client has to trust for the certificates of
  the built-in issuer of the region: the issuing CA (or the root above an
  imported one) and, until the certificates they signed have expired, the
  retired ones.

General Options:

  ` + generalOptionsUsage(usageOptsDefault|usageOptsNoNamespace) + `

Roots Options:

  -json
    Output the issuing CAs of the region in JSON format.
`
	return strings.TrimSpace(helpText)
}

func (c *CertificateRootsCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{"-json": complete.PredictNothing})
}

func (c *CertificateRootsCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *CertificateRootsCommand) Run(args []string) int {
	var jsonOutput bool
	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.BoolVar(&jsonOutput, "json", false, "")
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
	roots, _, err := client.Certificates().Roots(nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error querying the trust bundle: %s", err))
		return 1
	}
	if jsonOutput {
		f, err := DataFormat("json", "")
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error getting formatter: %s", err))
			return 1
		}
		out, err := f.TransformData(roots)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error formatting the data: %s", err))
			return 1
		}
		c.Ui.Output(out)
		return 0
	}
	if roots.Roots == "" {
		c.Ui.Error("The region has no issuing certificate authority: it is created with the first built-in CertificateIssuer")
		return 1
	}
	c.Ui.Output(strings.TrimSpace(roots.Roots))
	return 0
}

type CertificateCRLCommand struct {
	Meta
}

func (c *CertificateCRLCommand) Name() string { return "certificate crl" }

func (c *CertificateCRLCommand) Synopsis() string {
	return "Print the revocation list of the built-in certificate issuer"
}

func (c *CertificateCRLCommand) Help() string {
	helpText := `
Usage: norca certificate crl [options]

  Prints the PEM revocation list (RFC 5280) of the issuing CA of the region,
  signed when it is asked for and valid for a day.

General Options:

  ` + generalOptionsUsage(usageOptsDefault|usageOptsNoNamespace) + `

CRL Options:

  -authority=<ID>
    The issuing CA whose list is printed: a retired one, by its ID as
    "norca certificate roots -json" shows it. The default is the active CA.
`
	return strings.TrimSpace(helpText)
}

func (c *CertificateCRLCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{"-authority": complete.PredictAnything})
}

func (c *CertificateCRLCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *CertificateCRLCommand) Run(args []string) int {
	var authority string
	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.StringVar(&authority, "authority", "", "")
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
	crl, _, err := client.Certificates().CRL(authority, nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error querying the revocation list: %s", err))
		return 1
	}
	c.Ui.Output(strings.TrimSpace(crl.CRL))
	return 0
}

type CertificateRevokeCommand struct {
	Meta
}

func (c *CertificateRevokeCommand) Name() string { return "certificate revoke" }

func (c *CertificateRevokeCommand) Synopsis() string {
	return "Revoke the current certificate of a Certificate object"
}

func (c *CertificateRevokeCommand) Help() string {
	helpText := `
Usage: norca certificate revoke [options] <name>

  Revokes the current certificate of the Certificate object of the name in
  the namespace: the built-in issuer lists it in its revocation list, an
  external issuer is asked to revoke it. A new certificate with a new key is
  issued and written to the secret.

  If ACLs are enabled, this command requires a token with the 'write'
  capability for objects of the kind norca.io/Certificate in the namespace.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `
`
	return strings.TrimSpace(helpText)
}

func (c *CertificateRevokeCommand) AutocompleteFlags() complete.Flags {
	return c.Meta.AutocompleteFlags(FlagSetClient)
}

func (c *CertificateRevokeCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *CertificateRevokeCommand) Run(args []string) int {
	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if len(flags.Args()) != 1 {
		c.Ui.Error("This command takes one argument: <name>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}
	name := flags.Args()[0]

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}
	resp, err := client.Certificates().Revoke(name, nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error revoking the certificate: %s", err))
		return 1
	}
	c.Ui.Output(fmt.Sprintf("Revoked the certificate with the serial %s of %q; a new one is being issued", resp.Serial, name))
	return 0
}
