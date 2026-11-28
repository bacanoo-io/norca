// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/bacanoo-io/norca/rigger/cli/norcaapi"
	"github.com/bacanoo-io/norca/rigger/host"
)

var errReleaseName = errors.New("the command needs the name of a release")

// cmdList is rigger release list and rigger registry list.
const cmdList = "list"

// releaseDocs are the commands of rigger release, in the order of the
// help: use, short, long and example.
var releaseDocs = [][4]string{
	{
		cmdList, "List the releases of a namespace",
		`List the current revision of each release of a namespace (--namespace "*":
of every namespace): its status, its module and version, and the stack or
package manager that owns it.`,
		`  rigger release list
  rigger release list --namespace "*"`,
	},
	{
		"status <name>", "Show the current revision of a release and its drift",
		`Show the current revision of a release and each of its resources that
drifted from what was applied.`,
		`  rigger release status web --namespace apps`,
	},
	{
		"history <name>", "List the revisions of a release",
		`List the revisions of a release, the current one among them.`,
		`  rigger release history web --namespace apps`,
	},
	{
		"rollback <name> [revision]", "Make a revision of a release current again",
		`Make a revision of a release current again: the one given, else the
previous one. --adopt restores a revision another package manager or stack
applied.`,
		`  rigger release rollback web
  rigger release rollback web 3 --namespace apps`,
	},
	{
		"test <name>", "Run the tests of a release",
		`Run the tests of the current revision of a release: the jobs of its
modules with hook { on = "test" }. The command waits for them, prints each
hook job with its status, and ends with exit code 1 when a test failed. A
release without such hooks passes. The servers keep the result as the last
test of the release. --timeout bounds the wait (by default the servers').`,
		`  rigger release test web --namespace apps
  rigger release test web --timeout 10m`,
	},
	{
		"uninstall <name>", "Uninstall a release",
		`Uninstall a release of the namespace.`,
		`  rigger release uninstall web --namespace apps`,
	},
}

// newReleaseCommand is rigger release and its commands: one release,
// without a stack, through the release API (design 10).
func (a *app) newReleaseCommand() *cobra.Command {
	sub := make([]*cobra.Command, 0, len(releaseDocs))
	for _, doc := range releaseDocs {
		sub = append(sub, a.newReleaseSubcommand(doc))
	}

	return newGroup(&cobra.Command{
		Use:   "release",
		Short: "One release, without a stack: list, status, history, rollback, test, uninstall",
		Long: `The commands of one release of a module, without a stack, through the
release API of the cluster.`,
	}, sub...)
}

// releaseOptions are the flags of rigger release that only some of its
// commands use.
type releaseOptions struct {
	// adopt is --adopt of rollback.
	adopt bool
	// timeout is --timeout of test.
	timeout time.Duration
}

// newReleaseSubcommand is one command of rigger release. Each parses the
// flags they all took before; --adopt shows for rollback only, --timeout
// for test only.
func (a *app) newReleaseSubcommand(doc [4]string) *cobra.Command {
	var (
		namespace, region string
		opts              releaseOptions
	)

	name, _, _ := strings.Cut(doc[0], " ")

	cmd := &cobra.Command{
		Use: doc[0], Short: doc[1], Long: doc[2], Example: doc[3],
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newClient(region)
			if err != nil {
				return err
			}

			return releaseCommand(cmd.Context(), a.stdout, norcaapi.NewReleases(client), name, namespace, args, opts)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&namespace, namespaceFlag, "default", "namespace of the release (list: * for all)")
	flags.StringVar(&region, regionFlag, "", "the region to talk to")
	flags.BoolVar(&opts.adopt, adoptFlag, false, "restore a revision of another manager or stack")
	flags.DurationVar(&opts.timeout, timeoutFlag, 0, "how long to wait for the tests (default: the servers')")

	switch name {
	case "rollback":
		showOnly(cmd, namespaceFlag, regionFlag, adoptFlag)
	case cmdTest:
		showOnly(cmd, namespaceFlag, regionFlag, timeoutFlag)
	default:
		showOnly(cmd, namespaceFlag, regionFlag)
	}

	return cmd
}

func releaseCommand(ctx context.Context, w io.Writer, releases *norcaapi.Releases, cmd, namespace string, args []string, opts releaseOptions) error {
	if cmd == cmdList {
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
		return releaseRollback(ctx, w, releases, ref, args[1:], opts.adopt)
	case cmdTest:
		return releaseTest(ctx, w, releases, ref, opts.timeout)
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

// errTestFailed ends rigger release test with exit code 1 when a test of
// the release failed; the hooks are printed before it.
var errTestFailed = errors.New("a test of the release failed")

// releaseTest runs the tests of the current revision of a release and
// writes the result and each hook job.
func releaseTest(ctx context.Context, w io.Writer, releases *norcaapi.Releases, ref host.ReleaseRef, timeout time.Duration) error {
	res, err := releases.Test(ctx, ref, timeout)
	if err != nil {
		return err
	}

	result := "passed"
	if !res.Succeeded {
		result = "failed"
	}

	if _, err := fmt.Fprintf(w, "test %s/%s: revision %d %s\n", ref.Namespace, ref.Name, res.Revision, result); err != nil {
		return err
	}

	for _, h := range res.Hooks {
		line := fmt.Sprintf("  %s %s/%s: %s", h.Phase, h.Namespace, h.Job, h.Status)
		if h.Message != "" {
			line += " (" + h.Message + ")"
		}

		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}

	if !res.Succeeded {
		return errTestFailed
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
