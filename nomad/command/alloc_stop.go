// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"errors"
	"fmt"
	"strings"

	"github.com/hashicorp/nomad/api"
)

type AllocStopCommand struct {
	Meta
}

func (c *AllocStopCommand) Help() string {
	helpText := `
Usage: norca alloc stop [options] <allocation>
Alias: norca stop

  Stop an existing allocation. This command is used to signal a specific alloc
  to shut down. When the allocation has been shut down, it will then be
  rescheduled. An interactive monitoring session will display log lines as the
  allocation completes shutting down. It is safe to exit the monitor early with
  ctrl-c.

  When ACLs are enabled, this command requires a token with the
  'alloc-lifecycle', 'read-job', and 'list-jobs' capabilities for the
  allocation's namespace.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Stop Specific Options:

  -detach
    Return immediately instead of entering monitor mode. After the
    stop command is submitted, a new evaluation ID is printed to the
    screen, which can be used to examine the rescheduling evaluation using the
    eval-status command.

  -no-shutdown-delay
    Ignore the the group and task shutdown_delay configuration so there is no
    delay between service deregistration and task shutdown. Note that using
    this flag will result in failed network connections to the allocation
    being stopped.

  -respect-disruption
    Stop the allocation through the eviction API, which only stops it if the
    disruption budget of its task group allows it. If the budget does not
    allow it, the command fails and the allocation keeps running.

  -verbose
    Show full information.
`
	return strings.TrimSpace(helpText)
}

func (c *AllocStopCommand) Name() string { return "alloc stop" }

func (c *AllocStopCommand) Run(args []string) int {
	var detach, verbose, noShutdownDelay, respectDisruption bool

	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.BoolVar(&detach, "detach", false, "")
	flags.BoolVar(&verbose, "verbose", false, "")
	flags.BoolVar(&noShutdownDelay, "no-shutdown-delay", false, "")
	flags.BoolVar(&respectDisruption, "respect-disruption", false, "")

	if err := flags.Parse(args); err != nil {
		return 1
	}

	// Check that we got exactly one alloc
	args = flags.Args()
	if len(args) != 1 {
		c.Ui.Error("This command takes one argument: <alloc-id>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}

	allocID := args[0]

	// Truncate the id unless full length is requested
	length := shortId
	if verbose {
		length = fullId
	}

	// Query the allocation info
	if len(allocID) == 1 {
		c.Ui.Error("Alloc ID must contain at least two characters.")
		return 1
	}

	allocID = sanitizeUUIDPrefix(allocID)

	// Get the HTTP client
	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}

	allocs, _, err := client.Allocations().PrefixList(allocID)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error querying allocation: %v", err))
		return 1
	}

	if len(allocs) == 0 {
		c.Ui.Error(fmt.Sprintf("No allocation(s) with prefix or id %q found", allocID))
		return 1
	}

	if len(allocs) > 1 {
		// Format the allocs
		out := formatAllocListStubs(allocs, verbose, length)
		c.Ui.Error(fmt.Sprintf("Prefix matched multiple allocations\n\n%s", out))
		return 1
	}

	// Prefix lookup matched a single allocation
	q := &api.QueryOptions{Namespace: allocs[0].Namespace}
	alloc, _, err := client.Allocations().Info(allocs[0].ID, q)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error querying allocation: %s", err))
		return 1
	}

	if respectDisruption {
		return c.evict(client, alloc, noShutdownDelay, detach, length)
	}

	var opts *api.QueryOptions
	if noShutdownDelay {
		opts = &api.QueryOptions{Params: map[string]string{"no_shutdown_delay": "true"}}
	}

	resp, err := client.Allocations().Stop(alloc, opts)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error stopping allocation: %s", err))
		return 1
	}

	if detach {
		c.Ui.Output(resp.EvalID)
		return 0
	}

	mon := newMonitor(c.Ui, client, length)
	return mon.monitor(resp.EvalID)
}

// evict stops the allocation through the eviction API.
func (c *AllocStopCommand) evict(client *api.Client, alloc *api.Allocation, noShutdownDelay, detach bool, length int) int {
	resp, err := client.Allocations().Evict(alloc.ID, noShutdownDelay, &api.WriteOptions{Namespace: alloc.Namespace})
	if api.IsDisruptionBudgetExceeded(err) {
		c.Ui.Error(fmt.Sprintf("Allocation %q was not stopped: %s", limit(alloc.ID, length), disruptionErrorBody(err)))
		return 1
	}
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error evicting allocation: %s", err))
		return 1
	}
	if resp.EvalID == "" {
		c.Ui.Output(fmt.Sprintf("Allocation %q is already stopping", limit(alloc.ID, length)))
		return 0
	}

	if detach {
		c.Ui.Output(resp.EvalID)
		return 0
	}

	mon := newMonitor(c.Ui, client, length)
	return mon.monitor(resp.EvalID)
}

// disruptionErrorBody returns the message of an eviction refused by the
// disruption budget.
func disruptionErrorBody(err error) string {
	var uErr api.UnexpectedResponseError
	if errors.As(err, &uErr) && uErr.HasBody() {
		return strings.TrimSpace(uErr.Body())
	}
	return err.Error()
}

func (c *AllocStopCommand) Synopsis() string {
	return "Stop and reschedule a running allocation"
}
