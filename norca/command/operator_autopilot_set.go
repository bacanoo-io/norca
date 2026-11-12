// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"strings"

	flaghelper "github.com/bacanoo-io/norca/norca/helper/flags"
	"github.com/posener/complete"
)

type OperatorAutopilotSetCommand struct {
	Meta
}

func (c *OperatorAutopilotSetCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-cleanup-dead-servers":      complete.PredictAnything,
			"-max-trailing-logs":         complete.PredictAnything,
			"-last-contact-threshold":    complete.PredictAnything,
			"-server-stabilization-time": complete.PredictAnything,
			"-enable-redundancy-zones":   complete.PredictNothing,
			"-disable-upgrade-migration": complete.PredictNothing,
			"-enable-custom-upgrades":    complete.PredictNothing,
			"-target-voters":             complete.PredictAnything,
			"-standby-failover-after":    complete.PredictAnything,
			"-standby-cooldown":          complete.PredictAnything,
		})
}

func (c *OperatorAutopilotSetCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *OperatorAutopilotSetCommand) Name() string { return "operator autopilot set-config" }

func (c *OperatorAutopilotSetCommand) Run(args []string) int {
	// Autopilot command line flags behave differently from other commands
	// in Nomad. Here, flags assume no default value. The value of the flag
	// is taken into consideration if the flag is set, whether or not it contains
	// the zero value when being applied to inherited configuration.
	//
	// This behavior was inherited from Consul.
	var cleanupDeadServers flaghelper.BoolValue
	var maxTrailingLogs flaghelper.UintValue
	var minQuorum flaghelper.UintValue
	var lastContactThreshold flaghelper.DurationValue
	var serverStabilizationTime flaghelper.DurationValue
	var enableRedundancyZones flaghelper.BoolValue
	var disableUpgradeMigration flaghelper.BoolValue
	var enableCustomUpgrades flaghelper.BoolValue
	var targetVoters flaghelper.UintValue
	var standbyFailoverAfter flaghelper.DurationValue
	var standbyCooldown flaghelper.DurationValue

	flagSet := c.Meta.FlagSet("autopilot", FlagSetClient)
	flagSet.Usage = func() { c.Ui.Output(c.Help()) }

	flagSet.Var(&cleanupDeadServers, "cleanup-dead-servers", "")
	flagSet.Var(&maxTrailingLogs, "max-trailing-logs", "")
	flagSet.Var(&lastContactThreshold, "last-contact-threshold", "")
	flagSet.Var(&serverStabilizationTime, "server-stabilization-time", "")
	flagSet.Var(&enableRedundancyZones, "enable-redundancy-zones", "")
	flagSet.Var(&disableUpgradeMigration, "disable-upgrade-migration", "")
	flagSet.Var(&enableCustomUpgrades, "enable-custom-upgrades", "")
	flagSet.Var(&minQuorum, "min-quorum", "")
	flagSet.Var(&targetVoters, "target-voters", "")
	flagSet.Var(&standbyFailoverAfter, "standby-failover-after", "")
	flagSet.Var(&standbyCooldown, "standby-cooldown", "")

	if err := flagSet.Parse(args); err != nil {
		c.Ui.Error(fmt.Sprintf("Failed to parse args: %v", err))
		return 1
	}

	// Set up a client.
	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}

	// Fetch the current configuration.
	operator := client.Operator()
	conf, _, err := operator.AutopilotGetConfiguration(nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error querying for Autopilot configuration: %s", err))
		return 1
	}

	// Update the config values based on the set flags.
	cleanupDeadServers.Merge(&conf.CleanupDeadServers)
	enableRedundancyZones.Merge(&conf.EnableRedundancyZones)
	disableUpgradeMigration.Merge(&conf.DisableUpgradeMigration)
	enableCustomUpgrades.Merge(&conf.EnableCustomUpgrades)

	trailing := uint(conf.MaxTrailingLogs)
	maxTrailingLogs.Merge(&trailing)
	conf.MaxTrailingLogs = uint64(trailing)

	minQuorum.Merge(&conf.MinQuorum)

	lastContactThreshold.Merge(&conf.LastContactThreshold)

	serverStabilizationTime.Merge(&conf.ServerStabilizationTime)

	targetVoters.Merge(&conf.TargetVoters)
	standbyFailoverAfter.Merge(&conf.StandbyFailoverAfter)
	standbyCooldown.Merge(&conf.StandbyCooldown)

	// Check-and-set the new configuration.
	result, _, err := operator.AutopilotCASConfiguration(conf, nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error setting Autopilot configuration: %s", err))
		return 1
	}
	if result {
		c.Ui.Output("Configuration updated!")
		return 0
	}
	c.Ui.Output("Configuration could not be atomically updated, please try again")
	return 1
}

func (c *OperatorAutopilotSetCommand) Synopsis() string {
	return "Modify the current Autopilot configuration"
}

func (c *OperatorAutopilotSetCommand) Help() string {
	helpText := `
Usage: norca operator autopilot set-config [options]

  Modifies the current Autopilot configuration.

  If ACLs are enabled, this command requires a token with the 'operator:write'
  capability.

General Options:

  ` + generalOptionsUsage(usageOptsDefault|usageOptsNoNamespace) + `

Set Config Options:

  -cleanup-dead-servers=[true|false]
     Controls whether Norca will automatically remove dead servers when
     new ones are successfully added. Must be one of [true|false].

  -disable-upgrade-migration=[true|false]
     Not supported: the servers reject the value true. Must be one of
     "true|false".

  -last-contact-threshold=200ms
     Controls the maximum amount of time a server can go without contact
     from the leader before being considered unhealthy. Must be a
     duration value such as "200ms".

  -max-trailing-logs=<value>
     Controls the maximum number of log entries that a server can trail
     the leader by before being considered unhealthy.

  -min-quorum=<value>
      Controls the minimum number of servers required in a cluster
      before autopilot can prune dead servers.

  -redundancy-zone-tag=<value>
     Not supported: redundancy zones are not available, the value has no
     effect.

  -server-stabilization-time=<10s>
     Controls the minimum amount of time a server must be stable in
     the 'healthy' state before being added to the cluster. Only takes
     effect if all servers are running Raft protocol version 3 or
     higher. Must be a duration value such as "10s".

  -standby-cooldown=<5m>
     The minimum time between two changes of the voter set made for
     standby servers. 0 is the default, 5m.

  -standby-failover-after=<1m>
     How long a voter must be unhealthy before a standby server replaces
     it. With target-voters set, the dead server cleanup leaves a failed
     voter in the Raft configuration for the same time. 0 is the default,
     1m.

  -target-voters=<value>
     The number of voters autopilot keeps with standby servers: 0, 1, 3,
     5 or 7. 0 (the default): every server with the role "voter" votes
     and standby servers are never promoted.

  -upgrade-version-tag=<value>
     Not supported: custom upgrade migrations are not available, the value
     has no effect.
`
	return strings.TrimSpace(helpText)
}
