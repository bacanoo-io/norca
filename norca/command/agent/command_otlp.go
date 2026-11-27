// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/bacanoo-io/norca/norca/helper/telemetry"
	"github.com/bacanoo-io/norca/norca/helper/useragent"
	"github.com/bacanoo-io/norca/norca/version"
	"github.com/hashicorp/go-hclog"
)

// setupOTLP creates the OTLP exporters of the telemetry.otlp block, before
// the agent starts: a missing CA, certificate or headers file fails the
// startup like any other configuration error. Nothing is dialled yet. It
// returns nil without an otlp block.
func (c *Command) setupOTLP(config *Config, logger hclog.Logger) (*telemetry.Providers, error) {
	if config.Telemetry == nil || config.Telemetry.OTLP == nil {
		return nil, nil
	}
	settings, err := telemetry.Resolve(config.Telemetry.OTLP)
	if err != nil {
		return nil, err
	}
	return telemetry.New(settings, logger, useragent.String())
}

// registerOTLPLogSink registers the sink of the logs signal on the agent's
// intercept logger, before the agent is created, so the lines of its setup
// are exported too. Without the logs signal (no otlp block, or
// logs.enabled = false) no sink is registered and logging costs nothing
// more.
func registerOTLPLogSink(logger hclog.InterceptLogger, otlp *telemetry.Providers) {
	if sink := otlp.LogSink(); sink != nil {
		logger.RegisterSink(sink)
	}
}

// startOTLP starts the providers once the agent runs, because the resource
// carries the client's node ID, which the client loads or creates at start.
func (c *Command) startOTLP(config *Config) error {
	if c.otlp == nil {
		return nil
	}
	return c.otlp.Start(otlpIdentity(config, c.agent))
}

// otlpIdentity describes the running agent for the resource attributes.
func otlpIdentity(config *Config, agent *Agent) telemetry.Identity {
	id := telemetry.Identity{
		Version:    version.NorcaVersion,
		Region:     config.Region,
		Datacenter: config.Datacenter,
	}
	// host.name as go-metrics names the host
	if config.Telemetry != nil && config.Telemetry.UseNodeName {
		id.HostName = config.NodeName
	} else if hostname, err := os.Hostname(); err == nil {
		id.HostName = hostname
	}
	if client := agent.Client(); client != nil {
		id.NodeID = client.NodeID()
		if node := client.Node(); node != nil {
			id.NodeName = node.Name
			id.NodePool = node.NodePool
		}
	}
	if server := agent.Server(); server != nil {
		// the Raft server ID is the server's node ID
		id.ServerID = server.GetConfig().NodeID
	}
	return id
}

// shutdownOTLP flushes and stops the OTLP export within
// telemetry.ShutdownTimeout. It runs after the agent and the HTTP servers
// are shut down; a failing flush is written to stderr only, as the logs may
// be gone.
func (c *Command) shutdownOTLP(stderr io.Writer) {
	if c.otlp == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), telemetry.ShutdownTimeout)
	defer cancel()
	if err := c.otlp.Shutdown(ctx); err != nil {
		fmt.Fprintf(stderr, "==> %v\n", err)
	}
}

// reloadOTLP applies a reloaded otlp block: the headers files are read
// again, and traces.sample_ratio and logs.level change; the rest needs a
// restart.
func (c *Command) reloadOTLP(newConf *Config) {
	var otlp *telemetry.OTLPConfig
	if newConf.Telemetry != nil {
		otlp = newConf.Telemetry.OTLP
	}
	if c.otlp == nil {
		if otlp != nil {
			c.agent.logger.Warn("adding the otlp block of telemetry needs a restart of the agent")
		}
		return
	}
	if err := c.otlp.Reload(otlp); err != nil {
		c.agent.logger.Error("failed to reload the OTLP telemetry", "error", err)
	}
}
