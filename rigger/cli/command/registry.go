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
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/bacanoo-io/norca/norca/api"
)

// The registries of a cluster (design/rig-integration.md 4.5 of norca):
// objects of the kind rigger.norca.io/Registry, synced when someone asks for
// it. The UI shows a registry whole, or only what is selected for it; the
// status the UI reads changes with a sync only.

var (
	errRegistryName = errors.New("the command needs the name of a registry")
	errRegistryRef  = errors.New("select and unselect need module|stack and the module or stack: <namespace>/<name> in the global registry, <name> in a registry repository")
)

// The commands of rigger registry.
const (
	cmdRegistrySync     = "sync"
	cmdRegistrySearch   = "search"
	cmdRegistrySelect   = "select"
	cmdRegistryUnselect = "unselect"
)

// registryFlags are the flags of rigger registry.
type registryFlags struct {
	region        string
	kind          string
	offset, limit int
}

// runRegistry runs rigger registry list|status|sync|search|select|unselect.
func runRegistry(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, args []string) int {
	if len(args) == 0 {
		warn(stderr, "registry needs a command: list, status, sync, search, select or unselect")

		return exitError
	}

	cmd, args := args[0], args[1:]

	var positional []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		positional, args = append(positional, args[0]), args[1:]
	}

	var f registryFlags

	flags := flag.NewFlagSet("rigger registry "+cmd, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&f.region, "region", "", "the region to talk to")
	flags.StringVar(&f.kind, "kind", "", "search: module or stack")
	flags.IntVar(&f.offset, "offset", 0, "search: the first match to show")
	flags.IntVar(&f.limit, "limit", 0, "search: how many matches to show (default 50, at most 500)")

	if err := flags.Parse(args); err != nil {
		return exitError
	}

	positional = append(positional, flags.Args()...)

	return run(ctx, logger, stderr, "registry."+cmd, func(context.Context) error {
		client, err := newClient(f.region)
		if err != nil {
			return err
		}

		return registryCommand(stdout, client.Rigger(), cmd, positional, f)
	})
}

func registryCommand(w io.Writer, rigger *api.Rigger, cmd string, args []string, f registryFlags) error {
	if cmd == "list" {
		regs, _, err := rigger.Registries(nil)
		if err != nil {
			return err
		}

		return writeRegistries(w, regs)
	}

	if len(args) == 0 {
		return errRegistryName
	}

	name := args[0]

	switch cmd {
	case cmdStatus:
		reg, _, err := rigger.Registry(name, nil)
		if err != nil {
			return err
		}

		return writeRegistryStatus(w, reg)
	case cmdRegistrySync:
		reg, _, err := rigger.SyncRegistry(name, nil)
		if err != nil {
			return err
		}

		return writeSynced(w, reg)
	case cmdRegistrySearch:
		query := &api.RiggerRegistryQuery{Kind: f.kind, Text: strings.Join(args[1:], " "), Offset: f.offset, Limit: f.limit}

		found, _, err := rigger.SearchRegistry(name, query, nil)
		if err != nil {
			return err
		}

		return writeSearch(w, found, f.offset)
	case cmdRegistrySelect, cmdRegistryUnselect:
		return registrySelect(w, rigger, name, cmd == cmdRegistrySelect, args[1:])
	default:
		return fmt.Errorf("unknown registry command %q", cmd)
	}
}

// registrySelect adds a module or stack to what a registry shows in the UI,
// or takes it off, with a check-and-set on the registry as it was read.
func registrySelect(w io.Writer, rigger *api.Rigger, name string, add bool, args []string) error {
	if len(args) != 2 || (args[0] != "module" && args[0] != "stack") {
		return errRegistryRef
	}

	ref := api.RiggerRegistryRef{Kind: args[0], Name: args[1]}
	if ns, n, ok := strings.Cut(args[1], "/"); ok {
		ref.Namespace, ref.Name = ns, n
	}

	reg, _, err := rigger.Registry(name, nil)
	if err != nil {
		return err
	}

	selected := slices.DeleteFunc(slices.Clone(reg.Spec.Selected), func(r api.RiggerRegistryRef) bool { return r == ref })
	verb := "unselected"

	if add {
		selected, verb = append(selected, ref), "selected"
	}

	if _, err := rigger.SetSelected(reg, selected, nil); err != nil {
		return err
	}

	note := ""
	if reg.Spec.Display != "selected" {
		note = "; the registry shows everything in the UI until its display is selected"
	}

	_, err = fmt.Fprintf(w, "%s %s in %s; the UI changes with the next sync (rigger registry sync %s)%s\n", verb, ref, name, name, note)

	return err
}

// writeRegistries writes the registries as a table.
func writeRegistries(w io.Writer, regs []*api.RiggerRegistry) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	_, _ = fmt.Fprintln(tw, "NAME\tTYPE\tDISPLAY\tPHASE\tCOMMIT\tMODULES\tSTACKS\tSOURCE")

	for _, reg := range regs {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%d\t%s\n", reg.Name, reg.Spec.Type, orDash(reg.Spec.Display),
			orDash(reg.Status.Phase), orDash(shortCommit(reg.Status.Commit)), reg.Status.Modules, reg.Status.Stacks, reg.Spec.Source)
	}

	return tw.Flush()
}

// writeRegistryStatus writes a registry and what its last sync found.
func writeRegistryStatus(w io.Writer, reg *api.RiggerRegistry) error {
	var b strings.Builder

	fmt.Fprintf(&b, "registry %s (%s) %s\n", reg.Name, reg.Spec.Type, reg.Spec.Source)
	fmt.Fprintf(&b, "  display: %s, %d selected\n", orDash(reg.Spec.Display), len(reg.Spec.Selected))

	st := reg.Status
	if st.Phase == "" {
		fmt.Fprintf(&b, "  not synced: rigger registry sync %s\n", reg.Name)
	} else {
		fmt.Fprintf(&b, "  %s, last sync %s, last good sync %s\n", st.Phase, orDash(st.LastSyncedAt), orDash(st.LastGoodSyncAt))
	}

	if st.Message != "" {
		fmt.Fprintf(&b, "  %s\n", st.Message)
	}

	if st.Commit != "" {
		fmt.Fprintf(&b, "  commit %s of %s: %d modules, %d stacks\n", st.Commit, st.Source, st.Modules, st.Stacks)
	}

	writeShown(&b, st)

	_, err := io.WriteString(w, b.String())

	return err
}

// writeSynced writes what a sync found.
func writeSynced(w io.Writer, reg *api.RiggerRegistry) error {
	var b strings.Builder

	st := reg.Status
	fmt.Fprintf(&b, "synced %s: commit %s, %d modules, %d stacks\n", reg.Name, shortCommit(st.Commit), st.Modules, st.Stacks)
	writeShown(&b, st)

	_, err := io.WriteString(w, b.String())

	return err
}

// writeShown writes what a registry of the display selected shows, and the
// selected modules and stacks its index lacks.
func writeShown(b *strings.Builder, st api.RiggerRegistryStatus) {
	for _, item := range st.Shown {
		fmt.Fprintf(b, "  shown %s %s\n", item.RiggerRegistryRef, strings.Join(item.Versions, " "))
	}

	for _, ref := range st.Missing {
		fmt.Fprintf(b, "  missing %s: selected, not in the registry\n", ref)
	}
}

// writeSearch writes a page of a search.
func writeSearch(w io.Writer, found *api.RiggerRegistrySearch, offset int) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	_, _ = fmt.Fprintln(tw, "KIND\tNAME\tLATEST\tVERSIONS")

	for _, item := range found.Items {
		name := item.Name
		if item.Namespace != "" {
			name = item.Namespace + "/" + item.Name
		}

		latest := "-"
		if len(item.Versions) > 0 {
			latest = item.Versions[0]
		}

		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%d\n", item.Kind, name, latest, len(item.Versions))
	}

	if err := tw.Flush(); err != nil {
		return err
	}

	first := 0
	if len(found.Items) > 0 {
		first = offset + 1
	}

	_, err := fmt.Fprintf(w, "%d-%d of %d, at commit %s\n", first, offset+len(found.Items), found.Total, shortCommit(found.Commit))

	return err
}

// shortCommit is the first 12 characters of a commit.
func shortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}

	return commit
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}

	return s
}
