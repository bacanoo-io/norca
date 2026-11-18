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
	"strings"
	"time"

	rig "github.com/bacanoo-io/norca-rig"
	"github.com/bacanoo-io/norca-rig/cli/internal/indexer"
	"github.com/bacanoo-io/norca-rig/cli/norcaapi"
)

const (
	cmdIndex  = "index"
	cmdUpdate = "update"
	cmdCheck  = "check"
)

var (
	errIndexCommand = errors.New("index needs a command: update or check")
	errReplace      = errors.New("-replace takes url=path")
	errProblems     = errors.New("the registry has problems")
)

// runIndex runs rig index update|check, the work of the workflows of the
// global registry (design 7.3) on a checkout of it.
func runIndex(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, args []string) int {
	if len(args) == 0 || (args[0] != cmdUpdate && args[0] != cmdCheck) {
		warn(stderr, "%v", errIndexCommand)

		return exitError
	}

	flags := flag.NewFlagSet("rig index "+args[0], flag.ContinueOnError)
	flags.SetOutput(stderr)

	dir := flags.String("dir", ".", "the checkout of the registry")
	base := flags.String("base", "", "check: the git revision the change is compared with (append-only); none checks every version")
	today := flags.String("today", time.Now().UTC().Format(time.DateOnly), "update: the day versions are listed on")

	var namespaces, replace listFlag

	flags.Var(&namespaces, "namespace", "update: a namespace to update (repeated; none: all)")
	flags.Var(&replace, "replace", "read the repository url from path, a clone (url=path, repeated)")

	if err := flags.Parse(args[1:]); err != nil {
		return exitError
	}

	ix, err := newIndexer(logger, *dir, *today, replace)
	if err != nil {
		warn(stderr, "%v", err)

		return exitError
	}

	if args[0] == cmdUpdate {
		return run(ctx, logger, stderr, "index.update", func(ctx context.Context) error {
			outcomes, err := ix.Update(ctx, namespaces)
			for _, o := range outcomes {
				if _, werr := fmt.Fprintln(stdout, o); werr != nil {
					return werr
				}
			}

			return err
		})
	}

	return run(ctx, logger, stderr, "index.check", func(ctx context.Context) error {
		problems, err := ix.Check(ctx, *base)
		for _, p := range problems {
			if _, werr := fmt.Fprintln(stdout, p); werr != nil {
				return werr
			}
		}

		if err != nil {
			return err
		}

		if len(problems) > 0 {
			return fmt.Errorf("%w: %d", errProblems, len(problems))
		}

		_, err = fmt.Fprintf(stdout, "the registry in %s is valid\n", *dir)

		return err
	})
}

// newIndexer is an indexer with the fetcher of the cache and an engine that
// renders the examples of modules with the decoders of Norca.
func newIndexer(logger *slog.Logger, dir, today string, replace []string) (*indexer.Indexer, error) {
	fetcher, err := newFetcher()
	if err != nil {
		return nil, err
	}

	eng, err := rig.New(rig.WithLogger(logger), rig.WithJobDecoder(norcaapi.JobDecoder{}),
		rig.WithResourceDecoder(norcaapi.ResourceDecoder{}), rig.WithFetcher(fetcher))
	if err != nil {
		return nil, err
	}

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
