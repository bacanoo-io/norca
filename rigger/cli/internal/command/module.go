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
	"path"
	"path/filepath"

	"github.com/hashicorp/hcl/v2"

	rig "github.com/bacanoo-io/norca-rig"
	"github.com/bacanoo-io/norca-rig/cli/norcaapi"
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

// validateModule loads the module in dir and validates it; the examples are
// rendered with the job decoder of Norca.
func validateModule(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, dir string) error {
	eng, err := rig.New(rig.WithLogger(logger), rig.WithJobDecoder(norcaapi.JobDecoder{}))
	if err != nil {
		return err
	}

	m, diags, err := loadModule(ctx, eng, dir)
	if err != nil {
		return err
	}

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

// loadModule loads the module in dir, with the root it may read child
// modules from: the git repository dir is in, or dir alone outside one.
func loadModule(ctx context.Context, eng *rig.Engine, dir string) (*rig.Module, hcl.Diagnostics, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, nil, err
	}

	if !info.IsDir() {
		return nil, nil, fmt.Errorf("%s is not a directory", dir)
	}

	root, rel, err := moduleRoot(dir)
	if err != nil {
		return nil, nil, err
	}

	m, diags := eng.LoadModuleAt(ctx, os.DirFS(root), rel)

	return m, diags, nil
}

// moduleRoot returns the root of the module in dir, and the directory of
// the module in it, slash separated.
func moduleRoot(dir string) (root, rel string, err error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", "", err
	}

	for d := abs; ; {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			rel, err := filepath.Rel(d, abs)
			if err != nil {
				return "", "", err
			}

			return d, filepath.ToSlash(rel), nil
		}

		parent := filepath.Dir(d)
		if parent == d {
			return abs, ".", nil
		}

		d = parent
	}
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
// files of the module and of its children, named from dir, inside the root
// of the module; the files of values inside the working directory. A file
// that can't be read is shown without its lines.
func sources(dir string, diags hcl.Diagnostics) map[string]*hcl.File {
	files := map[string]*hcl.File{}

	if root, rel, err := moduleRoot(dir); err == nil {
		readSources(files, root, rel, diags)
	}

	readSources(files, ".", ".", diags)

	return files
}

// readSources reads into files the files of diags not read yet, named from
// the directory rel of root, if they are inside root.
func readSources(files map[string]*hcl.File, root, rel string, diags hcl.Diagnostics) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return
	}
	defer r.Close() //nolint:errcheck // read only: closing can lose nothing

	for _, d := range diags {
		if d.Subject == nil || files[d.Subject.Filename] != nil {
			continue
		}

		if src, err := r.ReadFile(path.Join(rel, d.Subject.Filename)); err == nil {
			files[d.Subject.Filename] = &hcl.File{Bytes: src}
		}
	}
}
