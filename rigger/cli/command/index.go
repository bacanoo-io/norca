// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/cli/internal/indexer"
	"github.com/bacanoo-io/norca/rigger/cli/norcaapi"
)

const (
	cmdIndex  = "index"
	cmdUpdate = "update"
	cmdCheck  = "check"
)

var (
	errReplace  = errors.New("--replace takes url=path")
	errProblems = errors.New("the registry has problems")
)

// newIndexCommand is rigger index update|check, the work of the workflows of
// the global registry (design 7.3) on a checkout of it.
func (a *app) newIndexCommand() *cobra.Command {
	return newGroup(&cobra.Command{
		Use:   "index",
		Short: "Keep a registry of the shape of the global registry: update, check",
		Long: `The work of the workflows of the global registry, on a checkout of a
registry of its shape: list the versions its repositories tagged, and check
its files.`,
	},
		a.newIndexSubcommand(cmdUpdate, "List the new versions of the repositories of a registry",
			`List in the entries of the registry in --dir the versions the repositories of
its namespaces have tagged, signed and checked.`,
			`  rigger index update --dir norca-global-registry
  rigger index update --namespace acme --replace https://git.example.com/acme/web.git=../web`),
		a.newIndexSubcommand(cmdCheck, "Check the files of a registry",
			`Check the files of the registry in --dir; with --base, that the change since
that git revision only adds, and the added versions against their sources.`,
			`  rigger index check --dir norca-global-registry --base origin/main`),
	)
}

// newIndexSubcommand is rigger index update or rigger index check. Both
// parse the flags they took before and show their own.
func (a *app) newIndexSubcommand(name, short, long, example string) *cobra.Command {
	var (
		dir, base, today    string
		namespaces, replace []string
	)

	cmd := &cobra.Command{
		Use: name, Short: short, Long: long, Example: example,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ix, err := newIndexer(a.logger, dir, today, replace)
			if err != nil {
				return err
			}

			if name == cmdUpdate {
				outcomes, err := ix.Update(cmd.Context(), namespaces)
				for _, o := range outcomes {
					if _, werr := fmt.Fprintln(a.stdout, o); werr != nil {
						return werr
					}
				}

				return err
			}

			problems, err := ix.Check(cmd.Context(), base)
			for _, p := range problems {
				if _, werr := fmt.Fprintln(a.stdout, p); werr != nil {
					return werr
				}
			}

			if err != nil {
				return err
			}

			if len(problems) > 0 {
				return fmt.Errorf("%w: %d", errProblems, len(problems))
			}

			_, err = fmt.Fprintf(a.stdout, "the registry in %s is valid\n", dir)

			return err
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&dir, "dir", ".", "the checkout of the registry")
	flags.StringVar(&base, "base", "", "the git `revision` the change is compared with (append-only); none checks every version")
	flags.StringVar(&today, "today", time.Now().UTC().Format(time.DateOnly), "the `day` versions are listed on")
	flags.StringArrayVar(&namespaces, namespaceFlag, nil, "a namespace to update (repeated; none: all)")
	flags.StringArrayVar(&replace, "replace", nil, "read the repository url from path, a clone (`url=path`, repeated)")

	if name == cmdUpdate {
		showOnly(cmd, "dir", "today", namespaceFlag, "replace")
	} else {
		showOnly(cmd, "dir", "base", "replace")
	}

	return cmd
}

// newIndexer is an indexer with the fetcher of the cache and an engine that
// renders the examples of modules with the decoders of Norca.
func newIndexer(logger *slog.Logger, dir, today string, replace []string) (*indexer.Indexer, error) {
	fetcher, err := newFetcher()
	if err != nil {
		return nil, err
	}

	eng, err := rigger.New(rigger.WithLogger(logger), rigger.WithJobDecoder(norcaapi.JobDecoder{}),
		rigger.WithResourceDecoder(norcaapi.ResourceDecoder{}), rigger.WithFetcher(fetcher))
	if err != nil {
		return nil, err
	}

	// The modules of the stacks of the registry come from the registry
	// itself.
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}

	fetcher.SetGlobalRegistry(abs)

	ix := &indexer.Indexer{Dir: dir, Engine: eng, Fetcher: fetcher, Today: today, Replace: map[string]string{}}

	for _, r := range replace {
		url, p, ok := strings.Cut(r, "=")
		if !ok || url == "" || p == "" {
			return nil, fmt.Errorf("%w: %q", errReplace, r)
		}

		ix.Replace[url] = p
	}

	return ix, nil
}
