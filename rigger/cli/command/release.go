// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/bacanoo-io/norca/rigger/cli/norcaapi"
	"github.com/bacanoo-io/norca/rigger/host"
)

var errReleaseName = errors.New("the command needs the name of a release")

// runRelease runs rigger release list|status|history|rollback|uninstall: one
// release, without a stack, through the release API (design 10).
func runRelease(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, args []string) int {
	if len(args) == 0 {
		warn(stderr, "release needs a command: list, status, history, rollback or uninstall")

		return exitError
	}

	cmd, args := args[0], args[1:]

	var positional []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		positional, args = append(positional, args[0]), args[1:]
	}

	flags := flag.NewFlagSet("rigger release "+cmd, flag.ContinueOnError)
	flags.SetOutput(stderr)
	namespace := flags.String("namespace", "default", "namespace of the release (list: * for all)")
	region := flags.String("region", "", "the region to talk to")
	adopt := flags.Bool("adopt", false, "rollback: restore a revision of another manager or stack")

	if err := flags.Parse(args); err != nil {
		return exitError
	}

	positional = append(positional, flags.Args()...)

	return run(ctx, logger, stderr, "release."+cmd, func(ctx context.Context) error {
		client, err := newClient(*region)
		if err != nil {
			return err
		}

		return releaseCommand(ctx, stdout, norcaapi.NewReleases(client), cmd, *namespace, positional, *adopt)
	})
}

func releaseCommand(ctx context.Context, w io.Writer, releases *norcaapi.Releases, cmd, namespace string, args []string, adopt bool) error {
	if cmd == "list" {
		revs, err := releases.List(ctx, namespace)
		if err != nil {
			return err
		}

		return writeRevisions(w, revs)
	}

	if len(args) == 0 {
		return errReleaseName
	}

	ref := host.ReleaseRef{Namespace: namespace, Name: args[0]}

	switch cmd {
	case cmdStatus:
		return releaseStatus(ctx, w, releases, ref)
	case "history":
		revs, err := releases.History(ctx, ref)
		if err != nil {
			return err
		}

		return writeRevisions(w, revs)
	case "rollback":
		return releaseRollback(ctx, w, releases, ref, args[1:], adopt)
	case "uninstall":
		if err := releases.Uninstall(ctx, ref); err != nil {
			return err
		}

		_, err := fmt.Fprintf(w, "uninstalled %s/%s\n", ref.Namespace, ref.Name)

		return err
	default:
		return fmt.Errorf("unknown release command %q", cmd)
	}
}

// releaseStatus writes the current revision of a release and its drift.
func releaseStatus(ctx context.Context, w io.Writer, releases *norcaapi.Releases, ref host.ReleaseRef) error {
	rev, err := releases.Current(ctx, ref)
	if err != nil {
		return err
	}

	drift, err := releases.Drift(ctx, ref)
	if err != nil {
		return err
	}

	if err := writeRevisions(w, []host.Revision{rev}); err != nil {
		return err
	}

	for _, d := range drift.Entries {
		if _, err := fmt.Fprintf(w, "  drifted %s %s: %s\n", d.Kind, d.Name, d.Detail); err != nil {
			return err
		}
	}

	return nil
}

// releaseRollback makes a revision current again: the one given, else the
// previous one.
func releaseRollback(ctx context.Context, w io.Writer, releases *norcaapi.Releases, ref host.ReleaseRef, args []string, adopt bool) error {
	var number uint64

	if len(args) > 0 {
		n, err := strconv.ParseUint(args[0], 10, 64)
		if err != nil {
			return fmt.Errorf("%q is not a revision", args[0])
		}

		number = n
	}

	rev, err := releases.RollbackRevision(ctx, ref, number, adopt)
	if err != nil {
		return err
	}

	return writeRevisions(w, []host.Revision{rev})
}

// writeRevisions writes revisions as a table.
func writeRevisions(w io.Writer, revs []host.Revision) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAMESPACE\tRELEASE\tREVISION\tSTATUS\tMODULE\tOWNER")

	for _, r := range revs {
		owner := r.Owner
		if owner == "" {
			owner = "-"
		}

		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s %s\t%s\n", r.Release.Namespace, r.Release.Name, r.Number, r.Status,
			r.Module.Name, r.Module.Version, owner)
	}

	return tw.Flush()
}
