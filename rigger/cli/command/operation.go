// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/bacanoo-io/norca/norca/api"
)

const cmdOperation = "operation"

// operationName is the one name of an operation given before or after the
// flags; empty when there is none or more than one.
func operationName(before string, after []string) string {
	switch {
	case before == "" && len(after) == 1:
		return after[0]
	case len(after) > 0:
		return ""
	default:
		return before
	}
}

// runOperation runs rigger operation follow|cancel|status <name>: an apply
// or a destroy of a stack the servers run (design/rig-integration.md 4.6c).
func runOperation(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, args []string) int {
	if len(args) == 0 {
		warn(stderr, "operation needs a command: follow, cancel or status")

		return exitError
	}

	cmd, args := args[0], args[1:]

	var name string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}

	flags := flag.NewFlagSet("rigger operation "+cmd, flag.ContinueOnError)
	flags.SetOutput(stderr)
	namespace := flags.String("namespace", "default", "the namespace of the stack of the operation")
	region := flags.String("region", "", "the region of the operation (default: the region of the cluster)")
	asJSON := flags.Bool("json", false, "status: write the operation as JSON")

	if err := flags.Parse(args); err != nil {
		return exitError
	}

	name = operationName(name, flags.Args())
	if name == "" {
		warn(stderr, "operation %s needs the name of one operation", cmd)

		return exitError
	}

	return run(ctx, logger, stderr, "operation."+cmd, func(ctx context.Context) error {
		client, err := newClient(*region)
		if err != nil {
			return err
		}

		client.SetNamespace(*namespace)

		return operationCommand(ctx, logger, stdout, stderr, client, cmd, *namespace, name, *asJSON)
	})
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
