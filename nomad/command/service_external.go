// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"
	flaghelper "github.com/hashicorp/nomad/helper/flags"
	"github.com/posener/complete"
)

// ServiceRegisterCommand registers an external service.
type ServiceRegisterCommand struct {
	Meta
}

func (c *ServiceRegisterCommand) Help() string {
	helpText := `
Usage: norca service register [options] <service_name>

  Register registers or updates an external service: a service that is not
  run by an allocation, such as a machine outside the cluster or a managed
  database. It appears in the service catalog, the built-in DNS and the
  Consul-compatible API like the services of allocations.

  An external service is kept alive by a TTL check that the registrant renews
  with "norca service ttl", or by http and tcp checks that the client agent
  the command talks to runs. When its checks are critical for longer than
  -deregister-critical-after, it is deregistered. A service without a TTL
  check that is not registered through a client agent with http or tcp checks
  is static, and requires operator write in addition.

  With -alias, the service is a DNS alias: the built-in DNS answers its name
  with a CNAME record pointing to the target.

  When ACLs are enabled, this command requires a token with the
  'register-external-service' capability for the namespace.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Register Options:

  -id <id>
    ID of the registration. Defaults to the service name.

  -address <address>
    IP address or host name of the service.

  -alias <dns name>
    Register a DNS alias of the given target name instead of an address.

  -port <port>
    Port of the service.

  -tag <tag>
    Tag of the service. May be given several times.

  -meta <key=value>
    Meta of the service. May be given several times.

  -datacenter <datacenter>
    Datacenter the service is reported in.

  -ttl <duration>
    Add a TTL check with the given TTL.

  -http <url>
    Add an http check of the URL, run by the client agent.

  -tcp <host:port>
    Add a tcp check of the address, run by the client agent.

  -node <node id>
    Client node that owns the service and runs its http and tcp checks,
    instead of the client agent the command talks to. Requires node write.

  -interval <duration>
    Interval of the http and tcp checks. Defaults to 10s.

  -timeout <duration>
    Timeout of the http and tcp checks. Defaults to 10s.

  -initial-status <status>
    Status of new checks: passing (default), warning or critical.

  -deregister-critical-after <duration>
    Deregister the service after its checks were critical this long.
    Defaults to 1h; 0 keeps the service.
`
	return strings.TrimSpace(helpText)
}

func (c *ServiceRegisterCommand) Name() string { return "service register" }

func (c *ServiceRegisterCommand) Synopsis() string { return "Register an external service" }

func (c *ServiceRegisterCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-id":                        complete.PredictAnything,
			"-address":                   complete.PredictAnything,
			"-alias":                     complete.PredictAnything,
			"-port":                      complete.PredictAnything,
			"-tag":                       complete.PredictAnything,
			"-meta":                      complete.PredictAnything,
			"-datacenter":                complete.PredictAnything,
			"-ttl":                       complete.PredictAnything,
			"-http":                      complete.PredictAnything,
			"-node":                      complete.PredictAnything,
			"-tcp":                       complete.PredictAnything,
			"-interval":                  complete.PredictAnything,
			"-timeout":                   complete.PredictAnything,
			"-initial-status":            complete.PredictSet("passing", "warning", "critical"),
			"-deregister-critical-after": complete.PredictAnything,
		})
}

func (c *ServiceRegisterCommand) Run(args []string) int {
	var id, address, alias, datacenter, httpURL, tcpAddr, nodeID, initialStatus, dca string
	var port int
	var ttl, interval, timeout time.Duration
	var tags, meta flaghelper.StringFlag

	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.StringVar(&id, "id", "", "")
	flags.StringVar(&address, "address", "", "")
	flags.StringVar(&alias, "alias", "", "")
	flags.IntVar(&port, "port", 0, "")
	flags.Var(&tags, "tag", "")
	flags.Var(&meta, "meta", "")
	flags.StringVar(&datacenter, "datacenter", "", "")
	flags.DurationVar(&ttl, "ttl", 0, "")
	flags.StringVar(&httpURL, "http", "", "")
	flags.StringVar(&tcpAddr, "tcp", "", "")
	flags.StringVar(&nodeID, "node", "", "")
	flags.DurationVar(&interval, "interval", 0, "")
	flags.DurationVar(&timeout, "timeout", 0, "")
	flags.StringVar(&initialStatus, "initial-status", "", "")
	flags.StringVar(&dca, "deregister-critical-after", "", "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	args = flags.Args()
	if len(args) != 1 {
		c.Ui.Error("This command takes one argument: <service_name>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}
	if (address == "") == (alias == "") {
		c.Ui.Error("Exactly one of -address and -alias is required")
		return 1
	}

	svc := &api.ExternalService{
		ID:         id,
		Name:       args[0],
		Address:    address,
		Port:       port,
		Tags:       tags,
		Datacenter: datacenter,
		NodeID:     nodeID,
	}
	if alias != "" {
		svc.Kind = api.ExternalServiceKindAlias
		svc.Target = alias
	}
	for _, kv := range meta {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			c.Ui.Error(fmt.Sprintf("Invalid meta %q: expected key=value", kv))
			return 1
		}
		if svc.Meta == nil {
			svc.Meta = make(map[string]string)
		}
		svc.Meta[k] = v
	}
	if dca != "" {
		d, err := time.ParseDuration(dca)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Invalid -deregister-critical-after: %v", err))
			return 1
		}
		svc.DeregisterCriticalAfter = &d
	}
	if ttl > 0 {
		svc.Checks = append(svc.Checks, &api.ExternalServiceCheck{
			Type: api.ExternalCheckTypeTTL, TTL: ttl, InitialStatus: initialStatus})
	}
	if httpURL != "" {
		svc.Checks = append(svc.Checks, &api.ExternalServiceCheck{
			Type: api.ExternalCheckTypeHTTP, HTTP: httpURL, Interval: interval, Timeout: timeout,
			InitialStatus: initialStatus})
	}
	if tcpAddr != "" {
		svc.Checks = append(svc.Checks, &api.ExternalServiceCheck{
			Type: api.ExternalCheckTypeTCP, TCP: tcpAddr, Interval: interval, Timeout: timeout,
			InitialStatus: initialStatus})
	}

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}
	regID, _, err := client.Services().RegisterExternal(svc, nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error registering external service: %s", err))
		return 1
	}
	c.Ui.Output(fmt.Sprintf("Registered external service %q with ID %q", svc.Name, regID))
	return 0
}

// ServiceDeregisterCommand deregisters an external service.
type ServiceDeregisterCommand struct {
	Meta
}

func (c *ServiceDeregisterCommand) Help() string {
	helpText := `
Usage: norca service deregister [options] <service_id>

  Deregister removes an external service registered with
  "norca service register" or through the Consul-compatible agent API.

  When ACLs are enabled, this command requires a token with the
  'register-external-service' capability for the namespace.

General Options:

  ` + generalOptionsUsage(usageOptsDefault)

	return strings.TrimSpace(helpText)
}

func (c *ServiceDeregisterCommand) Name() string { return "service deregister" }

func (c *ServiceDeregisterCommand) Synopsis() string { return "Deregister an external service" }

func (c *ServiceDeregisterCommand) AutocompleteFlags() complete.Flags {
	return c.Meta.AutocompleteFlags(FlagSetClient)
}

func (c *ServiceDeregisterCommand) Run(args []string) int {
	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	if err := flags.Parse(args); err != nil {
		return 1
	}
	args = flags.Args()
	if len(args) != 1 {
		c.Ui.Error("This command takes one argument: <service_id>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}
	if _, err := client.Services().DeregisterExternal(args[0], nil); err != nil {
		c.Ui.Error(fmt.Sprintf("Error deregistering external service: %s", err))
		return 1
	}
	c.Ui.Output(fmt.Sprintf("Deregistered external service %q", args[0]))
	return 0
}

// ServiceTTLCommand renews the TTL checks of an external service.
type ServiceTTLCommand struct {
	Meta
}

func (c *ServiceTTLCommand) Help() string {
	helpText := `
Usage: norca service ttl [options] <service_id>

  TTL renews the TTL checks of an external service, or sets their status.
  A TTL check that is not renewed within its TTL turns critical.

  When ACLs are enabled, this command requires a token with the
  'register-external-service' capability for the namespace.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

TTL Options:

  -status <status>
    Status of the checks: passing (default), warning or critical.

  -output <text>
    Output recorded with a status change.

  -check <check_id>
    Update this check only. By default every TTL check of the service.
`
	return strings.TrimSpace(helpText)
}

func (c *ServiceTTLCommand) Name() string { return "service ttl" }

func (c *ServiceTTLCommand) Synopsis() string { return "Renew the TTL checks of an external service" }

func (c *ServiceTTLCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-status": complete.PredictSet("passing", "warning", "critical"),
			"-output": complete.PredictAnything,
			"-check":  complete.PredictAnything,
		})
}

func (c *ServiceTTLCommand) Run(args []string) int {
	update := &api.ExternalCheckUpdate{}
	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.StringVar(&update.Status, "status", "passing", "")
	flags.StringVar(&update.Output, "output", "", "")
	flags.StringVar(&update.CheckID, "check", "", "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	args = flags.Args()
	if len(args) != 1 {
		c.Ui.Error("This command takes one argument: <service_id>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}
	if _, err := client.Services().UpdateExternalTTL(args[0], update, nil); err != nil {
		c.Ui.Error(fmt.Sprintf("Error updating TTL checks: %s", err))
		return 1
	}
	c.Ui.Output(fmt.Sprintf("Updated the TTL checks of external service %q: %s", args[0], update.Status))
	return 0
}
