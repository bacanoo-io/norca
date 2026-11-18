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
	"os"

	"github.com/hashicorp/hcl/v2"

	rig "github.com/bacanoo-io/norca-rig"
)

// errReported is the error of a command that has written its diagnostics
// already: run exits with an error and writes nothing more.
var errReported = errors.New("reported")

// runModule runs a module command: rig module <command> [args].
func runModule(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, args []string) int {
	if len(args) == 0 {
		warn(stderr, "module needs a command: render or validate")

		return exitError
	}

	switch args[0] {
	case cmdRender:
		f, err := parseRenderFlags(stderr, args[1:])
		if err != nil {
			if !errors.Is(err, flag.ErrHelp) {
				warn(stderr, "%v", err)
			}

			return exitError
		}

		return run(ctx, logger, stderr, "module.render", func(ctx context.Context) error {
			return renderModule(ctx, logger, stdout, stderr, f)
		})
	case cmdValidate:
		dir := "."
		if len(args) > 1 {
			dir = args[1]
		}

		if len(args) > 2 {
			warn(stderr, "module validate takes one directory")

			return exitError
		}

		return run(ctx, logger, stderr, "module.validate", func(ctx context.Context) error {
			return validateModule(ctx, logger, stdout, stderr, dir)
		})
	default:
		warn(stderr, "unknown module command %q", args[0])

		return exitError
	}
}

// validateModule loads the module in dir and validates it.
func validateModule(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, dir string) error {
	eng, err := rig.New(rig.WithLogger(logger))
	if err != nil {
		return err
	}

	info, err := os.Stat(dir)
	if err != nil {
		return err
	}

	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}

	m, diags := eng.LoadModule(ctx, os.DirFS(dir))
	if !diags.HasErrors() {
		diags = append(diags, eng.ValidateModule(ctx, m)...)
	}

	if len(diags) > 0 {
		if err := writeDiags(stderr, dir, diags); err != nil {
			return err
		}
	}

	if diags.HasErrors() {
		return errReported
	}

	md := m.Metadata()
	_, err = fmt.Fprintf(stdout, "module %s %s in %s is valid\n", md.Name, md.Version, dir)

	return err
}

// writeDiags writes diagnostics for a person, with the lines of the files
// they name, which are relative to dir.
func writeDiags(w io.Writer, dir string, diags hcl.Diagnostics) error {
	if _, err := fmt.Fprintf(w, "In %s:\n", dir); err != nil {
		return err
	}

	return hcl.NewDiagnosticTextWriter(w, sources(dir, diags), 0, false).WriteDiagnostics(diags)
}

// sources reads the files diagnostics name, for the lines they show: the
// files of the module inside dir, the files of values inside the working
// directory. A file that can't be read is shown without its lines.
func sources(dir string, diags hcl.Diagnostics) map[string]*hcl.File {
	files := map[string]*hcl.File{}

	for _, d := range []string{dir, "."} {
		readSources(files, d, diags)
	}

	return files
}

// readSources reads into files the files of diags that are inside dir and
// not read yet.
func readSources(files map[string]*hcl.File, dir string, diags hcl.Diagnostics) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return
	}
	defer root.Close() //nolint:errcheck // read only: closing can lose nothing

	for _, d := range diags {
		if d.Subject == nil || files[d.Subject.Filename] != nil {
			continue
		}

		if src, err := root.ReadFile(d.Subject.Filename); err == nil {
			files[d.Subject.Filename] = &hcl.File{Bytes: src}
		}
	}
}
