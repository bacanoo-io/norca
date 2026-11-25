// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/bacanoo-io/norca/norca/api"

	"github.com/bacanoo-io/norca/rigger/module"
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
	category      string
	keyword       string
	offset, limit int
}

// registryDoc is the help of a command of rigger registry.
type registryDoc struct {
	use, short, long, example string
}

// registryDocs are the commands of rigger registry, in the order of the
// help.
var registryDocs = []registryDoc{
	{
		use:   cmdList,
		short: "List the registries of the cluster",
		long: `List the registries of the cluster: their type, what they show in the UI,
the phase and commit of their last sync, how many modules and stacks they
hold, and their source.`,
		example: `  rigger registry list`,
	},
	{
		use:   "status <name>",
		short: "Show a registry of the cluster and its last sync",
		long: `Show a registry of the cluster: its source, what it shows in the UI, its
last sync and last good sync, and the modules and stacks it shows.`,
		example: `  rigger registry status global`,
	},
	{
		use:   "sync <name>",
		short: "Fetch the index of a registry of the cluster now",
		long: `Have the servers fetch the index of a registry now. A registry is never
synced on its own; the UI shows what the last sync read.`,
		example: `  rigger registry sync global`,
	},
	{
		use:   "search <name> [text...]",
		short: "Search a registry of the cluster at its last sync",
		long: `List the modules and stacks of a registry of the cluster that match the
text, as its last sync read them. Without a cluster: rigger search.`,
		example: `  rigger registry search global redis
  rigger registry search acme --kind stack --category database`,
	},
	{
		use:   "select <name> module|stack <ref>",
		short: "Show a module or stack of a registry in the UI",
		long: `Select a module or stack of a registry for the UI: a registry with the
display "selected" shows only what is selected. The ref is <namespace>/<name>
in the global registry, <name> in a registry repository. The UI changes with
the next sync (rigger registry sync).`,
		example: `  rigger registry select global module norca/redis`,
	},
	{
		use:   "unselect <name> module|stack <ref>",
		short: "Stop showing a module or stack of a registry in the UI",
		long: `Take a module or stack out of what a registry selects for the UI. The ref
is <namespace>/<name> in the global registry, <name> in a registry
repository. The UI changes with the next sync (rigger registry sync).`,
		example: `  rigger registry unselect global module norca/redis`,
	},
}

// newRegistryCommand is rigger registry and its commands.
func (a *app) newRegistryCommand() *cobra.Command {
	sub := make([]*cobra.Command, 0, len(registryDocs))
	for _, doc := range registryDocs {
		sub = append(sub, a.newRegistrySubcommand(doc))
	}

	return newGroup(&cobra.Command{
		Use:   "registry",
		Short: "The registries of the cluster: list, status, sync, search, select, unselect",
		Long: `The registries of the cluster (objects of the kind rigger.norca.io/Registry),
which the servers install modules from and the UI shows. A registry is
synced when someone asks for it (rigger registry sync).`,
	}, sub...)
}

// newRegistrySubcommand is one command of rigger registry. Each parses the
// flags of search, as before; the others show --region only.
func (a *app) newRegistrySubcommand(doc registryDoc) *cobra.Command {
	var f registryFlags

	name, _, _ := strings.Cut(doc.use, " ")

	cmd := &cobra.Command{
		Use: doc.use, Short: doc.short, Long: doc.long, Example: doc.example,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newClient(f.region)
			if err != nil {
				return err
			}

			return registryCommand(cmd.Context(), a.stdout, client.Rigger(), name, args, f)
		},
	}

	if name == cmdRegistrySelect || name == cmdRegistryUnselect {
		cmd.ValidArgsFunction = func(_ *cobra.Command, args []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
			if len(args) == 1 {
				return kindValues, cobra.ShellCompDirectiveNoFileComp
			}

			return nil, cobra.ShellCompDirectiveNoFileComp
		}
	}

	flags := cmd.Flags()
	flags.StringVar(&f.region, regionFlag, "", "the region to talk to")
	flags.StringVar(&f.kind, "kind", "", "module or stack")
	flags.StringVar(&f.category, "category", "", "one category ("+strings.Join(module.Categories, ", ")+")")
	flags.StringVar(&f.keyword, "keyword", "", "the modules and stacks with the keyword")
	flags.IntVar(&f.offset, "offset", 0, "the first match to show")
	flags.IntVar(&f.limit, "limit", 0, "how many matches to show (default 50, at most 500)")

	if name != cmdRegistrySearch {
		showOnly(cmd, regionFlag)
	}

	return cmd
}

// registryCommand runs a registry command. Every request ends with ctx: the
// client of the API has no time limit of its own, and a server that does
// not answer would hold the command until it is killed.
func registryCommand(ctx context.Context, w io.Writer, rigger *api.Rigger, cmd string, args []string, f registryFlags) error {
	if cmd == cmdList {
		regs, _, err := rigger.Registries(queryOptions(ctx))
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
		reg, _, err := rigger.Registry(name, queryOptions(ctx))
		if err != nil {
			return err
		}

		return writeRegistryStatus(w, reg)
	case cmdRegistrySync:
		reg, _, err := rigger.SyncRegistry(name, writeOptions(ctx))
		if err != nil {
			return err
		}

		return writeSynced(w, reg)
	case cmdRegistrySearch:
		query := &api.RiggerRegistryQuery{
			Kind: f.kind, Text: strings.Join(args[1:], " "), Category: f.category, Keyword: f.keyword,
			Offset: f.offset, Limit: f.limit,
		}

		found, _, err := rigger.SearchRegistry(name, query, queryOptions(ctx))
		if err != nil {
			return err
		}

		return writeSearch(w, found, f.offset)
	case cmdRegistrySelect, cmdRegistryUnselect:
		return registrySelect(ctx, w, rigger, name, cmd == cmdRegistrySelect, args[1:])
	default:
		return fmt.Errorf("unknown registry command %q", cmd)
	}
}

// registrySelect adds a module or stack to what a registry shows in the UI,
// or takes it off, with a check-and-set on the registry as it was read.
func registrySelect(ctx context.Context, w io.Writer, rigger *api.Rigger, name string, add bool, args []string) error {
	if len(args) != 2 || (args[0] != "module" && args[0] != "stack") {
		return errRegistryRef
	}

	ref := api.RiggerRegistryRef{Kind: args[0], Name: args[1]}
	if ns, n, ok := strings.Cut(args[1], "/"); ok {
		ref.Namespace, ref.Name = ns, n
	}

	reg, _, err := rigger.Registry(name, queryOptions(ctx))
	if err != nil {
		return err
	}

	selected := slices.DeleteFunc(slices.Clone(reg.Spec.Selected), func(r api.RiggerRegistryRef) bool { return r == ref })
	verb := "unselected"

	if add {
		selected, verb = append(selected, ref), "selected"
	}

	if _, err := rigger.SetSelected(reg, selected, writeOptions(ctx)); err != nil {
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
