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

	"github.com/bacanoo-io/norca/rigger/cli/internal/convert"
)

var errConvertUsage = errors.New("usage: rig convert pack <dir> -out <dir> [-examples <dir>]")

// runConvert runs rig convert pack <dir> -out <dir> [-examples <dir>]: the
// mechanical part of a conversion (design 14).
func runConvert(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, args []string) int {
	if len(args) < 2 || args[0] != "pack" {
		_, _ = fmt.Fprintln(stderr, errConvertUsage)

		return exitError
	}

	dir := args[1]

	flags := flag.NewFlagSet("rig convert pack", flag.ContinueOnError)
	flags.SetOutput(stderr)
	out := flags.String("out", "", "the directory of the module; must not exist or be empty")
	examples := flags.String("examples", "", "a directory of variable files of the pack, written as examples/<name>.vars.hcl")

	if err := flags.Parse(args[2:]); err != nil {
		return exitError
	}

	if *out == "" || flags.NArg() > 0 {
		_, _ = fmt.Fprintln(stderr, errConvertUsage)

		return exitError
	}

	return run(ctx, logger, stderr, "convert", func(context.Context) error {
		res, err := convert.Pack(dir, *examples, *out)
		if err != nil {
			return err
		}

		for _, f := range res.Files {
			_, _ = fmt.Fprintf(stdout, "wrote %s\n", f)
		}

		for _, n := range res.Notes {
			_, _ = fmt.Fprintf(stdout, "to do: %s\n", n)
		}

		return nil
	})
}
