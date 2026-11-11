// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"strings"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/api/contexts"
	flaghelper "github.com/bacanoo-io/norca/norca/helper/flags"
	"github.com/posener/complete"
)

// PluginCapacityCommand asks a controller of a CSI plugin for the capacity of
// its storage (Norca, CSI GetCapacity).
type PluginCapacityCommand struct {
	Meta
}

func (c *PluginCapacityCommand) Help() string {
	helpText := `
Usage: norca plugin capacity [options] <plugin>

  Ask a controller of a CSI plugin for the capacity of its storage: the
  bytes available for new volumes and, when the plugin reports them, the
  largest and the smallest size of a volume. Without options the answer is
  for the whole storage; with them, for volumes of these parameters,
  capability and topology (as a volume create would send them). The plugin
  needs the GET_CAPACITY controller capability.

  The latest capacity the leader asked for every capacity_interval is shown
  by "norca plugin status <plugin>".

  If ACLs are enabled, this command requires a token with the 'plugin:read'
  capability.

General Options:

  ` + generalOptionsUsage(usageOptsDefault|usageOptsNoNamespace) + `

Capacity Options:

  -parameter <key=value>
    A parameter of the plugin, as in the parameters block of a volume. May be
    given more than once.

  -topology <key=value>
    A segment of the accessible topology to ask for. May be given more than
    once. Only for a plugin with topologies.

  -access-mode <mode>
  -attachment-mode <mode>
    The capability of the volumes, e.g. single-node-writer and file-system.
    Both or neither.

  -json
    Output the capacity in JSON format.
`
	return strings.TrimSpace(helpText)
}

func (c *PluginCapacityCommand) Synopsis() string {
	return "Ask a CSI plugin for the capacity of its storage"
}

func (c *PluginCapacityCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-parameter":       complete.PredictAnything,
			"-topology":        complete.PredictAnything,
			"-access-mode":     complete.PredictSet("single-node-reader-only", "single-node-writer", "multi-node-reader-only", "multi-node-single-writer", "multi-node-multi-writer"),
			"-attachment-mode": complete.PredictSet("file-system", "block-device"),
			"-json":            complete.PredictNothing,
		})
}

func (c *PluginCapacityCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictFunc(func(a complete.Args) []string {
		client, err := c.Meta.Client()
		if err != nil {
			return nil
		}
		resp, _, err := client.Search().PrefixSearch(a.Last, contexts.Plugins, nil)
		if err != nil {
			return []string{}
		}
		return resp.Matches[contexts.Plugins]
	})
}

func (c *PluginCapacityCommand) Name() string { return "plugin capacity" }

// keyValues parses key=value flags into a map.
func keyValues(flag string, values []string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(values))
	for _, kv := range values {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("-%s %q is not key=value", flag, kv)
		}
		out[k] = v
	}
	return out, nil
}

func (c *PluginCapacityCommand) Run(args []string) int {
	var params, topology flaghelper.StringFlag
	var accessMode, attachmentMode string
	var jsonOut bool

	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.Var(&params, "parameter", "")
	flags.Var(&topology, "topology", "")
	flags.StringVar(&accessMode, "access-mode", "", "")
	flags.StringVar(&attachmentMode, "attachment-mode", "", "")
	flags.BoolVar(&jsonOut, "json", false, "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if len(flags.Args()) != 1 {
		c.Ui.Error("This command takes one argument: <plugin>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}
	if (accessMode == "") != (attachmentMode == "") {
		c.Ui.Error("-access-mode and -attachment-mode go together")
		return 1
	}

	req := &api.CSIPluginCapacityRequest{}
	var err error
	if req.Parameters, err = keyValues("parameter", params); err != nil {
		c.Ui.Error(err.Error())
		return 1
	}
	segments, err := keyValues("topology", topology)
	if err != nil {
		c.Ui.Error(err.Error())
		return 1
	}
	if len(segments) > 0 {
		req.Topology = &api.CSITopology{Segments: segments}
	}
	if accessMode != "" {
		req.Capabilities = []*api.CSIVolumeCapability{{
			AccessMode:     api.CSIVolumeAccessMode(accessMode),
			AttachmentMode: api.CSIVolumeAttachmentMode(attachmentMode),
		}}
	}

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}
	capacity, err := client.CSIPlugins().Capacity(flags.Args()[0], req, nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error asking for the capacity: %s", err))
		return 1
	}
	if jsonOut {
		out, err := Format(true, "", capacity)
		if err != nil {
			c.Ui.Error(err.Error())
			return 1
		}
		c.Ui.Output(out)
		return 0
	}
	c.Ui.Output(formatPluginCapacity([]*api.CSIPluginCapacity{capacity}))
	return 0
}
