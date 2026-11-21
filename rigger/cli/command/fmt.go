// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
)

// errUnformatted is the error of rigger fmt -check when a file is not
// formatted.
var errUnformatted = errors.New("files are not formatted")

// runFmt runs rigger fmt [-check] [path ...]: every .hcl file under the paths
// (design 10: all files are HCL, nothing is skipped), hidden directories
// left out.
func runFmt(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, args []string) int {
	flags := flag.NewFlagSet("rigger fmt", flag.ContinueOnError)
	flags.SetOutput(stderr)
	check := flags.Bool("check", false, "write nothing; list the files that are not formatted and fail if there are any")

	if err := flags.Parse(args); err != nil {
		return exitError
	}

	paths := flags.Args()
	if len(paths) == 0 {
		paths = []string{"."}
	}

	return run(ctx, logger, stderr, "fmt", func(ctx context.Context) error {
		return formatPaths(ctx, stdout, stderr, paths, *check)
	})
}

// formatter formats the .hcl files under paths, or with check lists those
// that are not formatted. A file that does not parse is reported and left
// as it is.
type formatter struct {
	stdout, stderr      io.Writer
	check               bool
	unformatted, broken int
}

func formatPaths(ctx context.Context, stdout, stderr io.Writer, paths []string, check bool) error {
	f := &formatter{stdout: stdout, stderr: stderr, check: check}

	for _, root := range paths {
		if err := filepath.WalkDir(root, f.visit(ctx, root)); err != nil {
			return err
		}
	}

	switch {
	case f.broken > 0:
		return errReported
	case check && f.unformatted > 0:
		return errUnformatted
	default:
		return nil
	}
}

// visit returns the function of the walk of root: hidden directories left
// out, .hcl files formatted.
func (f *formatter) visit(ctx context.Context, root string) fs.WalkDirFunc {
	return func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case ctx.Err() != nil:
			return ctx.Err()
		case d.IsDir() && p != root && strings.HasPrefix(d.Name(), "."):
			return fs.SkipDir
		case d.IsDir() || (p != root && !strings.HasSuffix(p, ".hcl")):
			return nil
		default:
			return f.file(p)
		}
	}
}

// file formats one file and writes its name when it was not formatted.
func (f *formatter) file(p string) error {
	changed, diags, err := formatFile(p, f.check)
	if err != nil {
		return err
	}

	if diags.HasErrors() {
		f.broken++

		return writeDiags(f.stderr, filepath.Dir(p), diags)
	}

	if changed {
		f.unformatted++
		_, err = fmt.Fprintln(f.stdout, p)
	}

	return err
}

// formatFile formats one file, and says whether its formatting changed it.
// With check it writes nothing.
func formatFile(p string, check bool) (bool, hcl.Diagnostics, error) {
	// The person running rigger names the files; they may be anywhere.
	src, err := os.ReadFile(filepath.Clean(p))
	if err != nil {
		return false, nil, err
	}

	if _, diags := hclsyntax.ParseConfig(src, filepath.Base(p), hcl.InitialPos); diags.HasErrors() {
		return false, diags, nil
	}

	out := hclwrite.Format(src)
	if bytes.Equal(out, src) {
		return false, nil, nil
	}

	if check {
		return true, nil, nil
	}

	info, err := os.Stat(p)
	if err != nil {
		return false, nil, err
	}

	// The person running rigger names the files to format.
	return true, nil, os.WriteFile(p, out, info.Mode().Perm()) //nolint:gosec // G703: the path is the user's own argument
}
