// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/bacanoo-io/norca/rigger/cli/internal/convert"
)

var errConvertUsage = errors.New("usage: rigger convert pack <dir> -out <dir> [-examples <dir>]")

// newConvertCommand is rigger convert pack <dir> --out <dir> [--examples
// <dir>]: the mechanical part of a conversion (design 14).
func (a *app) newConvertCommand() *cobra.Command {
	var out, examples string

	pack := &cobra.Command{
		Use:   "pack <dir> --out <dir>",
		Short: "Turn a pack into a module, the mechanical part",
		Long: `The mechanical part of turning the pack in dir into a module: its metadata,
its variables, its examples (from the variable files of --examples, written
as examples/<name>.vars.hcl), and its templates kept as comments, to rewrite
as HCL. The notes say what is left to do.`,
		Example: `  rigger convert pack packs/redis --out modules/redis --examples packs/redis/vars`,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 || out == "" {
				return errConvertUsage
			}

			res, err := convert.Pack(args[0], examples, out)
			if err != nil {
				return err
			}

			for _, f := range res.Files {
				_, _ = fmt.Fprintf(a.stdout, "wrote %s\n", f)
			}

			for _, n := range res.Notes {
				_, _ = fmt.Fprintf(a.stdout, "to do: %s\n", n)
			}

			return nil
		},
	}

	pack.Flags().StringVar(&out, "out", "", "the `directory` of the module; must not exist or be empty")
	pack.Flags().StringVar(&examples, "examples", "", "a `directory` of variable files of the pack, written as examples/<name>.vars.hcl")

	return newGroup(&cobra.Command{
		Use:   "convert",
		Short: "Convert a pack into a module",
		Long:  `Convert what other package formats hold into Rigger modules.`,
	}, pack)
}
