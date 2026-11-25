// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/bacanoo-io/norca/rigger/builtin"
	"github.com/bacanoo-io/norca/rigger/catalog"
	"github.com/bacanoo-io/norca/rigger/fetch"
	"github.com/bacanoo-io/norca/rigger/index"
	"github.com/bacanoo-io/norca/rigger/module"
	"github.com/bacanoo-io/norca/rigger/source"
)

// rigger search lists the modules and stacks of a registry without a
// cluster: the program fetches the registry itself, as the servers sync one
// (rigger registry sync), into its cache, and searches what it read.

// The registries rigger search names by a word; anything else is the
// address of a registry repository.
const (
	searchGlobal  = "global"
	searchBuiltin = "builtin"
)

// searchFlags are the flags of rigger search.
type searchFlags struct {
	registry      string
	kind          string
	category      string
	keyword       string
	offset, limit int
	json          bool
	maxWidth      int
}

// newSearchCommand is rigger search [text...] [flags].
func (a *app) newSearchCommand() *cobra.Command {
	var f searchFlags

	cmd := &cobra.Command{
		Use:   "search [text...]",
		Short: "Search the modules and stacks of a registry, without a cluster",
		Long: `List the modules and stacks of a registry that match the text, without a
cluster: of the global registry (the default; RIGGER_GLOBAL_REGISTRY names
another), of the modules built into the program (--registry builtin), or of
a registry repository (--registry with a git address or a directory of one)
at the newest commit of its default branch, fetched into the cache.

The table shows the kind, the name, the newest version and the description,
cut to --max-width characters; --json adds the source of each match, by
which rigger show and rigger install take it.`,
		Example: `  rigger search redis
  rigger search --registry builtin
  rigger search --kind stack --category database
  rigger search --registry https://git.example.com/acme/registry.git --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return search(cmd.Context(), a.logger, a.stdout, strings.Join(args, " "), f)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&f.registry, "registry", searchGlobal,
		"what is searched: global (the global registry), builtin (the modules built into the program), or the address of a registry repository (git, or a directory of one)")
	flags.StringVar(&f.kind, "kind", "", "module or stack")
	flags.StringVar(&f.category, "category", "", "one category ("+strings.Join(module.Categories, ", ")+")")
	flags.StringVar(&f.keyword, "keyword", "", "the modules and stacks with the keyword")
	flags.IntVar(&f.offset, "offset", 0, "the first match to show")
	flags.IntVar(&f.limit, "limit", 0, fmt.Sprintf("how many matches to show (default %d, at most %d)", catalog.DefaultLimit, catalog.MaxLimit))
	flags.BoolVar(&f.json, jsonFlag, false, "write the matches as JSON")
	flags.IntVar(&f.maxWidth, maxWidthFlag, defaultMaxWidth, maxWidthUsage)

	return cmd
}

// searched is a registry as rigger search read it: its items, the address
// they were read from and its commit (empty for a directory read as it is
// and for the modules built into the program).
type searched struct {
	registry string
	commit   string
	// global reports the global registry, whose items have a namespace;
	// builtin the modules built into the program.
	global, builtin bool
	items           []catalog.Item
}

// searchResult is what rigger search -json writes: a page of the items that
// match, and how many match in all.
type searchResult struct {
	Registry string        `json:"registry"`
	Commit   string        `json:"commit,omitempty"`
	Total    int           `json:"total"`
	Offset   int           `json:"offset"`
	Items    []searchMatch `json:"items"`
}

// searchMatch is an item of a search with the address rigger show and
// rigger install take it by.
type searchMatch struct {
	catalog.Item

	Source string `json:"source"`
}

func search(ctx context.Context, logger *slog.Logger, w io.Writer, text string, f searchFlags) error {
	if f.maxWidth < 0 {
		return errMaxWidth
	}

	reg, err := readRegistry(ctx, f.registry)
	if err != nil {
		return err
	}

	logger.InfoContext(ctx, "registry read", "registry", source.Redact(reg.registry), "commit", reg.commit, "items", len(reg.items))

	page, total, err := catalog.Search(reg.items, catalog.Query{
		Kind: index.Kind(f.kind), Text: text, Category: f.category, Keyword: f.keyword, Offset: f.offset, Limit: f.limit,
	})
	if err != nil {
		return err
	}

	result := searchResult{Registry: source.Redact(reg.registry), Commit: reg.commit, Total: total, Offset: f.offset, Items: make([]searchMatch, 0, len(page))}
	for _, item := range page {
		result.Items = append(result.Items, searchMatch{Item: item, Source: reg.address(item)})
	}

	if f.json {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)

		return enc.Encode(result)
	}

	return writeFound(w, reg, result, f.maxWidth)
}

// readRegistry reads the items of the registry rigger search names: the
// global registry of the program (RIGGER_GLOBAL_REGISTRY or the default)
// as the fetcher reads it for an install, the modules built into the
// program, or a registry repository at the commit of its default branch.
// A git repository is fetched into the cache of the program now; one on
// the local file system is read in place.
func readRegistry(ctx context.Context, registry string) (*searched, error) {
	if registry == searchBuiltin {
		items, err := catalog.Builtin(builtin.FS())
		if err != nil {
			return nil, err
		}

		return &searched{registry: searchBuiltin, builtin: true, items: items}, nil
	}

	fetcher, err := newFetcher()
	if err != nil {
		return nil, err
	}

	if registry == searchGlobal || registry == "" {
		reg := &searched{registry: fetcher.GlobalRegistry(), global: true}

		// A directory is read as it is, as an install reads it.
		if fetch.GlobalDirectory(reg.registry) {
			reg.items, err = catalog.Global(os.DirFS(reg.registry))

			return reg, err
		}

		dir, err := syncRegistry(ctx, fetcher, reg)
		if err != nil {
			return nil, err
		}

		if reg.items, err = catalog.Global(os.DirFS(dir)); err != nil {
			return nil, fmt.Errorf("the global registry %s at %s: %w", source.Redact(reg.registry), reg.commit, err)
		}

		return reg, nil
	}

	reg := &searched{registry: localRepository(registry)}

	dir, err := syncRegistry(ctx, fetcher, reg)
	if err != nil {
		return nil, err
	}

	tags, err := fetcher.TagsAt(ctx, reg.registry, reg.commit, "")
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(tags))
	for _, tag := range tags {
		names = append(names, tag.Name)
	}

	if reg.items, err = catalog.Repository(os.DirFS(dir), names); err != nil {
		return nil, fmt.Errorf("%s at %s: %w", source.Redact(reg.registry), reg.commit, err)
	}

	return reg, nil
}

// syncRegistry fetches the repository of a registry now, records the
// commit of its default branch, and returns the directory of the tree of
// that commit in the cache.
func syncRegistry(ctx context.Context, fetcher *fetch.Fetcher, reg *searched) (string, error) {
	commit, err := fetcher.Sync(ctx, reg.registry)
	if err != nil {
		return "", err
	}

	reg.commit = commit

	return fetcher.Checkout(ctx, reg.registry, commit)
}

// localRepository is the address of a registry repository as the fetcher
// takes it: a directory given by a relative path is made absolute, which
// the fetcher reads in place.
func localRepository(registry string) string {
	if filepath.IsAbs(registry) || strings.Contains(registry, "://") {
		return registry
	}

	if info, err := os.Stat(registry); err == nil && info.IsDir() {
		if abs, err := filepath.Abs(registry); err == nil {
			return abs
		}
	}

	return registry
}

// address is the address an item of the registry is fetched by:
// <namespace>/<name> of the global registry, builtin/<name>, or the module
// or stack directory of a registry repository.
func (s *searched) address(item catalog.Item) string {
	switch {
	case s.global:
		return item.Namespace + "/" + item.Name
	case s.builtin:
		return "builtin/" + item.Name
	case item.Kind == index.KindStack:
		return s.registry + "//stacks/" + item.Name
	default:
		return s.registry + "//modules/" + item.Name
	}
}

// writeFound writes a page of a search as a table, and which page it is
// when the matches do not fit on one. A description longer than maxWidth
// is cut (fitText), so that a row fits a line of a terminal.
func writeFound(w io.Writer, reg *searched, result searchResult, maxWidth int) error {
	if result.Total == 0 {
		_, err := fmt.Fprintln(w, "nothing matches")

		return err
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	_, _ = fmt.Fprintln(tw, "KIND\tNAME\tLATEST\tDESCRIPTION")

	for _, m := range result.Items {
		name := m.Name
		if reg.global || reg.builtin {
			name = m.Source
		}

		latest := "-"
		if len(m.Versions) > 0 {
			latest = m.Versions[0]
		}

		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", m.Kind, name, latest, orDash(fitText(m.Description, maxWidth)))
	}

	if err := tw.Flush(); err != nil {
		return err
	}

	if result.Offset == 0 && len(result.Items) == result.Total {
		return nil
	}

	first := 0
	if len(result.Items) > 0 {
		first = result.Offset + 1
	}

	_, err := fmt.Fprintf(w, "%d-%d of %d\n", first, result.Offset+len(result.Items), result.Total)

	return err
}
