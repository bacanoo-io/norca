// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/bacanoo-io/norca/norca/api"
)

const cmdOperation = "operation"

// newOperationCommand is rigger operation follow|cancel|status <name>: an
// apply or a destroy of a stack the servers run (design/rig-integration.md
// 4.6c).
func (a *app) newOperationCommand() *cobra.Command {
	return newGroup(&cobra.Command{
		Use:   "operation",
		Short: "An apply or a destroy the servers run: follow, cancel, status",
		Long: `An apply or a destroy of a stack the servers run, as rigger stack apply
--server and rigger stack destroy --server start it.`,
	},
		a.newOperationSubcommand("follow", "Follow the log of an operation until it ends",
			`Follow the log of an operation until it ends: Ctrl-C cancels it before its
next release; a second Ctrl-C leaves it running.`,
			`  rigger operation follow platform-apply-3 --namespace data`),
		a.newOperationSubcommand("cancel", "Cancel an operation before its next release",
			`Cancel an operation: it stops before its next release.`,
			`  rigger operation cancel platform-apply-3 --namespace data`),
		a.newOperationSubcommand(cmdStatus, "Show the phase of an operation",
			`Show an operation: its action, its stack, its phase and its error.`,
			`  rigger operation status platform-apply-3 --namespace data --json`),
	)
}

// newOperationSubcommand is one command of rigger operation. Each parses
// the flags they all took before and shows its own.
func (a *app) newOperationSubcommand(name, short, long, example string) *cobra.Command {
	var (
		namespace, region string
		asJSON            bool
	)

	cmd := &cobra.Command{
		Use: name + " <name>", Short: short, Long: long, Example: example,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("operation %s needs the name of one operation", name)
			}

			client, err := newClient(region)
			if err != nil {
				return err
			}

			client.SetNamespace(namespace)

			return operationCommand(cmd.Context(), a.logger, a.stdout, a.stderr, client, name, namespace, args[0], asJSON)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&namespace, namespaceFlag, "default", "the namespace of the stack of the operation")
	flags.StringVar(&region, regionFlag, "", "the region of the operation (default: the region of the cluster)")
	flags.BoolVar(&asJSON, jsonFlag, false, "write the operation as JSON")

	if name != cmdStatus {
		showOnly(cmd, namespaceFlag, regionFlag)
	}

	return cmd
}

// operationCommand runs rigger operation <cmd> on the operation name.
func operationCommand(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, client *api.Client, cmd, namespace, name string, asJSON bool) error {
	switch cmd {
	case "follow":
		return followOperation(ctx, logger, stdout, stderr, client, namespace, name)
	case "cancel":
		op, _, err := client.Rigger().CancelOperation(name, (&api.WriteOptions{}).WithContext(ctx))
		if err != nil {
			return err
		}

		_, err = fmt.Fprintf(stdout, "the operation %s stops before its next release (%s)\n", op.Name, op.Status.Phase)

		return err
	case cmdStatus:
		op, _, err := client.Rigger().Operation(name, (&api.QueryOptions{}).WithContext(ctx))
		if err != nil {
			return err
		}

		if asJSON {
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")

			return enc.Encode(op)
		}

		_, err = fmt.Fprintf(stdout, "%s %s of the stack %s: %s %s\n", op.Name, op.Spec.Action, op.Spec.Stack, op.Status.Phase, op.Status.Error)

		return err
	default:
		return fmt.Errorf("unknown operation command %q", cmd)
	}
}
